//go:build !linux && !windows

package portmap

import "net/netip"

// systemGateways: macOS and the BSDs keep their route tables behind system
// calls of their own; the guesses from the local networks (gatewayGuesses)
// find the usual home router, and UPnP finds it by searching.
func systemGateways() []netip.Addr { return nil }
