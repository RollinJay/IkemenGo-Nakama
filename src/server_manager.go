package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const defaultNakamaServerConfigPath = "save/server.json"

const (
	// defaultNakamaModules is the folder of this fork's server modules, which
	// the release packages ship next to the game (Nakama's runtime.path).
	defaultNakamaModules = "nakama/modules"
	// nakamaDefaultServerKey is the server key of a Nakama that sets none.
	nakamaDefaultServerKey = "defaultkey"
)

// defaultServerSTUNServers are the STUN servers of the wizard's client
// profile, as in data/online/servers.json: a game learns its public address
// from them, which a direct connection between players behind routers needs.
var defaultServerSTUNServers = []string{"stun.l.google.com:19302", "stun.cloudflare.com:3478"}

type IkemenServerConfig struct {
	Version           int      `json:"version"`
	Name              string   `json:"name"`
	Game              string   `json:"game"`
	BindAddress       string   `json:"bind_address"`
	PublicAddress     string   `json:"public_address"`
	ServerKey         string   `json:"server_key"`
	ClientSSL         bool     `json:"client_ssl"`
	NakamaBinary      string   `json:"nakama_binary"`
	NakamaConfig      string   `json:"nakama_config"`
	NakamaModules     string   `json:"nakama_modules"`
	WorkingDir        string   `json:"working_dir"`
	ExtraArgs         []string `json:"extra_args,omitempty"`
	Environment       []string `json:"environment,omitempty"`
	LogFile           string   `json:"log_file,omitempty"`
	DatabaseDSN       string   `json:"database_dsn,omitempty"`
	NakamaHTTPPort    int      `json:"nakama_http_port"`
	NakamaGRPCPort    int      `json:"nakama_grpc_port"`
	NakamaConsolePort int      `json:"nakama_console_port"`
	ConsoleAddress    string   `json:"console_address"`
	ConsoleUser       string   `json:"console_user"`
	// The server's secrets (serverSecrets).
	ConsolePassword      string `json:"console_password"`
	ConsoleSigningKey    string `json:"console_signing_key"`
	HTTPKey              string `json:"http_key"`
	SessionKey           string `json:"session_key"`
	RefreshKey           string `json:"refresh_key"`
	MFAKey               string `json:"mfa_key"`
	EnableMatchmaking    bool   `json:"enable_matchmaking"`
	EnableLobbies        bool   `json:"enable_lobbies"`
	EnableRanked         bool   `json:"enable_ranked"`
	EnableSpectators     bool   `json:"enable_spectators"`
	EnableChat           bool   `json:"enable_chat"`
	DefaultLobbyMode     string `json:"default_lobby_mode"`
	RankedBestOf         int    `json:"ranked_best_of"`
	RankedSwitchSides    bool   `json:"ranked_switch_sides"`
	WinnerKeepsSelection bool   `json:"winner_keeps_selection"`
	LobbyMaxGames        int    `json:"lobby_max_games"`
}

// serverSecrets are the profile's secrets, with Nakama's own default for
// each. The defaults are public: a server that keeps one lets anyone forge its
// players' sessions (session_key, refresh_key), call its RPCs as the server,
// the moderators' among them (http_key), or sign in to its console.
var serverSecrets = []struct {
	name, nakamaDefault string
	field               func(*IkemenServerConfig) *string
}{
	{"http_key", "defaulthttpkey", func(c *IkemenServerConfig) *string { return &c.HTTPKey }},
	{"session_key", "defaultencryptionkey", func(c *IkemenServerConfig) *string { return &c.SessionKey }},
	{"refresh_key", "defaultrefreshencryptionkey", func(c *IkemenServerConfig) *string { return &c.RefreshKey }},
	{"console_password", "password", func(c *IkemenServerConfig) *string { return &c.ConsolePassword }},
	{"console_signing_key", "defaultsigningkey", func(c *IkemenServerConfig) *string { return &c.ConsoleSigningKey }},
	{"mfa_key", "the-key-has-to-be-32-bytes-long!", func(c *IkemenServerConfig) *string { return &c.MFAKey }},
}

func normalizeServerConfig(c IkemenServerConfig) IkemenServerConfig {
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Name == "" {
		c.Name = "IKEMEN GO Server"
	}
	if c.BindAddress == "" {
		c.BindAddress = "0.0.0.0"
	}
	if c.ServerKey == "" {
		c.ServerKey = nakamaDefaultServerKey
	}
	if c.NakamaBinary == "" {
		if runtime.GOOS == "windows" {
			c.NakamaBinary = "nakama/nakama.exe"
		} else {
			c.NakamaBinary = "nakama/nakama"
		}
	}
	if c.NakamaConfig == "" {
		c.NakamaConfig = "nakama/config.yml"
	}
	if c.NakamaModules == "" {
		c.NakamaModules = defaultNakamaModules
	}
	if c.NakamaHTTPPort == 0 {
		c.NakamaHTTPPort = 7350
	}
	if c.NakamaGRPCPort == 0 {
		c.NakamaGRPCPort = 7349
	}
	if c.NakamaConsolePort == 0 {
		c.NakamaConsolePort = 7351
	}
	if c.ConsoleAddress == "" {
		c.ConsoleAddress = "127.0.0.1"
	}
	if c.ConsoleUser == "" {
		c.ConsoleUser = "operator"
	}
	if c.DefaultLobbyMode == "" {
		c.DefaultLobbyMode = "queue"
	}
	if c.DefaultLobbyMode != "queue" && c.DefaultLobbyMode != "winner_stays_on" && c.DefaultLobbyMode != "round_robin" && c.DefaultLobbyMode != "bracket" {
		c.DefaultLobbyMode = "queue"
	}
	if c.RankedBestOf <= 0 {
		c.RankedBestOf = 3
	}
	if c.RankedBestOf > 99 {
		c.RankedBestOf = 99
	}
	if c.RankedBestOf%2 == 0 {
		c.RankedBestOf--
	}
	if c.RankedBestOf < 1 {
		c.RankedBestOf = 1
	}
	if c.LobbyMaxGames <= 0 {
		c.LobbyMaxGames = 3
	}
	if c.LobbyMaxGames > 99 {
		c.LobbyMaxGames = 99
	}
	return c
}

// newServerSecret returns 16 random bytes in hex: 32 characters, the length
// Nakama's MFA key must have. crypto/rand.Read never fails.
func newServerSecret() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// workDir is the folder Nakama runs in.
func (c IkemenServerConfig) workDir() string {
	if c.WorkingDir != "" {
		return c.WorkingDir
	}
	return sys.baseDir
}

// fillServerProfile gives a profile the Nakama settings that profiles written
// before them lack: a random value for each secret, and the server modules'
// folder. normalizeServerConfig gives the others.
func fillServerProfile(c *IkemenServerConfig) {
	for _, s := range serverSecrets {
		if p := s.field(c); *p == "" {
			*p = newServerSecret()
		}
	}
	// Without runtime.path, Nakama loaded the modules from its own default,
	// data/modules in its working directory: a host who installed them there
	// keeps them.
	if c.NakamaModules == "" {
		if _, err := os.Stat(filepath.Join(c.workDir(), "data", "modules", "main.lua")); err == nil {
			c.NakamaModules = "data/modules"
		}
	}
}

// checkServerSecrets refuses a profile that sets a secret to Nakama's public
// default.
func checkServerSecrets(profile string, c IkemenServerConfig) error {
	var public []string
	for _, s := range serverSecrets {
		if *s.field(&c) == s.nakamaDefault {
			public = append(public, fmt.Sprintf("%s (%q)", s.name, s.nakamaDefault))
		}
	}
	if len(public) == 0 {
		return nil
	}
	return fmt.Errorf("%s keeps Nakama's public defaults, which let anyone act as the server: %s; delete them, and a random one is made for each, or set secrets of your own", profile, strings.Join(public, ", "))
}

// serverProfileWarnings are the wizard's and -server's warnings about a
// profile: Nakama's default server key, and a console other computers reach.
func serverProfileWarnings(profile string, c IkemenServerConfig) []string {
	var warnings []string
	if c.ServerKey == nakamaDefaultServerKey {
		warnings = append(warnings, fmt.Sprintf("server_key in %s is Nakama's default, %q. A server key is not a secret (every copy of the game carries it), but a server should have its own: set server_key there to a new one, such as %s, and the same in your game's data/online/servers.json.", profile, nakamaDefaultServerKey, newServerSecret()))
	}
	if !isLoopbackAddress(c.ConsoleAddress) {
		warnings = append(warnings, fmt.Sprintf("Nakama's console listens on %s (console_address in %s), where other computers can reach it, and whoever signs in to it controls the server. Leave it on 127.0.0.1, and reach it from elsewhere through an SSH tunnel.", c.ConsoleAddress, profile))
	}
	return warnings
}

// serverConsoleSummary says where Nakama's console is and how to sign in.
func serverConsoleSummary(profile string, c IkemenServerConfig) string {
	address := net.JoinHostPort(strings.Trim(c.ConsoleAddress, "[]"), strconv.Itoa(c.NakamaConsolePort))
	return fmt.Sprintf("Nakama's console: http://%s, user %q, password: console_password in %s", address, c.ConsoleUser, profile)
}

func isLoopbackAddress(address string) bool {
	address = strings.Trim(address, "[]")
	if strings.EqualFold(address, "localhost") {
		return true
	}
	ip := net.ParseIP(address)
	return ip != nil && ip.IsLoopback()
}

func resolveServerPath(path string) string {
	if strings.TrimSpace(path) == "" {
		path = defaultNakamaServerConfigPath
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(sys.baseDir, path)
}

// absServerPath is path made absolute, for Nakama, which runs in its own
// working directory.
func absServerPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// readIkemenServerConfig reads a profile without normalizeServerConfig's
// defaults, so that a caller can tell what it lacks.
func readIkemenServerConfig(path string) (IkemenServerConfig, error) {
	resolved := resolveServerPath(path)
	data, err := os.ReadFile(resolved)
	if err != nil {
		return IkemenServerConfig{}, fmt.Errorf("read server config %q: %w", resolved, err)
	}
	var cfg IkemenServerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return IkemenServerConfig{}, fmt.Errorf("parse server config %q: %w", resolved, err)
	}
	return cfg, nil
}

func loadIkemenServerConfig(path string) (IkemenServerConfig, error) {
	cfg, err := readIkemenServerConfig(path)
	if err != nil {
		return IkemenServerConfig{}, err
	}
	return normalizeServerConfig(cfg), nil
}

func saveIkemenServerConfig(path string, cfg IkemenServerConfig) error {
	resolved := resolveServerPath(path)
	if err := os.MkdirAll(filepath.Dir(resolved), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(normalizeServerConfig(cfg), "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writePrivateFile(resolved, data)
}

// writePrivateFile replaces path with a file that only its owner can read
// (mode 0600): a new file renamed over the old one, so that the secrets are
// never readable by others, not even while they are written, nor left in an
// older file that was.
func writePrivateFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	// After the rename there is nothing left to remove.
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func promptLine(in *bufio.Reader, out io.Writer, label, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(out, "%s: ", label)
	}
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

func promptBool(in *bufio.Reader, out io.Writer, label string, def bool) (bool, error) {
	defText := "n"
	if def {
		defText = "y"
	}
	value, err := promptLine(in, out, label+" (y/n)", defText)
	if err != nil {
		return false, err
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "y"), nil
}

func promptInt(in *bufio.Reader, out io.Writer, label string, def int) (int, error) {
	value, err := promptLine(in, out, label, strconv.Itoa(def))
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid integer for %s: %w", label, err)
	}
	return n, nil
}

func runServerWizard(in io.Reader, out io.Writer, path string) error {
	reader := bufio.NewReader(in)
	resolved := resolveServerPath(path)
	// A profile that exists is edited: its secrets stay, and so does its
	// server key, which the copies of the game carry.
	cfg, err := readIkemenServerConfig(path)
	editing := err == nil
	switch {
	case editing:
		if err := checkServerSecrets(resolved, cfg); err != nil {
			return err
		}
	case errors.Is(err, os.ErrNotExist):
		cfg = IkemenServerConfig{
			ServerKey:         newServerSecret(),
			NakamaModules:     defaultNakamaModules,
			EnableMatchmaking: true,
			EnableLobbies:     true,
			EnableRanked:      true,
			EnableSpectators:  true,
			EnableChat:        true,
		}
	default:
		return err
	}
	fillServerProfile(&cfg)
	cfg = normalizeServerConfig(cfg)
	fmt.Fprintln(out, "IKEMEN GO ONLINE SERVER WIZARD")
	fmt.Fprintln(out, "This headless wizard creates an optional dedicated Nakama launcher profile.")
	if editing {
		fmt.Fprintf(out, "Editing %s: press Enter to keep a value.\n", resolved)
	}
	fmt.Fprintln(out)
	if cfg.Name, err = promptLine(reader, out, "Server name", cfg.Name); err != nil {
		return err
	}
	if cfg.Game, err = promptLine(reader, out, "Game identifier", cfg.Game); err != nil {
		return err
	}
	if cfg.NakamaBinary, err = promptLine(reader, out, "Nakama executable path", cfg.NakamaBinary); err != nil {
		return err
	}
	if cfg.NakamaConfig, err = promptLine(reader, out, "Nakama config path", cfg.NakamaConfig); err != nil {
		return err
	}
	if cfg.WorkingDir, err = promptLine(reader, out, "Nakama working directory", cfg.WorkingDir); err != nil {
		return err
	}
	if cfg.BindAddress, err = promptLine(reader, out, "Bind address", cfg.BindAddress); err != nil {
		return err
	}
	if cfg.PublicAddress, err = promptLine(reader, out, "Public address (optional)", cfg.PublicAddress); err != nil {
		return err
	}
	if cfg.ServerKey, err = promptLine(reader, out, "Nakama server key", cfg.ServerKey); err != nil {
		return err
	}
	if cfg.ClientSSL, err = promptBool(reader, out, "Client uses TLS", cfg.ClientSSL); err != nil {
		return err
	}
	if cfg.NakamaHTTPPort, err = promptInt(reader, out, "Nakama HTTP/socket port", cfg.NakamaHTTPPort); err != nil {
		return err
	}
	if cfg.NakamaGRPCPort, err = promptInt(reader, out, "Nakama gRPC port", cfg.NakamaGRPCPort); err != nil {
		return err
	}
	if cfg.NakamaConsolePort, err = promptInt(reader, out, "Nakama console port", cfg.NakamaConsolePort); err != nil {
		return err
	}
	if cfg.EnableMatchmaking, err = promptBool(reader, out, "Enable matchmaking", cfg.EnableMatchmaking); err != nil {
		return err
	}
	if cfg.EnableLobbies, err = promptBool(reader, out, "Enable lobbies", cfg.EnableLobbies); err != nil {
		return err
	}
	if cfg.EnableRanked, err = promptBool(reader, out, "Enable ranked", cfg.EnableRanked); err != nil {
		return err
	}
	if cfg.EnableSpectators, err = promptBool(reader, out, "Enable spectators", cfg.EnableSpectators); err != nil {
		return err
	}
	if cfg.EnableChat, err = promptBool(reader, out, "Enable chat", cfg.EnableChat); err != nil {
		return err
	}
	if cfg.RankedBestOf, err = promptInt(reader, out, "Ranked best-of", cfg.RankedBestOf); err != nil {
		return err
	}
	if cfg.RankedSwitchSides, err = promptBool(reader, out, "Ranked side switching", cfg.RankedSwitchSides); err != nil {
		return err
	}
	if cfg.WinnerKeepsSelection, err = promptBool(reader, out, "Winner keeps character/team", cfg.WinnerKeepsSelection); err != nil {
		return err
	}
	if cfg.DefaultLobbyMode, err = promptLine(reader, out, "Default lobby mode", cfg.DefaultLobbyMode); err != nil {
		return err
	}
	if cfg.LobbyMaxGames, err = promptInt(reader, out, "Winner-stays max games", cfg.LobbyMaxGames); err != nil {
		return err
	}
	if cfg.DatabaseDSN, err = promptLine(reader, out, "Database DSN", cfg.DatabaseDSN); err != nil {
		return err
	}
	if cfg.LogFile, err = promptLine(reader, out, "Server log file", cfg.LogFile); err != nil {
		return err
	}
	cfg = normalizeServerConfig(cfg)
	if err := saveIkemenServerConfig(path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nSaved server configuration to %s: it holds the server's secrets, so keep it private and out of the game you ship\n", resolved)
	profilePath := strings.TrimSuffix(path, filepath.Ext(path)) + "-client.json"
	if err := saveNakamaClientProfile(profilePath, cfg); err != nil {
		return err
	}
	fmt.Fprintf(out, "Saved client connection profile to %s\n", resolveServerPath(profilePath))
	fmt.Fprintln(out, serverConsoleSummary(resolved, cfg))
	fmt.Fprintln(out, "Use: Ikemen_GO -server -server-config", path)
	for _, w := range serverProfileWarnings(resolved, cfg) {
		fmt.Fprintln(out, "WARNING:", w)
	}
	return nil
}

func saveNakamaClientProfile(path string, cfg IkemenServerConfig) error {
	host := strings.TrimSpace(cfg.PublicAddress)
	if host == "" {
		host = strings.TrimSpace(cfg.BindAddress)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	profile := NakamaServerProfile{
		ID:          "dedicated",
		Name:        cfg.Name,
		Host:        host,
		Port:        cfg.NakamaHTTPPort,
		ServerKey:   cfg.ServerKey,
		SSL:         cfg.ClientSSL,
		Status:      true,
		Game:        cfg.Game,
		Build:       Version,
		STUNServers: append([]string(nil), defaultServerSTUNServers...),
	}
	profiles := NakamaServerProfiles{Version: 1, Default: profile.ID, Servers: []NakamaServerProfile{profile}}
	resolved := resolveServerPath(path)
	if err := os.MkdirAll(filepath.Dir(resolved), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(profiles, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(resolved, append(data, '\n'), 0644)
}

func findNakamaBinary(config IkemenServerConfig) string {
	if config.NakamaBinary != "" {
		path := config.NakamaBinary
		if !filepath.IsAbs(path) {
			path = filepath.Join(sys.baseDir, path)
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
		if resolved, err := exec.LookPath(config.NakamaBinary); err == nil {
			return resolved
		}
	}
	candidates := []string{}
	if runtime.GOOS == "windows" {
		candidates = []string{"nakama/nakama.exe", "nakama.exe"}
	} else {
		candidates = []string{"nakama/nakama", "nakama"}
	}
	for _, candidate := range candidates {
		resolved := candidate
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(sys.baseDir, resolved)
		}
		if info, err := os.Stat(resolved); err == nil && !info.IsDir() {
			return resolved
		}
		if resolvedPath, err := exec.LookPath(candidate); err == nil {
			return resolvedPath
		}
	}
	return ""
}

// nakamaSettingsPath is the settings file -server writes for Nakama next to
// the profile: save/server-nakama.yml for save/server.json.
func nakamaSettingsPath(profile string) string {
	return strings.TrimSuffix(profile, filepath.Ext(profile)) + "-nakama.yml"
}

// nakamaServerYAML is the settings file -server hands Nakama first: the
// profile's secrets, which stay off Nakama's command line (other programs on
// the computer can read it), the console's address and user, and the
// modules' folder.
func nakamaServerYAML(c IkemenServerConfig, modules string) []byte {
	var b strings.Builder
	b.WriteString("# Nakama's settings from an IKEMEN GO server profile, written by Ikemen_GO -server\n")
	b.WriteString("# each time it starts Nakama: change the profile, not this file.\n")
	fmt.Fprintf(&b, "runtime:\n  path: %s\n  http_key: %s\n", yamlQuote(modules), yamlQuote(c.HTTPKey))
	fmt.Fprintf(&b, "session:\n  encryption_key: %s\n  refresh_encryption_key: %s\n", yamlQuote(c.SessionKey), yamlQuote(c.RefreshKey))
	fmt.Fprintf(&b, "mfa:\n  storage_encryption_key: %s\n", yamlQuote(c.MFAKey))
	fmt.Fprintf(&b, "console:\n  address: %s\n  username: %s\n  password: %s\n  signing_key: %s\n",
		yamlQuote(nakamaListenAddress(c.ConsoleAddress)), yamlQuote(c.ConsoleUser), yamlQuote(c.ConsolePassword), yamlQuote(c.ConsoleSigningKey))
	fmt.Fprintf(&b, "  mfa:\n    storage_encryption_key: %s\n", yamlQuote(c.MFAKey))
	return []byte(b.String())
}

// yamlQuote quotes s for a YAML file. A JSON string is a YAML double-quoted
// scalar with the same escapes, but YAML refuses a few characters that JSON
// leaves as they are (DEL, the C1 controls and the noncharacters U+FFFE and
// U+FFFF), so those go in as escapes too.
func yamlQuote(s string) string {
	j, _ := json.Marshal(s)
	var b strings.Builder
	for _, r := range string(j) {
		if r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0xfffe || r == 0xffff {
			fmt.Fprintf(&b, `\u%04x`, r)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// nakamaListenAddress is an address as Nakama listens on it: Nakama joins the
// address and the port with ':', so an IPv6 address goes in brackets.
func nakamaListenAddress(address string) string {
	if strings.Contains(address, ":") && net.ParseIP(address) != nil {
		return "[" + address + "]"
	}
	return address
}

// nakamaServerArgs returns the arguments -server starts Nakama with, and
// those of the database migration before it. The settings file -server
// writes (nakamaServerYAML) comes first, and the host's own Nakama config
// file after it overrides it. The profile's database, ports, server key and
// address come as flags, which override both, so that the wizard's client
// profile agrees with the running server; extra_args come last so that a
// host can still override any of them.
func nakamaServerArgs(c IkemenServerConfig, settingsPath, configPath string) (args, migrateArgs []string) {
	args = []string{"--config", settingsPath}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	if c.DatabaseDSN != "" {
		args = append(args, "--database.address", c.DatabaseDSN)
	}
	migrateArgs = append([]string{"migrate", "up"}, args...)
	args = append(args,
		"--socket.port", strconv.Itoa(c.NakamaHTTPPort),
		"--socket.server_key", c.ServerKey,
		"--console.port", strconv.Itoa(c.NakamaConsolePort),
		// IKEMEN's replay header exceeds Nakama's 4 KiB default message limit.
		"--socket.max_message_size_bytes", "65536",
	)
	if c.BindAddress != "" && c.BindAddress != "0.0.0.0" {
		args = append(args, "--socket.address", nakamaListenAddress(c.BindAddress))
	}
	return append(args, c.ExtraArgs...), migrateArgs
}

// printableNakamaArgs are Nakama's arguments for the line -server prints,
// without the database's password and the secrets that extra_args may set.
func printableNakamaArgs(args []string) []string {
	hide := func(flag, value string) string {
		switch strings.TrimLeft(flag, "-") {
		case "database.address":
			if u, err := url.Parse("postgres://" + value); err == nil {
				return strings.TrimPrefix(u.Redacted(), "postgres://")
			}
			return "(hidden)"
		case "runtime.http_key", "session.encryption_key", "session.refresh_encryption_key", "console.password",
			"console.signing_key", "mfa.storage_encryption_key", "console.mfa.storage_encryption_key":
			return "(hidden)"
		}
		return value
	}
	out := append([]string(nil), args...)
	for i, arg := range out {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		if flag, value, ok := strings.Cut(arg, "="); ok {
			out[i] = flag + "=" + hide(flag, value)
		} else if i+1 < len(out) {
			out[i+1] = hide(arg, out[i+1])
		}
	}
	return out
}

// runNakamaServer starts Nakama with the profile at path profile, which
// prepareServerProfile has read.
func runNakamaServer(ctx context.Context, profile string, cfg IkemenServerConfig) error {
	cfg = normalizeServerConfig(cfg)
	binary := findNakamaBinary(cfg)
	if binary == "" {
		return errors.New("Nakama executable not found; set nakama_binary in the server config")
	}
	binary = absServerPath(binary)
	workDir := cfg.workDir()
	configPath := ""
	if cfg.NakamaConfig != "" {
		configPath = cfg.NakamaConfig
		if !filepath.IsAbs(configPath) {
			configPath = filepath.Join(workDir, configPath)
		}
		// Nakama runs fine without a YAML file of the host's own; the wizard
		// only suggests a path.
		if _, err := os.Stat(configPath); err != nil {
			fmt.Fprintf(os.Stdout, "[SERVER] %s not found; starting Nakama with the profile's settings only\n", configPath)
			configPath = ""
		} else {
			configPath = absServerPath(configPath)
		}
	}
	modules := cfg.NakamaModules
	if !filepath.IsAbs(modules) {
		modules = filepath.Join(workDir, modules)
	}
	modules = absServerPath(modules)
	if _, err := os.Stat(filepath.Join(modules, "main.lua")); err != nil {
		fmt.Fprintf(os.Stdout, "[SERVER] WARNING: %s has no main.lua, so Nakama starts without this fork's server modules, which online play needs (nakama_modules in %s)\n", modules, profile)
	}
	settingsPath := absServerPath(nakamaSettingsPath(profile))
	if err := writePrivateFile(settingsPath, nakamaServerYAML(cfg, modules)); err != nil {
		return err
	}
	args, migrateArgs := nakamaServerArgs(cfg, settingsPath, configPath)
	fmt.Fprintf(os.Stdout, "[SERVER] %s\n", serverConsoleSummary(profile, cfg))
	fmt.Fprintf(os.Stdout, "[SERVER] Server modules: %s\n", modules)

	// Nakama refuses to start on an unmigrated database; migrate is idempotent.
	migrate := exec.CommandContext(ctx, binary, migrateArgs...)
	migrate.Dir = workDir
	migrate.Env = append(os.Environ(), cfg.Environment...)
	migrate.Stdout, migrate.Stderr = os.Stdout, os.Stderr
	if err := migrate.Run(); err != nil {
		return fmt.Errorf("nakama database migration failed: %w", err)
	}

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), cfg.Environment...)
	var output io.Writer = os.Stdout
	if cfg.LogFile != "" {
		logPath := cfg.LogFile
		if !filepath.IsAbs(logPath) {
			logPath = filepath.Join(sys.baseDir, logPath)
		}
		if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
			return err
		}
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		output = io.MultiWriter(os.Stdout, f)
	}
	cmd.Stdout = output
	cmd.Stderr = output
	fmt.Fprintf(os.Stdout, "[SERVER] launching Nakama: %s %s\n", binary, strings.Join(printableNakamaArgs(args), " "))
	return cmd.Run()
}

// prepareServerProfile reads the profile -server starts Nakama with. It
// refuses Nakama's public secrets, gives a profile written before the
// launcher had them the settings it lacks, saving it so that its secrets stay
// the same from one start to the next, and prints its warnings to out.
func prepareServerProfile(path string, out io.Writer) (IkemenServerConfig, error) {
	resolved := resolveServerPath(path)
	raw, err := readIkemenServerConfig(path)
	if err != nil {
		return IkemenServerConfig{}, err
	}
	if err := checkServerSecrets(resolved, raw); err != nil {
		return IkemenServerConfig{}, err
	}
	var added []string
	for _, s := range serverSecrets {
		if *s.field(&raw) == "" {
			added = append(added, s.name)
		}
	}
	if raw.ConsoleAddress == "" {
		added = append(added, "console_address")
	}
	if raw.ConsoleUser == "" {
		added = append(added, "console_user")
	}
	if raw.NakamaModules == "" {
		added = append(added, "nakama_modules")
	}
	cfg := raw
	fillServerProfile(&cfg)
	cfg = normalizeServerConfig(cfg)
	if len(added) > 0 {
		if err := saveIkemenServerConfig(path, cfg); err != nil {
			return IkemenServerConfig{}, err
		}
		private := ""
		if runtime.GOOS != "windows" {
			private = " (now readable by you alone)"
		}
		fmt.Fprintf(out, "[SERVER] Added to %s%s: %s\n", resolved, private, strings.Join(added, ", "))
	} else if info, err := os.Stat(resolved); err == nil && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(out, "[SERVER] WARNING: other users of this computer can read %s, which holds the server's secrets: chmod 600 %s\n", resolved, resolved)
	}
	for _, w := range serverProfileWarnings(resolved, cfg) {
		fmt.Fprintln(out, "[SERVER] WARNING:", w)
	}
	return cfg, nil
}

func runServerCommand() error {
	if runtime.GOOS == "android" {
		return errors.New("dedicated server mode is not supported on Android")
	}
	path := defaultNakamaServerConfigPath
	if val, ok := sys.cmdFlags["-server-config"]; ok && strings.TrimSpace(val) != "" {
		path = val
	}
	cfg, err := prepareServerProfile(path, os.Stdout)
	if err != nil {
		return err
	}
	return runNakamaServer(context.Background(), resolveServerPath(path), cfg)
}
