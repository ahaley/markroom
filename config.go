package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Root is a whitelisted directory of markdown files.
type Root struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type Config struct {
	Roots []Root `json:"roots"`
}

func configDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "markroom")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func configPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

func loadConfig() (*Config, error) {
	p, err := configPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p, err)
	}
	return &cfg, nil
}

func (c *Config) save() error {
	p, err := configPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0o644)
}

func (c *Config) find(name string) *Root {
	for i := range c.Roots {
		if c.Roots[i].Name == name {
			return &c.Roots[i]
		}
	}
	return nil
}
