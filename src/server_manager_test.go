package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestNormalizeServerConfig(t *testing.T) {
	cfg := normalizeServerConfig(IkemenServerConfig{})
	if cfg.BindAddress != "0.0.0.0" || cfg.NakamaHTTPPort != 7350 || cfg.RankedBestOf != 3 || cfg.LobbyMaxGames != 3 || cfg.ServerKey != "defaultkey" {
		t.Fatalf("unexpected normalized config: %+v", cfg)
	}
	even := normalizeServerConfig(IkemenServerConfig{RankedBestOf: 8})
	if even.RankedBestOf != 7 {
		t.Fatalf("even best-of was not normalized to odd: %+v", even)
	}
}

func TestServerWizardWritesJSON(t *testing.T) {
	input := strings.NewReader("Test Server\nmy_game\n./nakama/nakama\n./nakama/config.yml\n.\n0.0.0.0\nserver.example.com\ndefaultkey\ny\n7350\n7349\n7351\ny\ny\ny\ny\ny\n3\ny\ny\nqueue\n3\n\nlogs/server.log\n")
	var out bytes.Buffer
	path := t.TempDir() + "/server.json"
	old := sys.baseDir
	sys.baseDir = ""
	defer func() { sys.baseDir = old }()
	if err := runServerWizard(input, &out, path); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Saved server configuration") {
		t.Fatalf("wizard output missing save confirmation: %s", out.String())
	}
	cfg, err := loadIkemenServerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "Test Server" || cfg.Game != "my_game" || cfg.NakamaHTTPPort != 7350 || cfg.ServerKey != "defaultkey" || !cfg.ClientSSL {
		t.Fatalf("unexpected cfg %+v", cfg)
	}
	profilePath := strings.TrimSuffix(path, ".json") + "-client.json"
	profiles, err := LoadNakamaServerProfiles(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	profile, ok := profiles.Find("")
	if !ok || profile.Host != "server.example.com" || profile.Port != 7350 || profile.ServerKey != "defaultkey" || !profile.SSL {
		t.Fatalf("unexpected client profile %+v ok=%v", profile, ok)
	}
}
