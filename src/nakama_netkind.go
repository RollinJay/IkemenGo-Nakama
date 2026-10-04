package main

import (
	"net"
	"strings"
)

// Connection type: whether this player reaches the internet over a wired
// link, Wi-Fi or mobile data. The lobby screens report it to the lobby, which
// shows it with each member's connection bars. It describes the network
// interface that carries the connection to the Nakama server (the route P2P
// traffic takes too, unless a VPN splits them), as the operating system
// reports it:
//
//	Linux, Android  /sys/class/net/<if>: wireless, phy80211, uevent DEVTYPE,
//	                then the interface name (wlan*, wlp*, rmnet*, eth*, en*...)
//	Windows         the adapter's IfType (Ethernet, IEEE 802.11, WWAN)
//	macOS           the hardware port of the device (networksetup)
//
// Virtual interfaces (VPN tunnels and adapters, virtual switches, bridges
// without a known device) and a server on this machine report "" (unknown).
// USB tethering to a phone appears as the Ethernet adapter the phone
// presents.
const (
	netKindWired  = "wired"
	netKindWifi   = "wifi"
	netKindMobile = "mobile"
)

// normalizeNetKind accepts the reported connection types and "" for unknown.
func normalizeNetKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case netKindWired, "ethernet", "lan":
		return netKindWired
	case netKindWifi, "wi-fi", "wireless", "wlan":
		return netKindWifi
	case netKindMobile, "cellular", "wwan":
		return netKindMobile
	}
	return ""
}

// connectionTypeFor classifies the interface that owns the local address of
// a connection.
func connectionTypeFor(local net.Addr) string {
	ip := netAddrIP(local)
	if ip == nil || ip.IsLoopback() {
		return ""
	}
	ifc := interfaceForIP(ip)
	if ifc == nil || ifc.Flags&net.FlagLoopback != 0 {
		return ""
	}
	return interfaceKind(*ifc)
}

func interfaceForIP(ip net.IP) *net.Interface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for i := range ifaces {
		addrs, err := ifaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var aip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				aip = v.IP
			case *net.IPAddr:
				aip = v.IP
			}
			if aip != nil && aip.Equal(ip) {
				return &ifaces[i]
			}
		}
	}
	return nil
}

// netKindFromName guesses the type from common interface names, for systems
// that give nothing better.
func netKindFromName(name string) string {
	n := strings.ToLower(name)
	hasPrefix := func(prefixes ...string) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(n, p) {
				return true
			}
		}
		return false
	}
	switch {
	case hasPrefix("wlan", "wlp", "wlx", "wifi"):
		return netKindWifi
	case hasPrefix("wwan", "rmnet", "ccmni", "pdp", "seth_lte"):
		return netKindMobile
	case hasPrefix("eth", "enp", "eno", "ens", "enx"):
		return netKindWired
	}
	return ""
}

// virtualAdapterDescription reports whether an adapter description names a
// virtual adapter (a VPN or a virtual switch), which Windows lists with the
// Ethernet type whatever link carries its traffic.
func virtualAdapterDescription(description string) bool {
	d := strings.ToLower(description)
	for _, word := range []string{"virtual", "vpn", "tap-windows", "tunnel", "loopback", "hyper-v", "vmware", "virtualbox"} {
		if strings.Contains(d, word) {
			return true
		}
	}
	return false
}

// detectConnectionType records the connection type of the socket to the
// server. The -nakama-connection command-line option replaces the detected
// value (for testing several clients on one machine).
func (n *NakamaClient) detectConnectionType(local net.Addr) {
	kind := ""
	if sys.cmdFlags != nil {
		if override, ok := sys.cmdFlags["-nakama-connection"]; ok {
			kind = normalizeNetKind(override)
		}
	}
	if kind == "" {
		kind = connectionTypeFor(local)
	}
	n.mu.Lock()
	n.connection = kind
	n.mu.Unlock()
}

// darwinPortKind finds device in the list of hardware ports that macOS's
// "networksetup -listallhardwareports" prints:
//
//	Hardware Port: Wi-Fi
//	Device: en0
func darwinPortKind(listing, device string) string {
	port := ""
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "Hardware Port:"); ok {
			port = strings.ToLower(strings.TrimSpace(v))
			continue
		}
		if v, ok := strings.CutPrefix(line, "Device:"); ok && strings.TrimSpace(v) == device {
			switch {
			case strings.Contains(port, "wi-fi"), strings.Contains(port, "airport"), strings.Contains(port, "wlan"):
				return netKindWifi
			case strings.Contains(port, "iphone"), strings.Contains(port, "ipad"), strings.Contains(port, "bluetooth"),
				strings.Contains(port, "cellular"), strings.Contains(port, "wwan"):
				return netKindMobile
			case strings.Contains(port, "ethernet"), strings.Contains(port, "lan"), strings.Contains(port, "thunderbolt"):
				return netKindWired
			}
			return ""
		}
	}
	return ""
}
