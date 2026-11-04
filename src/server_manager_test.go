package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizeServerConfig(t *testing.T) {
	cfg := normalizeServerConfig(IkemenServerConfig{})
	if cfg.BindAddress != "0.0.0.0" || cfg.NakamaHTTPPort != 7350 || cfg.RankedBestOf != 3 || cfg.LobbyMaxGames != 3 || cfg.ServerKey != "defaultkey" {
		t.Fatalf("unexpected normalized config: %+v", cfg)
	}
	if cfg.ConsoleAddress != "127.0.0.1" || cfg.ConsoleUser != "operator" || cfg.NakamaModules != "nakama/modules" {
		t.Fatalf("console and modules defaults: %+v", cfg)
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
	// A server key typed in stays, but the wizard warns about Nakama's own.
	if !strings.Contains(out.String(), `WARNING: server_key in `+resolveServerPath(path)+` is Nakama's default, "defaultkey"`) {
		t.Fatalf("no warning about the default server key: %s", out.String())
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
	if !reflect.DeepEqual(profile.STUNServers, defaultServerSTUNServers) {
		t.Fatalf("client profile STUN servers %v, want %v", profile.STUNServers, defaultServerSTUNServers)
	}
}

// withServerBaseDir makes the server profiles' relative paths start in dir.
func withServerBaseDir(t *testing.T, dir string) {
	old := sys.baseDir
	sys.baseDir = dir
	t.Cleanup(func() { sys.baseDir = old })
}

var randomServerSecret = regexp.MustCompile(`^[0-9a-f]{32}$`)

// checkRandomServerSecrets checks that each of the profile's secrets is
// random: 32 hex digits, and none the same as another.
func checkRandomServerSecrets(t *testing.T, cfg IkemenServerConfig) {
	t.Helper()
	seen := map[string]string{}
	for _, s := range serverSecrets {
		v := *s.field(&cfg)
		if !randomServerSecret.MatchString(v) {
			t.Errorf("%s = %q, want 32 random hex digits", s.name, v)
		}
		if other, ok := seen[v]; ok {
			t.Errorf("%s is the same as %s", s.name, other)
		}
		seen[v] = s.name
	}
}

func sameServerSecrets(a, b IkemenServerConfig) bool {
	for _, s := range serverSecrets {
		if *s.field(&a) != *s.field(&b) {
			return false
		}
	}
	return true
}

// checkPrivateFile checks that only the file's owner can read it.
func checkPrivateFile(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s has mode %o, want 600", path, perm)
	}
}

func TestServerWizardNewProfile(t *testing.T) {
	dir := t.TempDir()
	withServerBaseDir(t, dir)
	var out bytes.Buffer
	// Enter at every prompt: the wizard's defaults.
	if err := runServerWizard(strings.NewReader(""), &out, "save/server.json"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "save", "server.json")
	checkPrivateFile(t, path)
	cfg, err := loadIkemenServerConfig("save/server.json")
	if err != nil {
		t.Fatal(err)
	}
	checkRandomServerSecrets(t, cfg)
	if !randomServerSecret.MatchString(cfg.ServerKey) {
		t.Errorf("server_key %q, want a random one", cfg.ServerKey)
	}
	if cfg.ConsoleAddress != "127.0.0.1" || cfg.ConsoleUser != "operator" || cfg.NakamaModules != "nakama/modules" {
		t.Errorf("console %q user %q modules %q", cfg.ConsoleAddress, cfg.ConsoleUser, cfg.NakamaModules)
	}
	if !cfg.EnableMatchmaking || !cfg.EnableLobbies || !cfg.EnableRanked || !cfg.EnableSpectators || !cfg.EnableChat {
		t.Errorf("a new profile enables every feature: %+v", cfg)
	}
	if !strings.Contains(out.String(), `Nakama's console: http://127.0.0.1:7351, user "operator", password: console_password in `+path) {
		t.Errorf("wizard output does not say where the console is: %s", out.String())
	}
	if strings.Contains(out.String(), "WARNING") {
		t.Errorf("a new profile has nothing to warn about: %s", out.String())
	}

	// The client profile, which a game ships, has the server key and the
	// STUN servers, and none of the secrets.
	clientPath := filepath.Join(dir, "save", "server-client.json")
	data, err := os.ReadFile(clientPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range serverSecrets {
		if strings.Contains(string(data), *s.field(&cfg)) {
			t.Errorf("the client profile holds %s", s.name)
		}
	}
	profiles, err := LoadNakamaServerProfiles(clientPath)
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := profiles.Find("")
	if profile.ServerKey != cfg.ServerKey || profile.Host != "127.0.0.1" || !reflect.DeepEqual(profile.STUNServers, defaultServerSTUNServers) {
		t.Errorf("client profile %+v", profile)
	}
}

func TestServerWizardKeepsExistingProfile(t *testing.T) {
	dir := t.TempDir()
	withServerBaseDir(t, dir)
	if err := runServerWizard(strings.NewReader("My Server\nmy_game\n"), io.Discard, "save/server.json"); err != nil {
		t.Fatal(err)
	}
	first, err := loadIkemenServerConfig("save/server.json")
	if err != nil {
		t.Fatal(err)
	}
	first.DatabaseDSN = "nakama:dbpassword@127.0.0.1:5432/nakama"
	if err := saveIkemenServerConfig("save/server.json", first); err != nil {
		t.Fatal(err)
	}
	// Run again, pressing Enter at every prompt: nothing changes, the server
	// key that the game's copies carry and the database included, and the
	// database's password is not shown.
	var out bytes.Buffer
	if err := runServerWizard(strings.NewReader(""), &out, "save/server.json"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Editing "+filepath.Join(dir, "save", "server.json")) {
		t.Errorf("wizard output does not say it edits the profile: %s", out.String())
	}
	if !strings.Contains(out.String(), "Database DSN [nakama:xxxxx@127.0.0.1:5432/nakama]: ") {
		t.Errorf("wizard output does not show the database without its password: %s", out.String())
	}
	if strings.Contains(out.String(), "dbpassword") {
		t.Errorf("wizard output shows the database's password: %s", out.String())
	}
	second, err := loadIkemenServerConfig("save/server.json")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("profile changed:\n%+v\n%+v", first, second)
	}
}

func TestServerWizardRefusesPublicSecrets(t *testing.T) {
	dir := t.TempDir()
	withServerBaseDir(t, dir)
	path := filepath.Join(dir, "server.json")
	data := []byte(`{"server_key": "mykey", "http_key": "defaulthttpkey", "console_password": "password"}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	err := runServerWizard(strings.NewReader(""), io.Discard, path)
	if err == nil || !strings.Contains(err.Error(), `http_key ("defaulthttpkey")`) || !strings.Contains(err.Error(), `console_password ("password")`) {
		t.Fatalf("err = %v, want Nakama's public secrets refused", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, data) {
		t.Errorf("the profile was changed: %s", after)
	}
}

// oldServerProfile is a profile as the wizard wrote it before profiles had
// Nakama's secrets: readable by all, with Nakama's default server key.
const oldServerProfile = `{
  "version": 1,
  "name": "Old Server",
  "game": "my_game",
  "bind_address": "0.0.0.0",
  "public_address": "server.example.com",
  "server_key": "defaultkey",
  "client_ssl": true,
  "nakama_binary": "nakama/nakama",
  "nakama_config": "nakama/config.yml",
  "working_dir": "",
  "nakama_http_port": 17350,
  "nakama_grpc_port": 17349,
  "nakama_console_port": 17351,
  "enable_matchmaking": true,
  "enable_lobbies": true,
  "enable_ranked": true,
  "enable_spectators": true,
  "enable_chat": true,
  "default_lobby_mode": "queue",
  "ranked_best_of": 3,
  "ranked_switch_sides": true,
  "winner_keeps_selection": true,
  "lobby_max_games": 3
}
`

func writeOldServerProfile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "save", "server.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(oldServerProfile), 0o644); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode is subject to the umask.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServerProfileFillsInOldProfile(t *testing.T) {
	dir := t.TempDir()
	withServerBaseDir(t, dir)
	path := writeOldServerProfile(t, dir)
	var out bytes.Buffer
	cfg, err := prepareServerProfile("save/server.json", &out)
	if err != nil {
		t.Fatal(err)
	}
	checkRandomServerSecrets(t, cfg)
	if cfg.ServerKey != "defaultkey" || cfg.Name != "Old Server" || cfg.NakamaHTTPPort != 17350 {
		t.Errorf("the profile's own settings changed: %+v", cfg)
	}
	if cfg.ConsoleAddress != "127.0.0.1" || cfg.ConsoleUser != "operator" || cfg.NakamaModules != "nakama/modules" {
		t.Errorf("console %q user %q modules %q", cfg.ConsoleAddress, cfg.ConsoleUser, cfg.NakamaModules)
	}
	if !strings.Contains(out.String(), "[SERVER] Added to "+path) ||
		!strings.Contains(out.String(), "http_key, session_key, refresh_key, console_password, console_signing_key, mfa_key, console_address, console_user, nakama_modules") {
		t.Errorf("output does not list what was added: %s", out.String())
	}
	if !strings.Contains(out.String(), `[SERVER] WARNING: server_key in `+path+` is Nakama's default, "defaultkey"`) {
		t.Errorf("no warning about the default server key: %s", out.String())
	}

	// The secrets were saved, privately, so the next start keeps them.
	checkPrivateFile(t, path)
	saved, err := loadIkemenServerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved, cfg) {
		t.Errorf("saved profile differs:\n%+v\n%+v", saved, cfg)
	}
	out.Reset()
	again, err := prepareServerProfile("save/server.json", &out)
	if err != nil {
		t.Fatal(err)
	}
	if !sameServerSecrets(again, cfg) {
		t.Errorf("the secrets changed at the next start")
	}
	if strings.Contains(out.String(), "Added") {
		t.Errorf("nothing is missing the second time: %s", out.String())
	}
}

func TestServerProfileKeepsModulesInNakamaDefault(t *testing.T) {
	dir := t.TempDir()
	withServerBaseDir(t, dir)
	writeOldServerProfile(t, dir)
	// Nakama's own runtime path, data/modules in its working directory, has
	// the modules: the profile keeps them there.
	modules := filepath.Join(dir, "data", "modules")
	if err := os.MkdirAll(modules, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modules, "main.lua"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := prepareServerProfile("save/server.json", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NakamaModules != "data/modules" {
		t.Errorf("nakama_modules = %q, want data/modules", cfg.NakamaModules)
	}
}

func TestServerProfileRefusesPublicSecrets(t *testing.T) {
	for _, s := range serverSecrets {
		t.Run(s.name, func(t *testing.T) {
			dir := t.TempDir()
			withServerBaseDir(t, dir)
			path := filepath.Join(dir, "server.json")
			data, _ := json.Marshal(map[string]string{s.name: s.nakamaDefault})
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := prepareServerProfile(path, io.Discard)
			if err == nil || !strings.Contains(err.Error(), s.name+" ("+`"`+s.nakamaDefault+`"`+")") {
				t.Fatalf("err = %v, want %s refused", err, s.name)
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(after, data) {
				t.Errorf("the profile was changed: %s", after)
			}
		})
	}
}

func TestServerProfileWarnings(t *testing.T) {
	dir := t.TempDir()
	withServerBaseDir(t, dir)
	if err := runServerWizard(strings.NewReader(""), io.Discard, "server.json"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "server.json")
	cfg, err := loadIkemenServerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConsoleAddress = "0.0.0.0"
	if err := saveIkemenServerConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if _, err := prepareServerProfile(path, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "[SERVER] WARNING: Nakama's console listens on 0.0.0.0") {
		t.Errorf("no warning about the console: %s", out.String())
	}
	if runtime.GOOS != "windows" && !strings.Contains(out.String(), "chmod 600 "+path) {
		t.Errorf("no warning about the profile others can read: %s", out.String())
	}
	if strings.Contains(out.String(), "server_key") {
		t.Errorf("the wizard's random server key needs no warning: %s", out.String())
	}
}

// testServerConfig is a complete profile with secrets of its own.
func testServerConfig() IkemenServerConfig {
	return normalizeServerConfig(IkemenServerConfig{
		ServerKey:         "serverkey",
		HTTPKey:           "httpkey",
		SessionKey:        "sessionkey",
		RefreshKey:        "refreshkey",
		ConsolePassword:   "consolepassword",
		ConsoleSigningKey: "signingkey",
		MFAKey:            "mfakey-mfakey-mfakey-mfakey-mfak",
		NakamaHTTPPort:    17350,
		NakamaConsolePort: 17351,
	})
}

func TestNakamaServerArgs(t *testing.T) {
	cfg := testServerConfig()
	cfg.BindAddress = "::1"
	cfg.DatabaseDSN = "nakama:dbpassword@127.0.0.1:5432/nakama"
	cfg.ExtraArgs = []string{"--logger.level", "DEBUG"}
	args, migrateArgs := nakamaServerArgs(cfg, "/srv/save/server-nakama.yml", "/srv/nakama/config.yml", "/srv/save/server-nakama-database.yml")
	want := []string{
		"--config", "/srv/save/server-nakama.yml",
		"--config", "/srv/nakama/config.yml",
		"--config", "/srv/save/server-nakama-database.yml",
		"--socket.port", "17350",
		"--socket.server_key", "serverkey",
		"--console.port", "17351",
		"--socket.max_message_size_bytes", "65536",
		"--socket.address", "[::1]",
		"--logger.level", "DEBUG",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args\n%q\nwant\n%q", args, want)
	}
	wantMigrate := []string{
		"migrate", "up",
		"--config", "/srv/save/server-nakama.yml",
		"--config", "/srv/nakama/config.yml",
		"--config", "/srv/save/server-nakama-database.yml",
	}
	if !reflect.DeepEqual(migrateArgs, wantMigrate) {
		t.Errorf("migrate args\n%q\nwant\n%q", migrateArgs, wantMigrate)
	}
	// The secrets and the database's password go in files, never on the
	// command line.
	for _, arg := range append(args, migrateArgs...) {
		if strings.Contains(arg, "dbpassword") {
			t.Errorf("argument %q holds the database's password", arg)
		}
		for _, s := range serverSecrets {
			if strings.Contains(arg, *s.field(&cfg)) {
				t.Errorf("argument %q holds %s", arg, s.name)
			}
		}
	}

	// No config file of the host's, every address, and no database.
	cfg.BindAddress, cfg.DatabaseDSN, cfg.ExtraArgs = "0.0.0.0", "", nil
	args, migrateArgs = nakamaServerArgs(cfg, "/srv/save/server-nakama.yml", "", "")
	want = []string{
		"--config", "/srv/save/server-nakama.yml",
		"--socket.port", "17350",
		"--socket.server_key", "serverkey",
		"--console.port", "17351",
		"--socket.max_message_size_bytes", "65536",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args\n%q\nwant\n%q", args, want)
	}
	if want := []string{"migrate", "up", "--config", "/srv/save/server-nakama.yml"}; !reflect.DeepEqual(migrateArgs, want) {
		t.Errorf("migrate args %q, want %q", migrateArgs, want)
	}
}

func TestNakamaServerYAML(t *testing.T) {
	cfg := testServerConfig()
	cfg.ConsoleAddress = "::1"
	got := string(nakamaServerYAML(cfg, `C:\Games\My "Game"\nakama\modules`))
	want := `# Nakama's settings from an IKEMEN GO server profile, written by Ikemen_GO -server
# each time it starts Nakama: change the profile, not this file.
runtime:
  path: "C:\\Games\\My \"Game\"\\nakama\\modules"
  http_key: "httpkey"
session:
  encryption_key: "sessionkey"
  refresh_encryption_key: "refreshkey"
mfa:
  storage_encryption_key: "mfakey-mfakey-mfakey-mfakey-mfak"
console:
  address: "[::1]"
  username: "operator"
  password: "consolepassword"
  signing_key: "signingkey"
  mfa:
    storage_encryption_key: "mfakey-mfakey-mfakey-mfakey-mfak"
`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestNakamaDatabaseYAML(t *testing.T) {
	got := string(nakamaDatabaseYAML("nakama:p%40ss@db.example.com:5432/nakama?sslmode=verify-full"))
	want := `# The database of an IKEMEN GO server profile (database_dsn), written by Ikemen_GO -server
# each time it starts Nakama: change the profile, not this file.
database:
  address:
    - "nakama:p%40ss@db.example.com:5432/nakama?sslmode=verify-full"
`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestYAMLQuote(t *testing.T) {
	for _, s := range []string{"", "plain", `back\slash "quote"`, "line\nbreak\ttab", "del\x7f", "nel\u0085c1\u009f", "\ufffe\uffff", "é ü 漢"} {
		q := yamlQuote(s)
		for _, r := range q {
			if r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0xfffe || r == 0xffff || r < 0x20 {
				t.Errorf("yamlQuote(%q) = %q holds %U, which YAML refuses", s, q, r)
			}
		}
		// The quoted text is a JSON string too, with the same meaning.
		var back string
		if err := json.Unmarshal([]byte(q), &back); err != nil || back != s {
			t.Errorf("yamlQuote(%q) = %q reads back as %q (%v)", s, q, back, err)
		}
	}
}

func TestRedactDatabaseAddress(t *testing.T) {
	for _, c := range []struct{ address, shown string }{
		{"", ""},
		{"nakama@127.0.0.1:5432/nakama", "nakama@127.0.0.1:5432/nakama"},
		{"nakama:dbpassword@127.0.0.1:5432/nakama", "nakama:xxxxx@127.0.0.1:5432/nakama"},
		{"nakama:db@password@127.0.0.1:5432/nakama?sslmode=disable", "nakama:xxxxx@127.0.0.1:5432/nakama?sslmode=disable"},
		{"postgresql://nakama:dbpassword@db.example.com/nakama", "postgresql://nakama:xxxxx@db.example.com/nakama"},
		{"nakama@127.0.0.1/nakama?sslmode=disable&password=dbpassword", "nakama@127.0.0.1/nakama?password=xxxxx&sslmode=disable"},
		{"nakama@127.0.0.1/nakama?sslpassword=dbpassword", "nakama@127.0.0.1/nakama?sslpassword=xxxxx"},
		// Not a URL: the / in the password ends its host.
		{"nakama:db/password@127.0.0.1/nakama", "(hidden)"},
	} {
		if got := redactDatabaseAddress(c.address); got != c.shown {
			t.Errorf("redactDatabaseAddress(%q) = %q, want %q", c.address, got, c.shown)
		}
	}
}

func TestPrintableNakamaArgs(t *testing.T) {
	args := []string{
		"--config", "/srv/save/server-nakama.yml",
		"--database.address", "nakama:dbpassword@127.0.0.1:5432/nakama",
		"--socket.server_key", "serverkey",
		"--runtime.http_key", "httpsecret",
		"--console.password=consolesecret",
		"-session.encryption_key", "sessionsecret",
	}
	got := strings.Join(printableNakamaArgs(args), " ")
	for _, secret := range []string{"dbpassword", "httpsecret", "consolesecret", "sessionsecret"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q shows %q", got, secret)
		}
	}
	for _, shown := range []string{"--database.address nakama:xxxxx@127.0.0.1:5432/nakama", "--socket.server_key serverkey", "--config /srv/save/server-nakama.yml"} {
		if !strings.Contains(got, shown) {
			t.Errorf("%q does not show %q", got, shown)
		}
	}
	if args[3] != "nakama:dbpassword@127.0.0.1:5432/nakama" {
		t.Errorf("the arguments themselves changed: %q", args)
	}
}

// TestRunNakamaServerStartsNakama runs -server's launch of an old profile
// with a stand-in for Nakama that records its arguments: nothing listens.
func TestRunNakamaServerStartsNakama(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in for Nakama is a shell script")
	}
	dir := t.TempDir()
	withServerBaseDir(t, dir)
	modules := filepath.Join(dir, "nakama", "modules")
	if err := os.MkdirAll(modules, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modules, "main.lua"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "nakama-args")
	stand := "#!/bin/sh\n{ printf '%s\\n' \"$@\"; echo ---; } >> \"$IKEMEN_TEST_NAKAMA_ARGS\"\n"
	if err := os.WriteFile(filepath.Join(dir, "nakama", "nakama"), []byte(stand), 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeOldServerProfile(t, dir)
	cfg, err := prepareServerProfile(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Environment = []string{"IKEMEN_TEST_NAKAMA_ARGS=" + record}
	// runNakama starts Nakama and returns the arguments of its two runs.
	runNakama := func(cfg IkemenServerConfig) (migrate, server string) {
		t.Helper()
		if err := os.Remove(record); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if err := runNakamaServer(context.Background(), path, cfg); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(record)
		if err != nil {
			t.Fatal(err)
		}
		runs := strings.Split(strings.TrimSuffix(string(data), "---\n"), "---\n")
		if len(runs) != 2 {
			t.Fatalf("Nakama ran %d times, want 2 (migrate, then the server): %q", len(runs), data)
		}
		return runs[0], runs[1]
	}

	migrateRun, serverRun := runNakama(cfg)
	settings := filepath.Join(dir, "save", "server-nakama.yml")
	wantMigrate := "migrate\nup\n--config\n" + settings + "\n"
	wantServer := "--config\n" + settings + "\n--socket.port\n17350\n--socket.server_key\ndefaultkey\n--console.port\n17351\n--socket.max_message_size_bytes\n65536\n"
	if migrateRun != wantMigrate {
		t.Errorf("migrate ran with\n%s\nwant\n%s", migrateRun, wantMigrate)
	}
	if serverRun != wantServer {
		t.Errorf("Nakama ran with\n%s\nwant\n%s", serverRun, wantServer)
	}

	checkPrivateFile(t, settings)
	yaml, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if want := string(nakamaServerYAML(cfg, modules)); string(yaml) != want {
		t.Errorf("settings file\n%s\nwant\n%s", yaml, want)
	}
	for _, line := range []string{"  path: " + yamlQuote(modules), `  address: "127.0.0.1"`, `  http_key: "` + cfg.HTTPKey + `"`} {
		if !strings.Contains(string(yaml), line+"\n") {
			t.Errorf("settings file lacks %q:\n%s", line, yaml)
		}
	}
	database := filepath.Join(dir, "save", "server-nakama-database.yml")
	if _, err := os.Stat(database); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a profile without a database got %s (%v)", database, err)
	}

	// The profile's database comes in a file of its own, after the others,
	// and its password stays off Nakama's command line.
	cfg.DatabaseDSN = "nakama:dbpassword@127.0.0.1:5432/nakama"
	migrateRun, serverRun = runNakama(cfg)
	if want := "migrate\nup\n--config\n" + settings + "\n--config\n" + database + "\n"; migrateRun != want {
		t.Errorf("migrate ran with\n%s\nwant\n%s", migrateRun, want)
	}
	if want := "--config\n" + settings + "\n--config\n" + database + strings.TrimPrefix(wantServer, "--config\n"+settings); serverRun != want {
		t.Errorf("Nakama ran with\n%s\nwant\n%s", serverRun, want)
	}
	if strings.Contains(migrateRun+serverRun, "dbpassword") {
		t.Errorf("Nakama's command line holds the database's password:\n%s%s", migrateRun, serverRun)
	}
	checkPrivateFile(t, database)
	if data, err := os.ReadFile(database); err != nil || string(data) != string(nakamaDatabaseYAML(cfg.DatabaseDSN)) {
		t.Errorf("database file %q (%v), want %q", data, err, nakamaDatabaseYAML(cfg.DatabaseDSN))
	}

	// A profile that drops its database leaves no password behind.
	cfg.DatabaseDSN = ""
	if _, serverRun = runNakama(cfg); serverRun != wantServer {
		t.Errorf("Nakama ran with\n%s\nwant\n%s", serverRun, wantServer)
	}
	if _, err := os.Stat(database); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s stayed after the profile dropped its database (%v)", database, err)
	}
}
