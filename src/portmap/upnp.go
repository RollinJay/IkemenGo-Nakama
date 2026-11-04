package portmap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// UPnP: the router answers an SSDP search with the URL of its description,
// which names the control URLs of its WANIPConnection and WANPPPConnection
// services; mappings are SOAP calls to those URLs.
//
// Any device on the network can answer a search, so an answer counts only
// when it comes from a router (Options.Routers) and names a description on
// the device that sent it, and only control URLs on that device are used:
// whatever answers, the game talks to nothing else.
const (
	upnpSearchTime = 2 * time.Second
	upnpHTTPTime   = 3 * time.Second
	// UPnP error codes (WANIPConnection:2, section 2.5).
	upnpNoSuchEntry   = 714 // no mapping of that external port
	upnpConflict      = 718 // the external port is mapped to another client
	upnpOnlyPermanent = 725 // the router takes only leases of 0 (permanent)
)

var upnpSearchTargets = []string{
	"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
	"urn:schemas-upnp-org:service:WANIPConnection:1",
	"urn:schemas-upnp-org:service:WANIPConnection:2",
	"urn:schemas-upnp-org:service:WANPPPConnection:1",
}

// ssdpAnswer is the description URL a device answered a search with.
type ssdpAnswer struct {
	location string
	from     netip.Addr
}

// mapUPnP asks each router that answered the search (discoverUPnP) for a
// mapping, through each of its connection services, until one maps the port.
func mapUPnP(ctx context.Context, answers <-chan ssdpAnswer, port int, opt Options) (*Mapping, error) {
	seen := map[string]bool{}
	var errs []error
	for answer := range answers {
		if seen[answer.location] {
			continue
		}
		seen[answer.location] = true
		services, err := fetchUPnPServices(ctx, answer)
		if err != nil {
			opt.Logf("portmap: UPnP %s: %v", answer.location, err)
			errs = append(errs, err)
			continue
		}
		for _, svc := range services {
			m, err := svc.addMapping(ctx, port, opt)
			if err == nil {
				return m, nil
			}
			opt.Logf("portmap: UPnP %s: %v", svc.controlURL, err)
			errs = append(errs, err)
		}
	}
	if ctx.Err() != nil {
		errs = append(errs, ctx.Err())
	}
	if len(errs) == 0 {
		return nil, errors.New("upnp: no router answered the search")
	}
	return nil, fmt.Errorf("upnp: %w", errors.Join(errs...))
}

// discoverUPnP sends SSDP searches (twice, as UDP gets lost) and reports each
// answer that counts (ssdpAnswerCounts), until upnpSearchTime passes or ctx
// ends.
func discoverUPnP(ctx context.Context, opt Options, answers chan<- ssdpAnswer) {
	defer close(answers)
	dst, err := net.ResolveUDPAddr("udp4", opt.SSDPAddr)
	if err != nil {
		return
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return
	}
	defer conn.Close()
	search := func() {
		for _, st := range upnpSearchTargets {
			msg := "M-SEARCH * HTTP/1.1\r\n" +
				"HOST: 239.255.255.250:1900\r\n" +
				"MAN: \"ssdp:discover\"\r\n" +
				"MX: 1\r\n" +
				"ST: " + st + "\r\n\r\n"
			_, _ = conn.WriteToUDP([]byte(msg), dst)
		}
	}
	search()
	deadline := time.Now().Add(upnpSearchTime)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	resent := false
	buf := make([]byte, 2048)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		wait := time.Now().Add(250 * time.Millisecond)
		if wait.After(deadline) {
			wait = deadline
		}
		_ = conn.SetReadDeadline(wait)
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if !resent && time.Until(deadline) < upnpSearchTime/2 {
				search()
				resent = true
			}
			continue
		}
		answer := ssdpAnswer{location: ssdpLocation(buf[:n]), from: from.AddrPort().Addr().Unmap()}
		if !ssdpAnswerCounts(answer, opt.Routers) {
			if answer.location != "" {
				opt.Logf("portmap: UPnP: ignoring %s from %s, which is not a router this machine routes through", answer.location, answer.from)
			}
			continue
		}
		select {
		case answers <- answer:
		case <-ctx.Done():
			return
		}
	}
}

// ssdpAnswerCounts reports whether an answer comes from one of routers (any
// device, when routers is empty) and names a description on that device, by
// its address.
func ssdpAnswerCounts(a ssdpAnswer, routers []netip.Addr) bool {
	if a.location == "" || (len(routers) > 0 && !slices.Contains(routers, a.from)) {
		return false
	}
	u, err := url.Parse(a.location)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host, err := netip.ParseAddr(u.Hostname())
	return err == nil && host.Unmap() == a.from
}

// ssdpLocation returns the LOCATION header of an SSDP answer.
func ssdpLocation(b []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "location") {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

type upnpDevice struct {
	Services []struct {
		ServiceType string `xml:"serviceType"`
		ControlURL  string `xml:"controlURL"`
	} `xml:"serviceList>service"`
	Devices []upnpDevice `xml:"deviceList>device"`
}

// upnpService is a router's WANIPConnection or WANPPPConnection service.
type upnpService struct {
	serviceType string
	controlURL  string
	client      net.IP // this machine's address on the router's network
	http        *http.Client
}

// fetchUPnPServices reads the description an answer names and returns its
// connection services whose control URLs are on the device that answered,
// WANIPConnection ones first. A router may have several, of which only the
// connected one has a public address.
func fetchUPnPServices(ctx context.Context, answer ssdpAnswer) ([]*upnpService, error) {
	base, err := url.Parse(answer.location)
	if err != nil {
		return nil, fmt.Errorf("bad description URL %q", answer.location)
	}
	// No redirects: they could lead the requests anywhere but the device.
	client := &http.Client{Timeout: upnpHTTPTime, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, answer.location, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("description: HTTP %d", resp.StatusCode)
	}
	var root struct {
		URLBase string     `xml:"URLBase"`
		Device  upnpDevice `xml:"device"`
	}
	if err := xmlDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&root); err != nil {
		return nil, fmt.Errorf("description: %w", err)
	}
	if root.URLBase != "" {
		if b, err := url.Parse(strings.TrimSpace(root.URLBase)); err == nil {
			base = b
		}
	}
	// This machine's address as the router sees it: the address a socket
	// connected to the router uses.
	conn, err := net.Dial("udp4", net.JoinHostPort(answer.from.String(), "9"))
	if err != nil {
		return nil, err
	}
	local := conn.LocalAddr().(*net.UDPAddr).IP
	conn.Close()
	var services []*upnpService
	for _, s := range findWANServices(&root.Device) {
		ref, err := url.Parse(strings.TrimSpace(s[1]))
		if err != nil {
			continue
		}
		control := base.ResolveReference(ref)
		host, err := netip.ParseAddr(control.Hostname())
		if err != nil || host.Unmap() != answer.from || (control.Scheme != "http" && control.Scheme != "https") {
			continue
		}
		services = append(services, &upnpService{serviceType: s[0], controlURL: control.String(), client: local, http: client})
	}
	if len(services) == 0 {
		return nil, errors.New("not an internet gateway (no WANIPConnection or WANPPPConnection on the device)")
	}
	return services, nil
}

// upnpServiceType matches the connection services' types, which the SOAP
// calls write into their XML as they are.
var upnpServiceType = regexp.MustCompile(`^urn:schemas-upnp-org:service:(WANIPConnection|WANPPPConnection):[0-9]{1,3}$`)

// findWANServices lists the device tree's connection services as service
// type and control URL, WANIPConnection ones before WANPPPConnection ones.
func findWANServices(d *upnpDevice) [][2]string {
	var ip, ppp [][2]string
	var walk func(d *upnpDevice)
	walk = func(d *upnpDevice) {
		for _, s := range d.Services {
			st := strings.TrimSpace(s.ServiceType)
			m := upnpServiceType.FindStringSubmatch(st)
			switch {
			case m == nil:
			case m[1] == "WANIPConnection":
				ip = append(ip, [2]string{st, s.ControlURL})
			default:
				ppp = append(ppp, [2]string{st, s.ControlURL})
			}
		}
		for i := range d.Devices {
			walk(&d.Devices[i])
		}
	}
	walk(d)
	return append(ip, ppp...)
}

// xmlDecoder reads XML in UTF-8, or in the 8-bit charsets routers declare,
// whose bytes become the runes of the same number (ISO-8859-1 exactly, and
// Windows-1252 but for a few symbols); what the game reads is ASCII.
func xmlDecoder(r io.Reader) *xml.Decoder {
	dec := xml.NewDecoder(r)
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		switch strings.ToLower(strings.TrimSpace(charset)) {
		case "us-ascii", "ascii":
			return input, nil
		case "iso-8859-1", "iso8859-1", "latin1", "latin-1", "windows-1252", "cp1252":
			b, err := io.ReadAll(input)
			if err != nil {
				return nil, err
			}
			runes := make([]rune, len(b))
			for i, c := range b {
				runes[i] = rune(c)
			}
			return strings.NewReader(string(runes)), nil
		}
		return nil, fmt.Errorf("unsupported charset %q", charset)
	}
	return dec
}

// upnpError is a SOAP fault carrying a UPnP error code.
type upnpError struct {
	Code        int
	Description string
}

func (e *upnpError) Error() string { return fmt.Sprintf("UPnP error %d (%s)", e.Code, e.Description) }

func isUPnPError(err error, code int) bool {
	var ue *upnpError
	return errors.As(err, &ue) && ue.Code == code
}

// call makes one SOAP call and returns the leaf elements of the answer.
func (s *upnpService) call(ctx context.Context, action string, args [][2]string) (map[string]string, error) {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body><u:` + action + ` xmlns:u="` + s.serviceType + `">`)
	for _, a := range args {
		body.WriteString("<" + a[0] + ">" + html.EscapeString(a[1]) + "</" + a[0] + ">")
	}
	body.WriteString(`</u:` + action + `></s:Body></s:Envelope>`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.controlURL, strings.NewReader(body.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+s.serviceType+"#"+action+`"`)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	fields := xmlLeaves(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		if code, err := strconv.Atoi(fields["errorCode"]); err == nil {
			return nil, &upnpError{Code: code, Description: fields["errorDescription"]}
		}
		return nil, fmt.Errorf("%s: HTTP %d", action, resp.StatusCode)
	}
	return fields, nil
}

// xmlLeaves returns the text of each element without child elements, by
// local name; enough for SOAP answers and faults.
func xmlLeaves(r io.Reader) map[string]string {
	out := map[string]string{}
	dec := xmlDecoder(r)
	var name string
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name = t.Name.Local
			text.Reset()
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			if name == t.Name.Local {
				out[name] = strings.TrimSpace(text.String())
			}
			name = ""
		}
	}
}

func (s *upnpService) add(ctx context.Context, internalPort, externalPort int, lease int, desc string) error {
	_, err := s.call(ctx, "AddPortMapping", [][2]string{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(externalPort)},
		{"NewProtocol", "UDP"},
		{"NewInternalPort", strconv.Itoa(internalPort)},
		{"NewInternalClient", s.client.String()},
		{"NewEnabled", "1"},
		{"NewPortMappingDescription", desc},
		{"NewLeaseDuration", strconv.Itoa(lease)},
	})
	return err
}

// removeIfOurs removes the mapping of external when it forwards to this
// machine's internalPort. By the time it runs, the external port may be
// another machine's mapping (the add was refused, or the lease ran out),
// which must stay.
func (s *upnpService) removeIfOurs(ctx context.Context, internalPort, external int) error {
	key := [][2]string{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(external)},
		{"NewProtocol", "UDP"},
	}
	entry, err := s.call(ctx, "GetSpecificPortMappingEntry", key)
	if isUPnPError(err, upnpNoSuchEntry) {
		return nil
	}
	if err != nil {
		return err
	}
	if !net.ParseIP(entry["NewInternalClient"]).Equal(s.client) || entry["NewInternalPort"] != strconv.Itoa(internalPort) {
		return nil
	}
	_, err = s.call(ctx, "DeletePortMapping", key)
	return err
}

func (s *upnpService) addMapping(ctx context.Context, port int, opt Options) (*Mapping, error) {
	// The public address first: a connection service that is not connected
	// has none, and a router behind another NAT would map in vain.
	fields, err := s.call(ctx, "GetExternalIPAddress", nil)
	if err != nil {
		return nil, err
	}
	extIP := net.ParseIP(fields["NewExternalIPAddress"]).To4()
	if extIP == nil {
		return nil, errors.New("the router reported no external address")
	}
	if !publicIPv4(extIP) {
		return nil, fmt.Errorf("the router's public address %s is behind another NAT", extIP)
	}
	lease := int(opt.Lifetime / time.Second)
	// The same port outside as inside first; if another machine has it,
	// a few others at random.
	externals := []int{port}
	for i := 0; i < 3; i++ {
		externals = append(externals, 20000+rand.IntN(40000))
	}
	var external int
	for _, external = range externals {
		err = s.add(ctx, port, external, lease, opt.Description)
		if !isUPnPError(err, upnpConflict) {
			break
		}
	}
	if err != nil {
		var ue *upnpError
		if !errors.As(err, &ue) {
			// The request may have reached the router although its answer
			// did not reach the game: remove what it may have mapped.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
				defer cancel()
				_ = s.removeIfOurs(ctx, port, external)
			}()
		}
		if isUPnPError(err, upnpOnlyPermanent) {
			// A permanent mapping would outlive a game that crashed or lost
			// its network: the router would keep the port open for good.
			return nil, errors.New("the router takes only permanent mappings, which the game does not make")
		}
		return nil, err
	}
	opt.Logf("portmap: UPnP: %s maps %s:%d to %s:%d", s.controlURL, extIP, external, s.client, port)
	return &Mapping{
		Method: "upnp", ExternalIP: extIP, ExternalPort: external, InternalPort: port,
		Lifetime: time.Duration(lease) * time.Second,
		renew: func(ctx context.Context) (time.Duration, error) {
			return time.Duration(lease) * time.Second, s.add(ctx, port, external, lease, opt.Description)
		},
		release: func(ctx context.Context) error { return s.removeIfOurs(ctx, port, external) },
	}, nil
}
