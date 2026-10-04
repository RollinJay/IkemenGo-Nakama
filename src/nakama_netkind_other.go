//go:build !linux && !windows && !darwin

package main

import "net"

func interfaceKind(ifc net.Interface) string {
	return netKindFromName(ifc.Name)
}
