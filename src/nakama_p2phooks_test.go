package main

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/ikemen-engine/Ikemen-GO/src/portmap"
)

// TestMain keeps every test off this machine's real router and IPv6
// addresses: P2P sessions ask for both from their start, and a test that
// forgot to stub them would ask the LAN's router to open ports. Tests that
// need a fake router or IPv6 address set one with stubP2PRouter.
func TestMain(m *testing.M) {
	setP2PRouterHooks(func(h *p2pRouterHooks) {
		h.ipv6 = func() ([]net.IP, error) { return nil, nil }
		h.mapPort = func(context.Context, int) (*portmap.Mapping, error) { return nil, errors.New("no router in tests") }
		h.unmapPort = func(*portmap.Mapping) error { return nil }
	})
	p2pCloseAllPorts = func(time.Duration) {}
	os.Exit(m.Run())
}

// stubP2PRouter replaces the router hooks edit changes for the rest of the
// test.
func stubP2PRouter(t *testing.T, edit func(h *p2pRouterHooks)) {
	t.Helper()
	prev := setP2PRouterHooks(edit)
	t.Cleanup(func() { p2pRouterHooksNow.Store(prev) })
}
