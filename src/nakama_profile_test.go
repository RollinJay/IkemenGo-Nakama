package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNakamaServerProfilesFindDefault(t *testing.T) {
	profiles := NakamaServerProfiles{Default: "alpha", Servers: []NakamaServerProfile{{ID: "alpha", Host: "example.org"}}}
	server, ok := profiles.Find("")
	if !ok || server.ID != "alpha" {
		t.Fatalf("Find default = %+v, %v", server, ok)
	}
}

func TestLoadNakamaServerProfiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.json")
	data := []byte(`{"version":1,"default":"alpha","servers":[{"id":"alpha","name":"A","host":"localhost","port":7350,"server_key":"k","ssl":true}]}`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	old := sys.baseDir
	sys.baseDir = ""
	defer func() { sys.baseDir = old }()
	profiles, err := LoadNakamaServerProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	server, ok := profiles.Find("alpha")
	if !ok || server.Port != 7350 || !server.SSL {
		t.Fatalf("unexpected server %+v ok=%v", server, ok)
	}
}
