// Package portmap asks the home router to forward a UDP port to this machine,
// so that a peer behind another NAT can reach it on the router's public
// address. Routers offer one or more of three protocols:
//
//	PCP      the Port Control Protocol (RFC 6887)
//	NAT-PMP  its predecessor (RFC 6886), on the same port of the router
//	UPnP     the UPnP Internet Gateway Device's WANIPConnection service
//
// Map asks with PCP and NAT-PMP first, and with UPnP only when neither
// mapped the port: a router that speaks both would otherwise open two ports.
// A mapping lasts for its lifetime and is renewed until Close, which asks the
// router to remove it; CloseAll does so for every mapping when a program
// exits. The router removes a mapping nobody closed at the end of its
// lifetime, so mappings are never made permanent.
package portmap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Options adjusts Map. The zero value asks the routers this machine finds.
type Options struct {
	// Description names the mapping in the router's UPnP table.
	Description string
	// Lifetime is how long the router keeps the mapping before Close; it is
	// renewed at half of it. Default one hour.
	Lifetime time.Duration
	// Gateways are the routers asked with PCP and NAT-PMP. Default: the
	// default routes' gateways where the system tells them, or else the
	// first and last address of each local IPv4 network, where home routers
	// sit.
	Gateways []netip.Addr
	// Routers are the devices whose UPnP answers count. Default: the default
	// routes' gateways where the system tells them, or else any device on the
	// network; either way only one that names a description on itself.
	Routers []netip.Addr
	// PCPPort is the routers' PCP and NAT-PMP port. Default 5351.
	PCPPort int
	// SSDPAddr is where UPnP discovery goes. Default 239.255.255.250:1900.
	SSDPAddr string
	// Logf receives what each protocol answered. Default: nothing.
	Logf func(format string, args ...any)
	// renewFloor is the shortest wait between renewals (tests shorten it).
	renewFloor time.Duration
}

// Mapping is a port the router forwards to this machine.
type Mapping struct {
	// Method is "pcp", "natpmp" or "upnp".
	Method       string
	ExternalIP   net.IP
	ExternalPort int
	InternalPort int
	Lifetime     time.Duration

	renew   func(ctx context.Context) (time.Duration, error)
	release func(ctx context.Context) error

	stop      context.CancelFunc
	closeOnce sync.Once
	done      chan struct{}
}

const (
	defaultLifetime = time.Hour
	defaultPCPPort  = 5351
	defaultSSDPAddr = "239.255.255.250:1900"
	renewTimeout    = 5 * time.Second
	releaseTimeout  = 2 * time.Second
	minRenewWait    = 10 * time.Second
)

// ErrNoRouter says that no router answered with a mapping.
var ErrNoRouter = errors.New("no router opened a port (PCP, NAT-PMP and UPnP)")

func (o Options) withDefaults() Options {
	if o.Description == "" {
		o.Description = "portmap"
	}
	if o.Lifetime <= 0 {
		o.Lifetime = defaultLifetime
	}
	if o.PCPPort == 0 {
		o.PCPPort = defaultPCPPort
	}
	if o.SSDPAddr == "" {
		o.SSDPAddr = defaultSSDPAddr
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.renewFloor <= 0 {
		o.renewFloor = minRenewWait
	}
	if o.Gateways == nil {
		o.Gateways = gatewayGuesses()
	}
	if o.Routers == nil {
		o.Routers = systemGateways()
	}
	return o
}

// Map asks the routers for a mapping of internalPort (UDP) and returns the
// first one granted. A router whose public address is itself private or
// shared (another NAT in front of it) does not help a peer on the internet:
// its mapping is removed again and does not count.
func Map(ctx context.Context, internalPort int, opt Options) (*Mapping, error) {
	if internalPort <= 0 || internalPort > 65535 {
		return nil, fmt.Errorf("portmap: invalid port %d", internalPort)
	}
	opt = opt.withDefaults()
	// UPnP's search takes a second or two, so it starts at once; the routers
	// that answer it are asked for a mapping only when PCP and NAT-PMP have
	// mapped nothing.
	upnpCtx, stopUPnP := context.WithCancel(ctx)
	defer stopUPnP()
	answers := make(chan ssdpAnswer, 16)
	go discoverUPnP(upnpCtx, opt, answers)

	m, err := mapPCP(ctx, internalPort, opt)
	errs := []error{err}
	if err != nil && ctx.Err() == nil {
		m, err = mapUPnP(upnpCtx, answers, internalPort, opt)
		if err == nil {
			err = keepIfPublic(m, opt)
		}
		errs = append(errs, err)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoRouter, errors.Join(errs...))
	}
	m.startRenewing(opt.renewFloor)
	return m, nil
}

// keepIfPublic removes a mapping whose public address is private or shared:
// another NAT is in front of the router.
func keepIfPublic(m *Mapping, opt Options) error {
	if publicIPv4(m.ExternalIP) {
		return nil
	}
	err := fmt.Errorf("%s: the router's public address %s is behind another NAT", m.Method, m.ExternalIP)
	opt.Logf("portmap: %v; removing the mapping", err)
	releaseNow(m)
	return err
}

func releaseNow(m *Mapping) {
	if m == nil || m.release == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	_ = m.release(ctx)
}

// startRenewing keeps the mapping alive: routers drop a mapping at the end of
// its lifetime, and renewing it at half of that survives a lost request. It
// waits floor at least between renewals.
func (m *Mapping) startRenewing(floor time.Duration) {
	ctx, stop := context.WithCancel(context.Background())
	m.stop = stop
	m.done = make(chan struct{})
	track(m)
	go func() {
		defer close(m.done)
		lifetime := m.Lifetime
		wait := lifetime / 2
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(max(wait, floor)):
			}
			if m.renew == nil {
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, renewTimeout)
			l, err := m.renew(rctx)
			cancel()
			switch {
			case err != nil:
				// Try again well before the mapping ends.
				wait = lifetime / 8
			case l > 0:
				lifetime, wait = l, l/2
			default:
				wait = lifetime / 2
			}
		}
	}()
}

// Close stops renewing the mapping and asks the router to remove it.
func (m *Mapping) Close() error {
	var err error
	m.closeOnce.Do(func() {
		if m.stop != nil {
			m.stop()
			<-m.done
		}
		if m.release != nil {
			ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
			defer cancel()
			err = m.release(ctx)
		}
		// Only now, so that a CloseAll meanwhile waits for this release too
		// (its Close waits in closeOnce).
		untrack(m)
	})
	return err
}

// The mappings Map made that are not closed yet, for CloseAll.
var (
	openMu sync.Mutex
	open   = map[*Mapping]bool{}
)

func track(m *Mapping) {
	openMu.Lock()
	open[m] = true
	openMu.Unlock()
}

func untrack(m *Mapping) {
	openMu.Lock()
	delete(open, m)
	openMu.Unlock()
}

// CloseAll closes every mapping not closed yet, waiting at most timeout: for
// a program's exit, so that routers do not keep its ports open until the
// mappings' lifetimes end.
func CloseAll(timeout time.Duration) {
	openMu.Lock()
	ms := make([]*Mapping, 0, len(open))
	for m := range open {
		ms = append(ms, m)
	}
	openMu.Unlock()
	if len(ms) == 0 {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, m := range ms {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = m.Close()
			}()
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// nonPublicIPv4 are the IPv4 ranges a peer on the internet cannot send to
// (RFC 6890). The documentation ranges (TEST-NET-1 to 3) are left out, so
// that test networks built on them work.
var nonPublicIPv4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // this network
	netip.MustParsePrefix("10.0.0.0/8"),     // private
	netip.MustParsePrefix("100.64.0.0/10"),  // shared: carrier-grade NAT
	netip.MustParsePrefix("127.0.0.0/8"),    // loopback
	netip.MustParsePrefix("169.254.0.0/16"), // link-local
	netip.MustParsePrefix("172.16.0.0/12"),  // private
	netip.MustParsePrefix("192.0.0.0/24"),   // protocol assignments: DS-Lite's 192.0.0.2
	netip.MustParsePrefix("192.168.0.0/16"), // private
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("224.0.0.0/4"),    // multicast
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, and the broadcast address
}

// publicIPv4 reports whether ip can be a peer's destination on the internet.
func publicIPv4(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip.To4())
	if !ok {
		return false
	}
	for _, p := range nonPublicIPv4 {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// gatewayGuesses lists the routers to ask with PCP and NAT-PMP: the default
// routes' gateways where the system tells them (systemGateways), or else
// the first and last host address of each local IPv4 network, the addresses
// home routers use.
func gatewayGuesses() []netip.Addr {
	seen := map[netip.Addr]bool{}
	var out []netip.Addr
	add := func(a netip.Addr) {
		if a.IsValid() && a.Is4() && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, gw := range systemGateways() {
		add(gw)
	}
	if len(out) > 0 {
		return out
	}
	interfaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range interfaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				if ipnet, ok := addr.(*net.IPNet); ok {
					for _, a := range networkEnds(ipnet) {
						add(a)
					}
				}
			}
		}
	} else if conn, err := net.Dial("udp4", "192.0.2.1:9"); err == nil {
		// Android does not let apps list interfaces: assume a /24 around the
		// address internet traffic leaves from.
		if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			for _, e := range networkEnds(&net.IPNet{IP: a.IP, Mask: net.CIDRMask(24, 32)}) {
				add(e)
			}
		}
		conn.Close()
	}
	return out
}

// networkEnds returns the first and last host address of an IPv4 network
// (192.168.1.1 and 192.168.1.254 for 192.168.1.0/24).
func networkEnds(n *net.IPNet) []netip.Addr {
	ip4 := n.IP.To4()
	ones, bits := n.Mask.Size()
	if ip4 == nil || bits != 32 || ones < 8 || ones > 30 {
		return nil
	}
	mask := net.CIDRMask(ones, 32)
	var network, broadcast [4]byte
	for i := 0; i < 4; i++ {
		network[i] = ip4[i] & mask[i]
		broadcast[i] = ip4[i] | ^mask[i]
	}
	first := network
	first[3]++
	last := broadcast
	last[3]--
	return []netip.Addr{netip.AddrFrom4(first), netip.AddrFrom4(last)}
}
