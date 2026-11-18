package main

import (
	"net"
	"testing"
)

type labelAddr string

func (a labelAddr) Network() string { return "nakama" }
func (a labelAddr) String() string  { return string(a) }

// synchronizeNetplayConfig reads the peer's IP from the connection; online
// that is the Nakama P2P link's *net.UDPAddr, which a *net.TCPAddr type
// assertion panicked on, crashing every online match's guest.
func TestAddrIP(t *testing.T) {
	cases := []struct {
		addr net.Addr
		want string
	}{
		{&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7500}, "127.0.0.1"},
		{&net.UDPAddr{IP: net.ParseIP("192.168.240.112"), Port: 7500}, "192.168.240.112"},
		{labelAddr("10.0.0.2:7500"), "10.0.0.2"},
		{labelAddr("host-label"), "<nil>"},
		{nil, "<nil>"},
	}
	for _, c := range cases {
		if got := addrIP(c.addr).String(); got != c.want {
			t.Errorf("addrIP(%v) = %s, want %s", c.addr, got, c.want)
		}
	}
	if ip := addrIP(&net.UDPAddr{IP: net.ParseIP("127.0.0.1")}); ip.To4() == nil || !ip.IsLoopback() {
		t.Error("a loopback UDP peer is not seen as loopback")
	}
}
