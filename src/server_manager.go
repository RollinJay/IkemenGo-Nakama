package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const defaultNakamaServerConfigPath = "save/server.json"

type IkemenServerConfig struct {
	Version              int      `json:"version"`
	Name                 string   `json:"name"`
	Game                 string   `json:"game"`
	BindAddress          string   `json:"bind_address"`
	PublicAddress        string   `json:"public_address"`
	ServerKey            string   `json:"server_key"`
	ClientSSL            bool     `json:"client_ssl"`
	NakamaBinary         string   `json:"nakama_binary"`
	NakamaConfig         string   `json:"nakama_config"`
	WorkingDir           string   `json:"working_dir"`
	ExtraArgs            []string `json:"extra_args,omitempty"`
	Environment          []string `json:"environment,omitempty"`
	LogFile              string   `json:"log_file,omitempty"`
	DatabaseDSN          string   `json:"database_dsn,omitempty"`
	NakamaHTTPPort       int      `json:"nakama_http_port"`
	NakamaGRPCPort       int      `json:"nakama_grpc_port"`
	NakamaConsolePort    int      `json:"nakama_console_port"`
	EnableMatchmaking    bool     `json:"enable_matchmaking"`
	EnableLobbies        bool     `json:"enable_lobbies"`
	EnableRanked         bool     `json:"enable_ranked"`
	EnableSpectators     bool     `json:"enable_spectators"`
	EnableChat           bool     `json:"enable_chat"`
	DefaultLobbyMode     string   `json:"default_lobby_mode"`
	RankedBestOf         int      `json:"ranked_best_of"`
	RankedSwitchSides    bool     `json:"ranked_switch_sides"`
	WinnerKeepsSelection bool     `json:"winner_keeps_selection"`
	LobbyMaxGames        int      `json:"lobby_max_games"`
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
		c.ServerKey = "defaultkey"
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
	if c.NakamaHTTPPort == 0 {
		c.NakamaHTTPPort = 7350
	}
	if c.NakamaGRPCPort == 0 {
		c.NakamaGRPCPort = 7349
	}
	if c.NakamaConsolePort == 0 {
		c.NakamaConsolePort = 7351
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

func resolveServerPath(path string) string {
	if strings.TrimSpace(path) == "" {
		path = defaultNakamaServerConfigPath
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(sys.baseDir, path)
}

func loadIkemenServerConfig(path string) (IkemenServerConfig, error) {
	resolved := resolveServerPath(path)
	data, err := os.ReadFile(resolved)
	if err != nil {
		return IkemenServerConfig{}, fmt.Errorf("read server config %q: %w", resolved, err)
	}
	var cfg IkemenServerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return IkemenServerConfig{}, fmt.Errorf("parse server config %q: %w", resolved, err)
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
	return os.WriteFile(resolved, data, 0644)
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
	cfg := normalizeServerConfig(IkemenServerConfig{})
	fmt.Fprintln(out, "IKEMEN GO ONLINE SERVER WIZARD")
	fmt.Fprintln(out, "This headless wizard creates an optional dedicated Nakama launcher profile.")
	fmt.Fprintln(out)
	var err error
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
	if cfg.EnableMatchmaking, err = promptBool(reader, out, "Enable matchmaking", true); err != nil {
		return err
	}
	if cfg.EnableLobbies, err = promptBool(reader, out, "Enable lobbies", true); err != nil {
		return err
	}
	if cfg.EnableRanked, err = promptBool(reader, out, "Enable ranked", true); err != nil {
		return err
	}
	if cfg.EnableSpectators, err = promptBool(reader, out, "Enable spectators", true); err != nil {
		return err
	}
	if cfg.EnableChat, err = promptBool(reader, out, "Enable chat", true); err != nil {
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
	if err := saveIkemenServerConfig(path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nSaved server configuration to %s\n", resolveServerPath(path))
	profilePath := strings.TrimSuffix(path, filepath.Ext(path)) + "-client.json"
	if err := saveNakamaClientProfile(profilePath, cfg); err != nil {
		return err
	}
	fmt.Fprintf(out, "Saved client connection profile to %s\n", resolveServerPath(profilePath))
	fmt.Fprintln(out, "Use: Ikemen_GO -server -server-config", path)
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
		STUNServers: nil,
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

func runNakamaServer(ctx context.Context, cfg IkemenServerConfig) error {
	cfg = normalizeServerConfig(cfg)
	binary := findNakamaBinary(cfg)
	if binary == "" {
		return errors.New("Nakama executable not found; set nakama_binary in the server config")
	}
	args := append([]string{}, cfg.ExtraArgs...)
	if cfg.NakamaConfig != "" {
		configPath := cfg.NakamaConfig
		if !filepath.IsAbs(configPath) && cfg.WorkingDir == "" {
			configPath = filepath.Join(sys.baseDir, configPath)
		}
		args = append([]string{"--config", configPath}, args...)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	if cfg.WorkingDir != "" {
		cmd.Dir = cfg.WorkingDir
	} else {
		cmd.Dir = sys.baseDir
	}
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
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return err
		}
		defer f.Close()
		output = io.MultiWriter(os.Stdout, f)
	}
	cmd.Stdout = output
	cmd.Stderr = output
	fmt.Fprintf(os.Stdout, "[SERVER] launching Nakama: %s %s\n", binary, strings.Join(args, " "))
	return cmd.Run()
}

func runServerCommand() error {
	if runtime.GOOS == "android" {
		return errors.New("dedicated server mode is not supported on Android")
	}
	path := defaultNakamaServerConfigPath
	if val, ok := sys.cmdFlags["-server-config"]; ok && strings.TrimSpace(val) != "" {
		path = val
	}
	cfg, err := loadIkemenServerConfig(path)
	if err != nil {
		return err
	}
	return runNakamaServer(context.Background(), cfg)
}
