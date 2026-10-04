//go:build linux

package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
)

// sysClassNet has one entry per network interface (tests point it at a
// directory of their own).
var sysClassNet = "/sys/class/net"

func interfaceKind(ifc net.Interface) string {
	return linuxInterfaceKind(sysClassNet, ifc.Name)
}

func linuxInterfaceKind(root, name string) string {
	dir := filepath.Join(root, name)
	exists := func(p string) bool {
		_, err := os.Stat(filepath.Join(dir, p))
		return err == nil
	}
	if exists("wireless") || exists("phy80211") {
		return netKindWifi
	}
	if data, err := os.ReadFile(filepath.Join(dir, "uevent")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			switch strings.TrimSpace(line) {
			case "DEVTYPE=wlan":
				return netKindWifi
			case "DEVTYPE=wwan":
				return netKindMobile
			}
		}
	}
	if kind := netKindFromName(name); kind != "" {
		return kind
	}
	// A physical (device-backed) Ethernet interface (ARPHRD_ETHER = 1).
	if exists("device") {
		if data, err := os.ReadFile(filepath.Join(dir, "type")); err == nil && strings.TrimSpace(string(data)) == "1" {
			return netKindWired
		}
	}
	return ""
}
