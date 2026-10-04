//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxInterfaceKind(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, files map[string]string, dirs ...string) {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, d := range dirs {
			if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for f, content := range files {
			if err := os.WriteFile(filepath.Join(dir, f), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("wlp2s0", map[string]string{"type": "1\n"}, "wireless", "device")
	mk("myradio", map[string]string{"uevent": "DEVTYPE=wlan\nINTERFACE=myradio\n", "type": "1\n"}, "device")
	mk("modem0", map[string]string{"uevent": "DEVTYPE=wwan\n"})
	mk("lan0", map[string]string{"type": "1\n"}, "device")
	mk("veth12", map[string]string{"type": "1\n"})
	mk("tun0", map[string]string{"type": "65534\n"}, "device")
	for name, want := range map[string]string{"wlp2s0": "wifi", "myradio": "wifi", "modem0": "mobile", "lan0": "wired",
		"veth12": "", "tun0": "", "eth7": "wired", "wlan9": "wifi"} {
		if got := linuxInterfaceKind(root, name); got != want {
			t.Errorf("linuxInterfaceKind(%q) = %q, want %q", name, got, want)
		}
	}
}
