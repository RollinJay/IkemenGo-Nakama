//go:build windows

package portmap

import (
	"errors"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
)

// systemGateways returns the IPv4 gateways of the network adapters that are
// up, as Windows lists them (GetAdaptersAddresses).
func systemGateways() []netip.Addr {
	size := uint32(15000) // the size Microsoft recommends to start with
	for i := 0; i < 3; i++ {
		buf := make([]byte, size)
		first := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_INET, windows.GAA_FLAG_INCLUDE_GATEWAYS, 0, first, &size)
		if errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) && size > uint32(len(buf)) {
			continue
		}
		if err != nil || size == 0 {
			return nil
		}
		var out []netip.Addr
		for a := first; a != nil; a = a.Next {
			if a.OperStatus != windows.IfOperStatusUp {
				continue
			}
			for g := a.FirstGatewayAddress; g != nil; g = g.Next {
				if ip, ok := netip.AddrFromSlice(g.Address.IP()); ok && ip.Unmap().Is4() && !ip.Unmap().IsUnspecified() {
					out = append(out, ip.Unmap())
				}
			}
		}
		return out
	}
	return nil
}
