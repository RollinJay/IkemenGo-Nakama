package portmap

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// PCP (RFC 6887) and NAT-PMP (RFC 6886) share the router's UDP port 5351. A
// router that speaks only NAT-PMP answers a PCP request with NAT-PMP's
// "unsupported version", so each gateway is asked with PCP first and with
// NAT-PMP when it says so, or does not answer PCP at all.
const (
	pcpVersion     = 2
	pcpOpMap       = 1
	pcpProtoUDP    = 17
	pcpRequestSize = 60
	natpmpVersion  = 0
	natpmpOpAddr   = 0
	natpmpOpMapUDP = 1
	// Each protocol gets this long per gateway: a router on the network
	// answers within milliseconds.
	pcpAttemptTimeout = 900 * time.Millisecond
)

// undo removes what a request given up on may have mapped: the router may
// have granted it although its answer never came. A NAT-PMP mapping is
// named by this machine's address and port alone (shared), so its undo could
// remove a mapping another address of the same router granted.
type undo struct {
	release func(ctx context.Context) error
	shared  bool
}

// mapPCP asks every gateway at once and returns the first mapping granted
// on a public address. A mapping on a private or shared one is removed and
// does not count (keepIfPublic), and neither does another gateway's mapping,
// which is removed too, unless it is the same one: a router that answers on
// two of the addresses asked. The requests given up on are undone in the
// background, but for a shared undo when a mapping was granted.
func mapPCP(ctx context.Context, port int, opt Options) (*Mapping, error) {
	if len(opt.Gateways) == 0 {
		return nil, errors.New("pcp/nat-pmp: no gateway to ask")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		m     *Mapping
		undos []undo
		err   error
	}
	results := make(chan result, len(opt.Gateways))
	for _, gw := range opt.Gateways {
		go func(gw netip.Addr) {
			m, undos, err := mapGateway(ctx, gw, port, opt)
			results <- result{m, undos, err}
		}(gw)
	}
	var won *Mapping
	var errs []error
	var undos []undo
	defer func() {
		for _, u := range undos {
			if u.shared && won != nil {
				continue
			}
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
				defer cancel()
				_ = u.release(ctx)
			}()
		}
	}()
	for range opt.Gateways {
		r := <-results
		undos = append(undos, r.undos...)
		if r.err == nil {
			// Before it wins: a router behind another NAT answering first
			// would have the others' mappings removed.
			r.err = keepIfPublic(r.m, opt)
		}
		switch {
		case r.err != nil:
			errs = append(errs, r.err)
		case won == nil:
			won = r.m
			cancel()
		case !r.m.ExternalIP.Equal(won.ExternalIP) || r.m.ExternalPort != won.ExternalPort:
			releaseNow(r.m)
		}
	}
	if won == nil {
		return nil, errors.Join(errs...)
	}
	return won, nil
}

func mapGateway(ctx context.Context, gw netip.Addr, port int, opt Options) (*Mapping, []undo, error) {
	server := net.UDPAddrFromAddrPort(netip.AddrPortFrom(gw, uint16(opt.PCPPort)))
	var undos []undo
	m, natpmpOnly, undoPCP, err := mapPCPGateway(ctx, server, port, opt)
	if undoPCP != nil {
		undos = append(undos, undo{release: undoPCP})
	}
	if err == nil {
		return m, undos, nil
	}
	if ctx.Err() != nil {
		return nil, undos, err
	}
	opt.Logf("portmap: PCP to %s: %v", server, err)
	m, undoNATPMP, err2 := mapNATPMPGateway(ctx, server, port, opt)
	if undoNATPMP != nil {
		undos = append(undos, undo{release: undoNATPMP, shared: true})
	}
	if err2 == nil {
		return m, undos, nil
	}
	if natpmpOnly {
		return nil, undos, fmt.Errorf("nat-pmp %s: %w", server, err2)
	}
	return nil, undos, fmt.Errorf("pcp %s: %v; nat-pmp: %w", server, err, err2)
}

// exchange sends req to the connected conn, sending it again after 250 ms,
// then 500 ms, until an answer that accept takes or until timeout or ctx
// ends. sent reports whether req went out at all.
func exchange(ctx context.Context, conn *net.UDPConn, req []byte, timeout time.Duration, accept func([]byte) bool) (resp []byte, sent bool, err error) {
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	buf := make([]byte, 1100)
	wait := 250 * time.Millisecond
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil, sent, ctx.Err()
		}
		if _, err := conn.Write(req); err != nil {
			return nil, sent, err
		}
		sent = true
		next := time.Now().Add(wait)
		if next.After(deadline) {
			next = deadline
		}
		wait *= 2
		_ = conn.SetReadDeadline(next)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break
				}
				// An ICMP "port unreachable" from a gateway without the
				// service reads as a refused connection: nothing will answer.
				return nil, sent, err
			}
			if accept(buf[:n]) {
				return append([]byte(nil), buf[:n]...), sent, nil
			}
		}
	}
	return nil, sent, errors.New("no answer")
}

// ---- PCP -------------------------------------------------------------------

func pcpMapRequest(client net.IP, nonce []byte, internalPort, externalPort int, lifetime uint32) []byte {
	b := make([]byte, pcpRequestSize)
	b[0] = pcpVersion
	b[1] = pcpOpMap
	binary.BigEndian.PutUint32(b[4:8], lifetime)
	copy(b[8:24], client.To16()) // IPv4-mapped (::ffff:a.b.c.d)
	copy(b[24:36], nonce)
	b[36] = pcpProtoUDP
	binary.BigEndian.PutUint16(b[40:42], uint16(internalPort))
	binary.BigEndian.PutUint16(b[42:44], uint16(externalPort))
	// The suggested external address: any IPv4 address (::ffff:0.0.0.0).
	b[54], b[55] = 0xff, 0xff
	return b
}

// pcpResult names PCP's result codes (RFC 6887, section 7.4).
func pcpResult(code byte) string {
	switch code {
	case 1:
		return "unsupported version"
	case 2:
		return "not authorized"
	case 3:
		return "malformed request"
	case 4:
		return "unsupported opcode"
	case 7:
		return "network failure"
	case 8:
		return "no resources"
	case 9:
		return "unsupported protocol"
	case 11:
		return "cannot provide an external port"
	case 12:
		return "address mismatch"
	}
	return fmt.Sprintf("result %d", code)
}

// errNATPMPOnly is what a PCP request gets from a router that speaks only
// NAT-PMP: an answer in NAT-PMP's format.
var errNATPMPOnly = errors.New("the router speaks NAT-PMP only")

// mapPCPGateway asks server with PCP. undo is set when a request went out
// without an answer (see undo).
func mapPCPGateway(ctx context.Context, server *net.UDPAddr, port int, opt Options) (m *Mapping, natpmpOnly bool, undo func(context.Context) error, err error) {
	conn, err := net.DialUDP("udp4", nil, server)
	if err != nil {
		return nil, false, nil, err
	}
	defer conn.Close()
	client := conn.LocalAddr().(*net.UDPAddr).IP
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, false, nil, err
	}
	lifetime := uint32(opt.Lifetime / time.Second)

	// request asks for the mapping, or (lifetime 0) its removal. A renewal
	// repeats the request with the same nonce, which the router recognises.
	// pending reports a request that went out without an answer.
	request := func(ctx context.Context, conn *net.UDPConn, lifetime uint32, externalPort int) (resultLifetime uint32, extPort int, extIP net.IP, pending bool, err error) {
		req := pcpMapRequest(client, nonce, port, externalPort, lifetime)
		resp, sent, err := exchange(ctx, conn, req, pcpAttemptTimeout, func(b []byte) bool {
			return (len(b) >= 4 && b[0] == natpmpVersion) ||
				(len(b) >= 60 && b[0] == pcpVersion && b[1] == 0x80|pcpOpMap && string(b[24:36]) == string(nonce))
		})
		if err != nil {
			return 0, 0, nil, sent, err
		}
		if resp[0] == natpmpVersion {
			return 0, 0, nil, false, errNATPMPOnly
		}
		if code := resp[3]; code != 0 {
			return 0, 0, nil, false, errors.New(pcpResult(code))
		}
		resultLifetime = binary.BigEndian.Uint32(resp[4:8])
		extPort = int(binary.BigEndian.Uint16(resp[42:44]))
		extIP = net.IP(append([]byte(nil), resp[44:60]...)).To4()
		return resultLifetime, extPort, extIP, false, nil
	}
	redial := func() (*net.UDPConn, error) { return net.DialUDP("udp4", nil, server) }
	release := func(ctx context.Context) error {
		c, err := redial()
		if err != nil {
			return err
		}
		defer c.Close()
		_, _, _, _, err = request(ctx, c, 0, 0)
		return err
	}

	granted, extPort, extIP, pending, err := request(ctx, conn, lifetime, port)
	if err != nil {
		if pending {
			// The removal names the mapping by its nonce, so it removes
			// nothing but what this request may have mapped.
			undo = release
		}
		return nil, errors.Is(err, errNATPMPOnly), undo, err
	}
	if extIP == nil || extPort == 0 || granted == 0 {
		return nil, false, nil, errors.New("the router granted no usable mapping")
	}
	opt.Logf("portmap: PCP: %s maps %s:%d to port %d for %ds", server, extIP, extPort, port, granted)
	m = &Mapping{
		Method: "pcp", ExternalIP: extIP, ExternalPort: extPort, InternalPort: port,
		Lifetime: time.Duration(granted) * time.Second,
		renew: func(ctx context.Context) (time.Duration, error) {
			c, err := redial()
			if err != nil {
				return 0, err
			}
			defer c.Close()
			l, _, _, _, err := request(ctx, c, lifetime, extPort)
			return time.Duration(l) * time.Second, err
		},
		release: release,
	}
	return m, false, nil, nil
}

// ---- NAT-PMP ---------------------------------------------------------------

// natpmpResult names NAT-PMP's result codes (RFC 6886, section 3.5).
func natpmpResult(code uint16) string {
	switch code {
	case 1:
		return "unsupported version"
	case 2:
		return "not authorized or refused"
	case 3:
		return "network failure"
	case 4:
		return "out of resources"
	case 5:
		return "unsupported opcode"
	}
	return fmt.Sprintf("result %d", code)
}

// mapNATPMPGateway asks server with NAT-PMP. undo is set when the map
// request went out without an answer (see undo).
func mapNATPMPGateway(ctx context.Context, server *net.UDPAddr, port int, opt Options) (m *Mapping, undo func(context.Context) error, err error) {
	conn, err := net.DialUDP("udp4", nil, server)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()

	// The external address comes from its own request.
	resp, _, err := exchange(ctx, conn, []byte{natpmpVersion, natpmpOpAddr}, pcpAttemptTimeout, func(b []byte) bool {
		return len(b) >= 12 && b[0] == natpmpVersion && b[1] == 128+natpmpOpAddr
	})
	if err != nil {
		return nil, nil, err
	}
	if code := binary.BigEndian.Uint16(resp[2:4]); code != 0 {
		return nil, nil, errors.New(natpmpResult(code))
	}
	extIP := net.IP(append([]byte(nil), resp[8:12]...)).To4()

	lifetime := uint32(opt.Lifetime / time.Second)
	// request asks for the mapping, or (lifetime 0) its removal; pending
	// reports a request that went out without an answer.
	request := func(ctx context.Context, conn *net.UDPConn, lifetime uint32, externalPort int) (granted uint32, extPort int, pending bool, err error) {
		req := make([]byte, 12)
		req[0], req[1] = natpmpVersion, natpmpOpMapUDP
		binary.BigEndian.PutUint16(req[4:6], uint16(port))
		binary.BigEndian.PutUint16(req[6:8], uint16(externalPort))
		binary.BigEndian.PutUint32(req[8:12], lifetime)
		resp, sent, err := exchange(ctx, conn, req, pcpAttemptTimeout, func(b []byte) bool {
			return len(b) >= 16 && b[0] == natpmpVersion && b[1] == 128+natpmpOpMapUDP &&
				int(binary.BigEndian.Uint16(b[8:10])) == port
		})
		if err != nil {
			return 0, 0, sent, err
		}
		if code := binary.BigEndian.Uint16(resp[2:4]); code != 0 {
			return 0, 0, false, errors.New(natpmpResult(code))
		}
		return binary.BigEndian.Uint32(resp[12:16]), int(binary.BigEndian.Uint16(resp[10:12])), false, nil
	}
	redial := func() (*net.UDPConn, error) { return net.DialUDP("udp4", nil, server) }
	release := func(ctx context.Context) error {
		c, err := redial()
		if err != nil {
			return err
		}
		defer c.Close()
		// A mapping is removed by asking for it with lifetime 0 and external
		// port 0 (RFC 6886, section 3.4).
		_, _, _, err = request(ctx, c, 0, 0)
		return err
	}
	granted, extPort, pending, err := request(ctx, conn, lifetime, port)
	if err != nil {
		if pending {
			undo = release
		}
		return nil, undo, err
	}
	if extPort == 0 || granted == 0 {
		return nil, nil, errors.New("the router granted no usable mapping")
	}
	opt.Logf("portmap: NAT-PMP: %s maps %s:%d to port %d for %ds", server, extIP, extPort, port, granted)
	return &Mapping{
		Method: "natpmp", ExternalIP: extIP, ExternalPort: extPort, InternalPort: port,
		Lifetime: time.Duration(granted) * time.Second,
		renew: func(ctx context.Context) (time.Duration, error) {
			c, err := redial()
			if err != nil {
				return 0, err
			}
			defer c.Close()
			l, _, _, err := request(ctx, c, lifetime, extPort)
			return time.Duration(l) * time.Second, err
		},
		release: release,
	}, nil, nil
}
