//go:build windows

package main

import (
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

// IfType values of mobile broadband adapters (not defined in x/sys/windows).
const (
	ifTypeWWANPP  = 243
	ifTypeWWANPP2 = 244
)

func interfaceKind(ifc net.Interface) string {
	size := uint32(16 * 1024)
	for attempt := 0; attempt < 3; attempt++ {
		buf := make([]byte, size)
		first := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		flags := uint32(windows.GAA_FLAG_SKIP_ANYCAST | windows.GAA_FLAG_SKIP_MULTICAST | windows.GAA_FLAG_SKIP_DNS_SERVER)
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, flags, 0, first, &size)
		if err == windows.ERROR_BUFFER_OVERFLOW {
			continue
		}
		if err != nil {
			return ""
		}
		for aa := first; aa != nil; aa = aa.Next {
			// net.Interfaces numbers an adapter by its IPv4 index, or by its
			// IPv6 index when it has no IPv4 one.
			index := aa.IfIndex
			if index == 0 {
				index = aa.Ipv6IfIndex
			}
			if int(index) != ifc.Index {
				continue
			}
			switch aa.IfType {
			case windows.IF_TYPE_IEEE80211:
				return netKindWifi
			case ifTypeWWANPP, ifTypeWWANPP2:
				return netKindMobile
			case windows.IF_TYPE_ETHERNET_CSMACD:
				// VPN (TAP) and virtual switch adapters present themselves
				// as Ethernet; the link beneath them is unknown.
				if virtualAdapterDescription(windows.UTF16PtrToString(aa.Description)) {
					return ""
				}
				return netKindWired
			}
			return ""
		}
		return ""
	}
	return ""
}
