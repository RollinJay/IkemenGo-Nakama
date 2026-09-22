package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const defaultNakamaProfilePath = "data/online/servers.json"

type NakamaServerProfile struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Host        string   `json:"host"`
	Port        int      `json:"port"`
	ServerKey   string   `json:"server_key"`
	SSL         bool     `json:"ssl"`
	Status      bool     `json:"status"`
	DeviceID    string   `json:"device_id,omitempty"`
	Username    string   `json:"username,omitempty"`
	Game        string   `json:"game"`
	Build       string   `json:"build"`
	Region      string   `json:"region,omitempty"`
	STUNServers []string `json:"stun_servers,omitempty"`
}

type NakamaServerProfiles struct {
	Version int                   `json:"version"`
	Default string                `json:"default"`
	Servers []NakamaServerProfile `json:"servers"`
}

func (p NakamaServerProfile) clientConfig() NakamaConfig {
	port := p.Port
	if port == 0 {
		port = 7350
	}
	serverKey := p.ServerKey
	if serverKey == "" {
		serverKey = "defaultkey"
	}
	return NakamaConfig{
		Host:       p.Host,
		Port:       port,
		ServerKey:  serverKey,
		UseTLS:     p.SSL,
		Status:     p.Status,
		DeviceID:   p.DeviceID,
		Username:   p.Username,
		Game:       p.Game,
		GameBuild:  p.Build,
		Region:     p.Region,
		RequestTTL: defaultNakamaRequestTTL,
	}
}

func resolveNakamaProfilePath(path string) string {
	if strings.TrimSpace(path) == "" {
		path = defaultNakamaProfilePath
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(sys.baseDir, path)
}

func LoadNakamaServerProfiles(path string) (*NakamaServerProfiles, error) {
	resolved := resolveNakamaProfilePath(path)
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read Nakama server profiles %q: %w", resolved, err)
	}
	var profiles NakamaServerProfiles
	if err := json.Unmarshal(data, &profiles); err != nil {
		return nil, fmt.Errorf("parse Nakama server profiles %q: %w", resolved, err)
	}
	if profiles.Version == 0 {
		profiles.Version = 1
	}
	if len(profiles.Servers) == 0 {
		return nil, errors.New("Nakama server profiles contain no servers")
	}
	if profiles.Default == "" {
		profiles.Default = profiles.Servers[0].ID
	}
	for i := range profiles.Servers {
		if profiles.Servers[i].ID == "" {
			return nil, fmt.Errorf("Nakama server profile %d has no id", i)
		}
		if strings.TrimSpace(profiles.Servers[i].Host) == "" {
			return nil, fmt.Errorf("Nakama server profile %q has no host", profiles.Servers[i].ID)
		}
	}
	return &profiles, nil
}

func (p *NakamaServerProfiles) Find(id string) (NakamaServerProfile, bool) {
	if p == nil {
		return NakamaServerProfile{}, false
	}
	id = strings.TrimSpace(id)
	if id == "" {
		id = p.Default
	}
	for _, server := range p.Servers {
		if server.ID == id {
			return server, true
		}
	}
	return NakamaServerProfile{}, false
}

func defaultNakamaClientConfig() (NakamaConfig, []string, error) {
	path := defaultNakamaProfilePath
	if val, ok := sys.cmdFlags["-nakama-config"]; ok && strings.TrimSpace(val) != "" {
		path = val
	}
	profiles, err := LoadNakamaServerProfiles(path)
	if err != nil {
		return NakamaConfig{}, nil, err
	}
	id := ""
	if val, ok := sys.cmdFlags["-nakama-server"]; ok {
		id = val
	}
	profile, ok := profiles.Find(id)
	if !ok {
		return NakamaConfig{}, nil, fmt.Errorf("Nakama server profile %q not found", id)
	}
	return profile.clientConfig(), append([]string(nil), profile.STUNServers...), nil
}
