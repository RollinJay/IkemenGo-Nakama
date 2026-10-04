//go:build darwin

package main

import (
	"context"
	"net"
	"os/exec"
	"strings"
	"time"
)

func interfaceKind(ifc net.Interface) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "networksetup", "-listallhardwareports").Output()
	if err == nil {
		if kind := darwinPortKind(string(out), ifc.Name); kind != "" {
			return kind
		}
	}
	// No networksetup (iOS): only the cellular interfaces have telling names;
	// en0 is Wi-Fi on some Macs and Ethernet on others.
	if strings.HasPrefix(ifc.Name, "pdp_ip") {
		return netKindMobile
	}
	return ""
}
