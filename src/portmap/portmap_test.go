package portmap

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakePCP answers PCP MAP requests (or, natpmpOnly, answers them with
// NAT-PMP's "unsupported version" and serves NAT-PMP instead) on localhost.
// As a PCP server does, it refuses a request for a mapping another nonce
// holds. With delay, it maps at once but answers map requests late.
type fakePCP struct {
	conn       *net.UDPConn
	natpmpOnly bool
	external   net.IP
	delay      time.Duration

	mu       sync.Mutex
	mapped   []uint32          // lifetimes granted, in order (0 is a removal)
	nonces   map[uint16]string // PCP: the nonce holding each internal port
	requests int
}

func newFakePCP(t *testing.T, natpmpOnly bool, external string) *fakePCP {
	return newFakePCPDelayed(t, natpmpOnly, external, 0)
}

func newFakePCPDelayed(t *testing.T, natpmpOnly bool, external string, delay time.Duration) *fakePCP {
	t.Helper()
	return newFakePCPAt(t, "127.0.0.1", 0, natpmpOnly, external, delay)
}

// newFakePCPAt makes a fakePCP on another address of the loopback network,
// where a second router of the same port can listen; the test is skipped
// where the system has no such address.
func newFakePCPAt(t *testing.T, ip string, port int, natpmpOnly bool, external string, delay time.Duration) *fakePCP {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip), Port: port})
	if err != nil {
		if ip != "127.0.0.1" {
			t.Skipf("no router can listen on %s here: %v", ip, err)
		}
		t.Fatal(err)
	}
	f := &fakePCP{conn: conn, natpmpOnly: natpmpOnly, external: net.ParseIP(external).To4(), delay: delay, nonces: map[uint16]string{}}
	t.Cleanup(func() { conn.Close() })
	go f.serve()
	return f
}

func (f *fakePCP) port() int { return f.conn.LocalAddr().(*net.UDPAddr).Port }

func (f *fakePCP) serve() {
	buf := make([]byte, 1100)
	for {
		n, from, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		req := buf[:n]
		f.mu.Lock()
		f.requests++
		f.mu.Unlock()
		var resp []byte
		switch {
		case len(req) >= 60 && req[0] == pcpVersion:
			if f.natpmpOnly {
				// A NAT-PMP server answers a PCP request in its own format.
				f.conn.WriteToUDP([]byte{0, 0x80 | req[1], 0, 1, 0, 0, 0, 0}, from)
				continue
			}
			lifetime := binary.BigEndian.Uint32(req[4:8])
			internal := binary.BigEndian.Uint16(req[40:42])
			resp = make([]byte, 60)
			resp[0], resp[1], resp[3] = pcpVersion, 0x80|pcpOpMap, 0
			f.mu.Lock()
			holder, held := f.nonces[internal]
			switch {
			case held && holder != string(req[24:36]):
				resp[3] = 2 // NOT_AUTHORIZED
			case lifetime == 0:
				delete(f.nonces, internal)
			default:
				f.nonces[internal] = string(req[24:36])
			}
			f.mu.Unlock()
			if resp[3] == 0 {
				f.record(lifetime)
			}
			binary.BigEndian.PutUint32(resp[4:8], lifetime)
			copy(resp[24:36], req[24:36]) // nonce
			resp[36] = pcpProtoUDP
			copy(resp[40:42], req[40:42])
			ext := binary.BigEndian.Uint16(req[42:44])
			if ext == 0 {
				ext = binary.BigEndian.Uint16(req[40:42])
			}
			binary.BigEndian.PutUint16(resp[42:44], ext)
			copy(resp[44:60], f.external.To16())
		case len(req) == 2 && req[0] == natpmpVersion && req[1] == natpmpOpAddr && f.natpmpOnly:
			resp = make([]byte, 12)
			resp[1] = 128 + natpmpOpAddr
			copy(resp[8:12], f.external)
			f.conn.WriteToUDP(resp, from)
			continue
		case len(req) == 12 && req[0] == natpmpVersion && req[1] == natpmpOpMapUDP && f.natpmpOnly:
			lifetime := binary.BigEndian.Uint32(req[8:12])
			f.record(lifetime)
			resp = make([]byte, 16)
			resp[1] = 128 + natpmpOpMapUDP
			copy(resp[8:10], req[4:6])
			ext := req[6:8]
			if binary.BigEndian.Uint16(ext) == 0 {
				ext = req[4:6]
			}
			copy(resp[10:12], ext)
			binary.BigEndian.PutUint32(resp[12:16], lifetime)
		default:
			continue
		}
		if f.delay > 0 {
			time.AfterFunc(f.delay, func() { f.conn.WriteToUDP(resp, from) })
			continue
		}
		f.conn.WriteToUDP(resp, from)
	}
}

func (f *fakePCP) record(lifetime uint32) {
	f.mu.Lock()
	f.mapped = append(f.mapped, lifetime)
	f.mu.Unlock()
}

func (f *fakePCP) lifetimes() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint32(nil), f.mapped...)
}

// waitFor polls cond for a little while: for what happens in the background.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// noUPnP is an SSDP address where nothing answers.
func noUPnP(t *testing.T) string {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	addr := c.LocalAddr().String()
	c.Close()
	return addr
}

func localhost() []netip.Addr { return []netip.Addr{netip.MustParseAddr("127.0.0.1")} }

func TestMapPCP(t *testing.T) {
	f := newFakePCP(t, false, "203.0.113.7")
	m, err := Map(context.Background(), 40123, Options{Gateways: localhost(), PCPPort: f.port(), SSDPAddr: noUPnP(t), Lifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if m.Method != "pcp" || !m.ExternalIP.Equal(net.ParseIP("203.0.113.7")) || m.ExternalPort != 40123 || m.InternalPort != 40123 {
		t.Fatalf("mapping = %s %s:%d -> %d", m.Method, m.ExternalIP, m.ExternalPort, m.InternalPort)
	}
	if m.Lifetime != time.Hour {
		t.Fatalf("lifetime = %v", m.Lifetime)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if got := f.lifetimes(); len(got) != 2 || got[0] != 3600 || got[1] != 0 {
		t.Fatalf("requests' lifetimes = %v, want a mapping then its removal", got)
	}
}

func TestMapNATPMPWhenTheRouterSpeaksOnlyThat(t *testing.T) {
	f := newFakePCP(t, true, "203.0.113.8")
	m, err := Map(context.Background(), 40124, Options{Gateways: localhost(), PCPPort: f.port(), SSDPAddr: noUPnP(t), Lifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.Method != "natpmp" || !m.ExternalIP.Equal(net.ParseIP("203.0.113.8")) || m.ExternalPort != 40124 {
		t.Fatalf("mapping = %s %s:%d", m.Method, m.ExternalIP, m.ExternalPort)
	}
	m.Close()
	if got := f.lifetimes(); len(got) != 2 || got[1] != 0 {
		t.Fatalf("requests' lifetimes = %v, want a mapping then its removal", got)
	}
}

func TestMapRefusesARouterBehindAnotherNAT(t *testing.T) {
	// The router's own "public" address is in the carrier-grade NAT range.
	f := newFakePCP(t, false, "100.64.12.34")
	_, err := Map(context.Background(), 40125, Options{Gateways: localhost(), PCPPort: f.port(), SSDPAddr: noUPnP(t)})
	if !errors.Is(err, ErrNoRouter) || !strings.Contains(err.Error(), "behind another NAT") {
		t.Fatalf("err = %v", err)
	}
	if got := f.lifetimes(); len(got) != 2 || got[1] != 0 {
		t.Fatalf("requests' lifetimes = %v, want the useless mapping removed", got)
	}
}

func TestMapWithoutARouter(t *testing.T) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Map(ctx, 40126, Options{Gateways: localhost(), PCPPort: closedUDPPort(t), SSDPAddr: noUPnP(t)})
	if !errors.Is(err, ErrNoRouter) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("took %v", d)
	}
}

// A NAT-PMP router that answers on two of the addresses asked grants the
// same mapping to both, since NAT-PMP names it by this machine's address and
// port. The first answer wins; the request to the other address, cut off,
// is not undone, since that would remove the mapping granted.
func TestMapNATPMPSameRouterTwice(t *testing.T) {
	first := newFakePCPDelayed(t, true, "203.0.113.11", 100*time.Millisecond)
	second := newFakePCPAt(t, "127.0.0.2", first.port(), true, "203.0.113.11", 2*time.Second)
	gws := []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")}
	m, err := Map(context.Background(), 40129, Options{Gateways: gws, PCPPort: first.port(), SSDPAddr: noUPnP(t), Lifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if got := second.lifetimes(); len(got) != 1 || got[0] != 3600 {
		t.Fatalf("the other address's requests = %v; want its map request, then nothing", got)
	}
	time.Sleep(500 * time.Millisecond) // past the undo of a request cut off
	if got := second.lifetimes(); slices.Contains(got, 0) {
		t.Fatalf("the other address's requests = %v: the mapping granted was removed", got)
	}
}

// Of two routers, the one whose own public address is shared (another NAT
// in front of it) answers first: its mapping is removed and does not count,
// and the public router's, which answers later, is kept.
func TestMapPCPSkipsARouterBehindAnotherNAT(t *testing.T) {
	shared := newFakePCP(t, false, "100.64.1.2")
	// After the first read window (250 ms): a request the shared router's
	// answer cut off would get no answer.
	public := newFakePCPAt(t, "127.0.0.2", shared.port(), false, "203.0.113.50", 300*time.Millisecond)
	gws := []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")}
	m, err := Map(context.Background(), 40141, Options{Gateways: gws, PCPPort: shared.port(), SSDPAddr: noUPnP(t), Lifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if !m.ExternalIP.Equal(net.ParseIP("203.0.113.50")) {
		t.Fatalf("mapping on %s, want the public router's", m.ExternalIP)
	}
	if got := public.lifetimes(); len(got) == 0 || slices.Contains(got, 0) {
		t.Fatalf("the public router's lifetimes = %v, want its mapping kept", got)
	}
	if got := shared.lifetimes(); len(got) != 2 || got[1] != 0 {
		t.Fatalf("the other router's lifetimes = %v, want its mapping removed", got)
	}
}

// A PCP router refuses the second address's request, whose nonce differs;
// nothing removes the mapping granted.
func TestMapPCPSameRouterTwice(t *testing.T) {
	f := newFakePCP(t, false, "203.0.113.14")
	gw := netip.MustParseAddr("127.0.0.1")
	m, err := Map(context.Background(), 40140, Options{Gateways: []netip.Addr{gw, gw}, PCPPort: f.port(), SSDPAddr: noUPnP(t), Lifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	time.Sleep(300 * time.Millisecond)
	if got := f.lifetimes(); len(got) != 1 || got[0] != 3600 {
		t.Fatalf("lifetimes granted = %v, want the one mapping", got)
	}
}

// A request given up on before its answer came may have been granted: the
// router is asked to remove the mapping.
func TestMapPCPRemovesAMappingAnsweredTooLate(t *testing.T) {
	f := newFakePCPDelayed(t, false, "203.0.113.12", 600*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := Map(ctx, 40130, Options{Gateways: localhost(), PCPPort: f.port(), SSDPAddr: noUPnP(t), Lifetime: time.Hour}); err == nil {
		t.Fatal("mapped although the answer came after the deadline")
	}
	waitFor(t, "the removal", func() bool { return slices.Contains(f.lifetimes(), 0) })
}

// A NAT-PMP router answering on two of the addresses asked answers both
// requests with the same mapping: the second answer, after the first won,
// leaves the mapping alone.
func TestMapNATPMPSameRouterAnswersTwice(t *testing.T) {
	first := newFakePCPDelayed(t, true, "203.0.113.18", 100*time.Millisecond)
	second := newFakePCPAt(t, "127.0.0.2", first.port(), true, "203.0.113.18", 150*time.Millisecond)
	gws := []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")}
	m, err := Map(context.Background(), 40147, Options{Gateways: gws, PCPPort: first.port(), SSDPAddr: noUPnP(t), Lifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	time.Sleep(300 * time.Millisecond)
	if a, b := first.lifetimes(), second.lifetimes(); slices.Contains(a, 0) || slices.Contains(b, 0) {
		t.Fatalf("lifetimes %v and %v: the mapping granted was removed", a, b)
	}
}

// Two routers with public addresses both grant a mapping: one wins, and the
// other's is removed.
func TestMapPCPRemovesTheOtherRoutersMapping(t *testing.T) {
	one := newFakePCPDelayed(t, false, "203.0.113.19", 100*time.Millisecond)
	two := newFakePCPAt(t, "127.0.0.2", one.port(), false, "203.0.113.20", 150*time.Millisecond)
	gws := []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")}
	m, err := Map(context.Background(), 40148, Options{Gateways: gws, PCPPort: one.port(), SSDPAddr: noUPnP(t), Lifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	winner, other := one, two
	if m.ExternalIP.Equal(net.ParseIP("203.0.113.20")) {
		winner, other = two, one
	}
	waitFor(t, "the other router's removal", func() bool { return slices.Contains(other.lifetimes(), 0) })
	if got := winner.lifetimes(); slices.Contains(got, 0) {
		t.Fatalf("the winner's lifetimes = %v: its mapping was removed", got)
	}
}

// The NAT-PMP version: the request went out, and its answer came too late.
func TestMapNATPMPRemovesAMappingAnsweredTooLate(t *testing.T) {
	f := newFakePCPDelayed(t, true, "203.0.113.15", 600*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := Map(ctx, 40142, Options{Gateways: localhost(), PCPPort: f.port(), SSDPAddr: noUPnP(t), Lifetime: time.Hour}); err == nil {
		t.Fatal("mapped although the answer came after the deadline")
	}
	waitFor(t, "the removal", func() bool { return slices.Contains(f.lifetimes(), 0) })
}

func TestMappingIsRenewed(t *testing.T) {
	f := newFakePCP(t, false, "203.0.113.16")
	m, err := Map(context.Background(), 40143, Options{Gateways: localhost(), PCPPort: f.port(), SSDPAddr: noUPnP(t), Lifetime: 2 * time.Second, renewFloor: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	waitFor(t, "a renewal", func() bool { return len(f.lifetimes()) >= 2 })
	if got := f.lifetimes(); got[0] != 2 || got[1] != 2 {
		t.Fatalf("lifetimes = %v, want the mapping asked for again", got)
	}
}

// CloseAll waits for a Close already under way, so that the game does not
// exit before the router was asked to remove the mapping.
func TestCloseAllWaitsForACloseUnderWay(t *testing.T) {
	f := newFakePCPDelayed(t, false, "203.0.113.17", 500*time.Millisecond)
	m, err := Map(context.Background(), 40144, Options{Gateways: localhost(), PCPPort: f.port(), SSDPAddr: noUPnP(t), Lifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		m.Close()
		close(closed)
	}()
	waitFor(t, "the removal request", func() bool { return slices.Contains(f.lifetimes(), 0) })
	CloseAll(2 * time.Second)
	select {
	case <-closed:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("CloseAll returned while a Close was still removing its mapping")
	}
}

func TestCloseAll(t *testing.T) {
	f := newFakePCP(t, false, "203.0.113.13")
	for _, port := range []int{40131, 40132} {
		if _, err := Map(context.Background(), port, Options{Gateways: localhost(), PCPPort: f.port(), SSDPAddr: noUPnP(t), Lifetime: time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
	CloseAll(2 * time.Second)
	if got := f.lifetimes(); len(got) != 4 || got[2] != 0 || got[3] != 0 {
		t.Fatalf("requests' lifetimes = %v, want two mappings, then both removed", got)
	}
	openMu.Lock()
	left := len(open)
	openMu.Unlock()
	if left != 0 {
		t.Fatalf("%d mappings still open", left)
	}
}

func closedUDPPort(t *testing.T) int {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()
	return port
}

// igdConfig shapes a fakeIGD.
type igdConfig struct {
	external string
	// conflictFirst answers the first AddPortMapping with 718; permanent
	// answers AddPortMapping with a non-zero lease with 725.
	conflictFirst, permanent bool
	// downFirst lists a WANIPConnection that is not connected (no public
	// address) before the one that is; ppp lists a WANPPPConnection after.
	downFirst, ppp bool
	// redirect answers the description with a redirect to it.
	redirect string
	// addDelay holds AddPortMapping's answer back; the mapping is made at once.
	addDelay time.Duration
	// location, if set, is the description URL the search is answered with.
	location string
	// charset, if set, is declared by the description, which then names the
	// router with a byte of that charset.
	charset string
	// controlURL and serviceType, if set, are the connection service's in
	// the description, as they are (XML).
	controlURL, serviceType string
}

// fakeIGD is a UPnP internet gateway: an SSDP responder and an HTTP server
// for its description and its connection services' control URLs.
type fakeIGD struct {
	igdConfig
	ssdp *net.UDPConn
	http *httptest.Server

	mu      sync.Mutex
	adds    []string          // "external:lease"
	deletes []string          // external ports
	mapped  map[string]string // external port -> "client:port"
}

func newFakeIGD(t *testing.T, cfg igdConfig) *fakeIGD {
	t.Helper()
	f := &fakeIGD{igdConfig: cfg, mapped: map[string]string{}}
	envelope := func(w http.ResponseWriter, inner string) {
		fmt.Fprint(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>`+inner+`</s:Body></s:Envelope>`)
	}
	fault := func(w http.ResponseWriter, code int) {
		w.WriteHeader(http.StatusInternalServerError)
		envelope(w, fmt.Sprintf(`<s:Fault><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>fake</errorDescription></UPnPError></detail></s:Fault>`, code))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/desc.xml", func(w http.ResponseWriter, r *http.Request) {
		decl, name := `<?xml version="1.0"?>`, "Router"
		if f.charset != "" {
			decl, name = `<?xml version="1.0" encoding="`+f.charset+`"?>`, "Routeur \xe9" // é in ISO-8859-1
		}
		if f.redirect != "" {
			http.Redirect(w, r, f.redirect, http.StatusFound)
			return
		}
		down, ppp := "", ""
		if f.downFirst {
			down = `<device><deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
<serviceList><service><serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType><controlURL>/ctl/IPConnDown</controlURL></service></serviceList></device>`
		}
		if f.ppp {
			ppp = `<device><deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
<serviceList><service><serviceType>urn:schemas-upnp-org:service:WANPPPConnection:1</serviceType><controlURL>/ctl/PPPConn</controlURL></service></serviceList></device>`
		}
		control, serviceType := "/ctl/IPConn", "urn:schemas-upnp-org:service:WANIPConnection:1"
		if f.controlURL != "" {
			control = f.controlURL
		}
		if f.serviceType != "" {
			serviceType = f.serviceType
		}
		// The services sit two levels down, as in real gateways.
		fmt.Fprint(w, decl+`<root xmlns="urn:schemas-upnp-org:device-1-0"><device>
<deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType><friendlyName>`+name+`</friendlyName>
<serviceList><service><serviceType>urn:schemas-upnp-org:service:Layer3Forwarding:1</serviceType><controlURL>/l3f</controlURL></service></serviceList>
<deviceList><device><deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
<deviceList>`+down+`<device><deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
<serviceList><service><serviceType>`+serviceType+`</serviceType><controlURL>`+control+`</controlURL></service></serviceList>
</device>`+ppp+`</deviceList></device></deviceList></device></root>`)
	})
	// A connection service that is not connected: no public address, and no
	// mappings.
	notConnected := func(st string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.Header.Get("SOAPAction"), `#GetExternalIPAddress"`) {
				envelope(w, `<u:GetExternalIPAddressResponse xmlns:u="urn:schemas-upnp-org:service:`+st+`:1"><NewExternalIPAddress></NewExternalIPAddress></u:GetExternalIPAddressResponse>`)
				return
			}
			f.mu.Lock()
			f.adds = append(f.adds, "down")
			f.mu.Unlock()
			fault(w, 401)
		}
	}
	mux.HandleFunc("/ctl/IPConnDown", notConnected("WANIPConnection"))
	mux.HandleFunc("/ctl/PPPConn", notConnected("WANPPPConnection"))
	mux.HandleFunc("/ctl/IPConn", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		action := r.Header.Get("SOAPAction")
		field := func(name string) string {
			s := string(body)
			i := strings.Index(s, "<"+name+">")
			j := strings.Index(s, "</"+name+">")
			if i < 0 || j < i {
				return ""
			}
			return s[i+len(name)+2 : j]
		}
		const ns = `xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"`
		switch {
		case strings.HasSuffix(action, `#AddPortMapping"`):
			f.mu.Lock()
			first := len(f.adds) == 0
			f.adds = append(f.adds, field("NewExternalPort")+":"+field("NewLeaseDuration"))
			f.mu.Unlock()
			if field("NewInternalClient") != "127.0.0.1" || field("NewProtocol") != "UDP" {
				fault(w, 402)
				return
			}
			if f.conflictFirst && first {
				fault(w, upnpConflict)
				return
			}
			if f.permanent && field("NewLeaseDuration") != "0" {
				fault(w, upnpOnlyPermanent)
				return
			}
			f.mu.Lock()
			f.mapped[field("NewExternalPort")] = field("NewInternalClient") + ":" + field("NewInternalPort")
			f.mu.Unlock()
			time.Sleep(f.addDelay)
			envelope(w, `<u:AddPortMappingResponse `+ns+`/>`)
		case strings.HasSuffix(action, `#GetExternalIPAddress"`):
			envelope(w, `<u:GetExternalIPAddressResponse `+ns+`><NewExternalIPAddress>`+f.external+`</NewExternalIPAddress></u:GetExternalIPAddressResponse>`)
		case strings.HasSuffix(action, `#GetSpecificPortMappingEntry"`):
			f.mu.Lock()
			entry, ok := f.mapped[field("NewExternalPort")]
			f.mu.Unlock()
			if !ok {
				fault(w, upnpNoSuchEntry)
				return
			}
			client, port, _ := strings.Cut(entry, ":")
			envelope(w, `<u:GetSpecificPortMappingEntryResponse `+ns+`><NewInternalPort>`+port+`</NewInternalPort><NewInternalClient>`+client+`</NewInternalClient><NewEnabled>1</NewEnabled></u:GetSpecificPortMappingEntryResponse>`)
		case strings.HasSuffix(action, `#DeletePortMapping"`):
			f.mu.Lock()
			f.deletes = append(f.deletes, field("NewExternalPort"))
			delete(f.mapped, field("NewExternalPort"))
			f.mu.Unlock()
			envelope(w, `<u:DeletePortMappingResponse `+ns+`/>`)
		default:
			fault(w, 401)
		}
	})
	f.http = httptest.NewServer(mux)
	t.Cleanup(f.http.Close)
	location := f.http.URL + "/desc.xml"
	if f.location != "" {
		location = f.location
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f.ssdp = conn
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if !strings.HasPrefix(string(buf[:n]), "M-SEARCH") {
				continue
			}
			resp := "HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=120\r\nST: upnp:rootdevice\r\nLocation: " + location + "\r\n\r\n"
			conn.WriteToUDP([]byte(resp), from)
		}
	}()
	return f
}

func (f *fakeIGD) calls() (adds, deletes []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.adds...), append([]string(nil), f.deletes...)
}

// upnpOptions asks only the fake IGD: no PCP gateway, and its answers count.
func upnpOptions(f *fakeIGD) Options {
	return Options{Gateways: []netip.Addr{}, Routers: localhost(), SSDPAddr: f.ssdp.LocalAddr().String(), Lifetime: time.Hour}
}

func TestMapUPnP(t *testing.T) {
	f := newFakeIGD(t, igdConfig{external: "198.51.100.20"})
	m, err := Map(context.Background(), 40127, upnpOptions(f))
	if err != nil {
		t.Fatal(err)
	}
	if m.Method != "upnp" || !m.ExternalIP.Equal(net.ParseIP("198.51.100.20")) || m.ExternalPort != 40127 {
		t.Fatalf("mapping = %s %s:%d", m.Method, m.ExternalIP, m.ExternalPort)
	}
	m.Close()
	adds, deletes := f.calls()
	if len(adds) != 1 || adds[0] != "40127:3600" || len(deletes) != 1 || deletes[0] != "40127" {
		t.Fatalf("adds %v, deletes %v", adds, deletes)
	}
}

func TestMapUPnPConflict(t *testing.T) {
	f := newFakeIGD(t, igdConfig{external: "198.51.100.21", conflictFirst: true})
	m, err := Map(context.Background(), 40128, upnpOptions(f))
	if err != nil {
		t.Fatal(err)
	}
	if m.ExternalPort == 40128 {
		t.Fatalf("the conflicting external port %d was kept", m.ExternalPort)
	}
	m.Close()
	adds, deletes := f.calls()
	if len(adds) != 2 || !strings.HasPrefix(adds[0], "40128:") || len(deletes) != 1 || deletes[0]+":3600" != adds[1] {
		t.Fatalf("adds %v, deletes %v", adds, deletes)
	}
}

// A mapping would outlive a game that crashed if it were permanent.
func TestMapUPnPMakesNoPermanentMapping(t *testing.T) {
	f := newFakeIGD(t, igdConfig{external: "198.51.100.22", permanent: true})
	if _, err := Map(context.Background(), 40133, upnpOptions(f)); !errors.Is(err, ErrNoRouter) || !strings.Contains(err.Error(), "permanent") {
		t.Fatalf("err = %v", err)
	}
	adds, _ := f.calls()
	for _, a := range adds {
		if strings.HasSuffix(a, ":0") {
			t.Fatalf("asked for a permanent mapping: adds %v", adds)
		}
	}
}

func TestMapUPnPRefusesARouterBehindAnotherNAT(t *testing.T) {
	f := newFakeIGD(t, igdConfig{external: "100.64.7.7"})
	if _, err := Map(context.Background(), 40134, upnpOptions(f)); !errors.Is(err, ErrNoRouter) || !strings.Contains(err.Error(), "behind another NAT") {
		t.Fatalf("err = %v", err)
	}
	if adds, _ := f.calls(); len(adds) != 0 {
		t.Fatalf("mapped through a router behind another NAT: %v", adds)
	}
}

// A router's first connection service may be one that is not connected.
func TestMapUPnPTriesEachConnectionService(t *testing.T) {
	f := newFakeIGD(t, igdConfig{external: "198.51.100.23", downFirst: true, ppp: true})
	m, err := Map(context.Background(), 40135, upnpOptions(f))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if adds, _ := f.calls(); len(adds) != 1 || adds[0] != "40135:3600" {
		t.Fatalf("adds %v, want one through the WANIPConnection", adds)
	}
}

// A redirect could lead the game's requests anywhere: none is followed.
func TestMapUPnPFollowsNoRedirect(t *testing.T) {
	var hits int
	var mu sync.Mutex
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer elsewhere.Close()
	f := newFakeIGD(t, igdConfig{external: "198.51.100.28", redirect: elsewhere.URL + "/desc.xml"})
	if _, err := Map(context.Background(), 40141, upnpOptions(f)); !errors.Is(err, ErrNoRouter) {
		t.Fatalf("err = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("the redirect was followed %d times", hits)
	}
}

func TestMapUPnPReadsALatin1Description(t *testing.T) {
	f := newFakeIGD(t, igdConfig{external: "198.51.100.24", charset: "ISO-8859-1"})
	m, err := Map(context.Background(), 40136, upnpOptions(f))
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
}

// An AddPortMapping given up on before its answer came may have been made:
// the mapping is removed, being this machine's.
func TestMapUPnPRemovesAMappingAnsweredTooLate(t *testing.T) {
	f := newFakeIGD(t, igdConfig{external: "198.51.100.25", addDelay: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if _, err := Map(ctx, 40137, upnpOptions(f)); err == nil {
		t.Fatal("mapped although the answer came after the deadline")
	}
	waitFor(t, "the removal", func() bool {
		_, deletes := f.calls()
		return slices.Contains(deletes, "40137")
	})
}

// Only a router's answers count, and only for a description on the router.
func TestMapUPnPIgnoresAnswersFromOtherDevices(t *testing.T) {
	f := newFakeIGD(t, igdConfig{external: "198.51.100.26"})
	opt := upnpOptions(f)
	opt.Routers = []netip.Addr{netip.MustParseAddr("192.0.2.1")}
	if _, err := Map(context.Background(), 40138, opt); !errors.Is(err, ErrNoRouter) {
		t.Fatalf("err = %v", err)
	}
	// The description named by a host name, here one for the answering device.
	g := newFakeIGD(t, igdConfig{external: "198.51.100.27", location: strings.Replace(f.http.URL, "127.0.0.1", "localhost", 1) + "/desc.xml"})
	if _, err := Map(context.Background(), 40139, upnpOptions(g)); !errors.Is(err, ErrNoRouter) {
		t.Fatalf("err = %v", err)
	}
	for _, x := range []*fakeIGD{f, g} {
		if adds, _ := x.calls(); len(adds) != 0 {
			t.Fatalf("mapped through an answer that does not count: %v", adds)
		}
	}
}

func TestSSDPAnswerCounts(t *testing.T) {
	router := netip.MustParseAddr("192.168.0.1")
	for _, c := range []struct {
		location string
		from     string
		routers  []netip.Addr
		counts   bool
	}{
		{"http://192.168.0.1:5000/desc.xml", "192.168.0.1", []netip.Addr{router}, true},
		{"http://192.168.0.1:5000/desc.xml", "192.168.0.1", nil, true},
		{"http://192.168.0.1:5000/desc.xml", "192.168.0.7", nil, false},
		{"http://192.168.0.7:5000/desc.xml", "192.168.0.7", []netip.Addr{router}, false},
		{"http://router.lan:5000/desc.xml", "192.168.0.1", nil, false},
		{"file:///etc/passwd", "192.168.0.1", nil, false},
		{"", "192.168.0.1", nil, false},
	} {
		a := ssdpAnswer{location: c.location, from: netip.MustParseAddr(c.from)}
		if got := ssdpAnswerCounts(a, c.routers); got != c.counts {
			t.Errorf("ssdpAnswerCounts(%q from %s, %v) = %v", c.location, c.from, c.routers, got)
		}
	}
}

func TestNetworkEndsAndPublicAddresses(t *testing.T) {
	_, n, _ := net.ParseCIDR("192.168.4.37/24")
	n.IP = net.ParseIP("192.168.4.37")
	ends := networkEnds(n)
	if len(ends) != 2 || ends[0].String() != "192.168.4.1" || ends[1].String() != "192.168.4.254" {
		t.Fatalf("ends = %v", ends)
	}
	for ip, public := range map[string]bool{
		"8.8.8.8": true, "203.0.113.5": true, "198.51.100.1": true, "192.0.2.1": true,
		"192.168.1.1": false, "10.1.2.3": false, "172.20.0.1": false,
		"100.64.0.1": false, "100.127.255.254": false, "100.128.0.1": true,
		"127.0.0.1": false, "169.254.1.1": false, "0.0.0.0": false, "0.1.2.3": false,
		"192.0.0.2": false, "198.18.0.1": false, "198.19.255.1": false, "198.20.0.1": true,
		"224.0.0.1": false, "240.0.0.1": false, "255.255.255.255": false, "::1": false,
	} {
		if got := publicIPv4(net.ParseIP(ip)); got != public {
			t.Errorf("publicIPv4(%s) = %v", ip, got)
		}
	}
}

// A description that names a control URL on another host is not followed:
// the requests go to the device that answered the search, or nowhere.
func TestMapUPnPSendsNothingToAnotherHost(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skip("no server can listen on 127.0.0.2 here:", err)
	}
	var hits atomic.Int32
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	other.Listener.Close()
	other.Listener = ln
	other.Start()
	defer other.Close()
	f := newFakeIGD(t, igdConfig{external: "198.51.100.30", controlURL: other.URL + "/ctl/IPConn"})
	if _, err := Map(context.Background(), 40145, upnpOptions(f)); !errors.Is(err, ErrNoRouter) {
		t.Fatalf("err = %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d requests went to another host", n)
	}
}

// A service whose type is not exactly a connection service's is skipped: the
// type is written as it is into the SOAP request.
func TestMapUPnPSkipsAMalformedServiceType(t *testing.T) {
	f := newFakeIGD(t, igdConfig{external: "198.51.100.31", serviceType: `urn:schemas-upnp-org:service:WANIPConnection:1&quot;&gt;&lt;x`})
	if _, err := Map(context.Background(), 40146, upnpOptions(f)); !errors.Is(err, ErrNoRouter) {
		t.Fatalf("err = %v", err)
	}
	if adds, _ := f.calls(); len(adds) != 0 {
		t.Fatalf("a service of a malformed type was asked for mappings: %v", adds)
	}
}
