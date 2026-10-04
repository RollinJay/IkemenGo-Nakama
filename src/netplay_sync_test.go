package main

import (
	"strings"
	"testing"

	"gopkg.in/ini.v1"
)

// syncTestConfig is a configuration with common files and code (Common.*,
// its keys in lower case as the configuration file loads them) and a host
// setting.
func syncTestConfig(lua string, life float32) Config {
	var cfg Config
	cfg.IniFile = ini.Empty()
	cfg.Common.Lua = map[string][]string{"lua": {lua}}
	cfg.Common.States = map[string][]string{"states": {"common1.cns"}}
	cfg.Options.Life = life
	return cfg
}

func syncTestSettings(t *testing.T, cfg Config) []SyncSetting {
	t.Helper()
	settings, err := collectSyncSettings(&cfg, syncHost)
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func syncSettingValue(settings []SyncSetting, path string) (string, bool) {
	for _, st := range settings {
		if st.Path == path {
			return st.Value, true
		}
	}
	return "", false
}

// A netplay host or a replay file sets this game's host settings for the
// session, but never its common files and code (Common.*, Lua run every frame
// included): they must be this game's, or the session does not start. A
// setting this game does not have is ignored.
func TestSessionHostSettings(t *testing.T) {
	warning := sys.sessionWarning
	t.Cleanup(func() { sys.sessionWarning = warning })
	local := syncTestSettings(t, syncTestConfig("print('ok')", 100))
	if _, ok := syncSettingValue(local, "Common.lua"); !ok {
		t.Fatalf("no Common.lua setting in %v", local)
	}

	out, err := sessionHostSettings(local, syncTestSettings(t, syncTestConfig("print('ok')", 50)))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := syncSettingValue(out, "Options.Life"); v != "50" {
		t.Fatalf("Options.Life %q, want the host's 50 (%v)", v, out)
	}
	for _, st := range out {
		if strings.HasPrefix(st.Path, "Common.") {
			t.Fatalf("a common setting is applied: %v", st)
		}
	}

	changed := syncTestSettings(t, syncTestConfig("os.exit()", 100))
	missing := syncTestSettings(t, syncTestConfig("print('ok')", 100))
	added := append(append([]SyncSetting(nil), missing...), SyncSetting{Path: "Common.air", Value: "x.air"})
	for i, st := range missing {
		if st.Path == "Common.states" {
			missing = append(missing[:i:i], missing[i+1:]...)
			break
		}
	}
	for name, incoming := range map[string][]SyncSetting{"changed": changed, "missing": missing, "added": added} {
		sys.sessionWarning = ""
		if _, err := sessionHostSettings(local, incoming); err == nil || !strings.Contains(sys.sessionWarning, "Common.") {
			t.Errorf("%s common setting: err %v, warning %q", name, err, sys.sessionWarning)
		}
	}

	unknown := append(syncTestSettings(t, syncTestConfig("print('ok')", 100)), SyncSetting{Path: "Options.Unknown", Value: "1"})
	if out, err := sessionHostSettings(local, unknown); err != nil {
		t.Fatal(err)
	} else if _, ok := syncSettingValue(out, "Options.Unknown"); ok {
		t.Fatal("a setting this game does not have is applied")
	}
}

// A replay whose common Lua code differs does not start, and changes nothing;
// one whose host settings differ applies them for the session, and records
// the common settings with them.
func TestSessionOverrideRefusesCommonCode(t *testing.T) {
	warning := sys.sessionWarning
	t.Cleanup(func() { sys.sessionWarning = warning })
	s := &System{}
	s.cfg = syncTestConfig("print('ok')", 100)
	if err := s.beginSessionOverride("replay", nil, syncTestSettings(t, syncTestConfig("os.exit()", 100)), ""); err == nil {
		t.Fatal("a replay's common Lua code was accepted")
	}
	if s.netplayOverride.Active || s.cfg.Common.Lua["lua"][0] != "print('ok')" {
		t.Fatalf("the refused replay changed the game: %v %v", s.netplayOverride.Active, s.cfg.Common.Lua)
	}

	if err := s.beginSessionOverride("replay", nil, syncTestSettings(t, syncTestConfig("print('ok')", 50)), ""); err != nil {
		t.Fatal(err)
	}
	if s.cfg.Options.Life != 50 {
		t.Fatalf("Life %v, want the replay's 50", s.cfg.Options.Life)
	}
	if _, ok := syncSettingValue(s.currentReplayHeader().Host, "Common.lua"); !ok {
		t.Fatal("the session's header does not record the common settings")
	}
	if err := s.endSyncSessionOverride(); err != nil {
		t.Fatal(err)
	}
	if s.cfg.Options.Life != 100 {
		t.Fatalf("Life %v after the session, want 100", s.cfg.Options.Life)
	}
}

// A replay or netplay host cannot set a palette count a character's palette
// table cannot be allocated with, and a session whose settings fail to apply
// leaves this game's settings as they were.
func TestSessionOverrideBounds(t *testing.T) {
	warning := sys.sessionWarning
	t.Cleanup(func() { sys.sessionWarning = warning })
	s := &System{}
	s.cfg = syncTestConfig("print('ok')", 100)
	s.cfg.Config.PaletteMax = 100
	with := func(path, value string) []SyncSetting {
		cfg := syncTestConfig("print('ok')", 50)
		cfg.Config.PaletteMax = 100
		settings := syncTestSettings(t, cfg)
		for i := range settings {
			if settings[i].Path == path {
				settings[i].Value = value
			}
		}
		return settings
	}
	for value, want := range map[string]int{"-1": 1, "2000000000": 10000} {
		if err := s.beginSessionOverride("replay", nil, with("Config.PaletteMax", value), ""); err != nil {
			t.Fatal(err)
		}
		if s.cfg.Config.PaletteMax != want {
			t.Errorf("PaletteMax %s applied as %d, want %d", value, s.cfg.Config.PaletteMax, want)
		}
		if err := s.endSyncSessionOverride(); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.beginSessionOverride("netplay", nil, with("Options.Time", "not a number"), ""); err == nil {
		t.Fatal("a setting that cannot apply was accepted")
	}
	if s.netplayOverride.Active || s.cfg.Options.Life != 100 {
		t.Fatalf("the failed session changed the game: active %v, Life %v", s.netplayOverride.Active, s.cfg.Options.Life)
	}
}
