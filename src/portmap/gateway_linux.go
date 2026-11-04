package portmap

import (
	"bufio"
	"encoding/hex"
	"net/netip"
	"os"
	"strings"
)

// systemGateways reads the default routes' gateways from /proc/net/route
// (Linux, and Android when it allows it), whose addresses are hexadecimal in
// the kernel's byte order:
//
//	Iface   Destination  Gateway   Flags ...
//	enp1s0  00000000     0100A8C0  0003  ...   (192.168.0.1)
func systemGateways() []netip.Addr {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseProcRoute(bufio.NewScanner(f))
}

func parseProcRoute(sc *bufio.Scanner) []netip.Addr {
	var out []netip.Addr
	first := true
	for sc.Scan() {
		if first {
			first = false // the header line
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}
		b, err := hex.DecodeString(fields[2])
		if err != nil || len(b) != 4 {
			continue
		}
		// Little-endian on the platforms Go runs Linux on that matter here.
		a := netip.AddrFrom4([4]byte{b[3], b[2], b[1], b[0]})
		if !a.IsUnspecified() {
			out = append(out, a)
		}
	}
	return out
}
