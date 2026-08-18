package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

type Config struct {
	Secret     string `json:"secret"`
	ServerPort int    `json:"server_port"`
	// PublicURL overrides Tailscale Funnel when set (e.g. a Cloudflare Tunnel
	// hostname). Corporate proxies block *.ts.net, so Funnel is not always an
	// option — see docs/architecture.md.
	PublicURL string `json:"public_url,omitempty"`
}

func Load() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return generateNew()
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.ServerPort == 0 {
		cfg.ServerPort = 8457
	}
	return &cfg, nil
}

func Save(cfg *Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func SlotsDir() (string, error) {
	dir, err := baseDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(dir, "slots")
	return d, os.MkdirAll(d, 0700)
}

func generateNew() (*Config, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	cfg := &Config{
		Secret:     hex.EncodeToString(b),
		ServerPort: 8457,
	}
	if err := Save(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func configPath() (string, error) {
	dir, err := baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

func baseDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".clipbridge")
	return dir, os.MkdirAll(dir, 0700)
}
