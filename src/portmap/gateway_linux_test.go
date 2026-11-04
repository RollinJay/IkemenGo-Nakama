package portmap

import (
	"bufio"
	"net/netip"
	"strings"
	"testing"
)

func TestParseProcRoute(t *testing.T) {
	table := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"enp1s0\t00000000\t0100A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"enp1s0\t0000A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n" +
		"wlo1\t00000000\t0101A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n"
	got := parseProcRoute(bufio.NewScanner(strings.NewReader(table)))
	want := []netip.Addr{netip.MustParseAddr("192.168.0.1"), netip.MustParseAddr("192.168.1.1")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("gateways = %v, want %v", got, want)
	}
}
