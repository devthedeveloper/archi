package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const archiDirName = ".archi"

// Config is the cached state written by `init` and read by `design`/`status`.
// It holds no secrets — API keys live only in the environment.
type Config struct {
	Version       string         `json:"version"`
	Provider      string         `json:"provider"`
	Model         string         `json:"model"`
	Root          string         `json:"root"`
	InitializedAt string         `json:"initializedAt"`
	Languages     map[string]int `json:"languages"`
	FileCount     int            `json:"fileCount"`
	ApproxTokens  int            `json:"approxTokens"`
}

func archiDir(root string) string   { return filepath.Join(root, archiDirName) }
func mapPath(root string) string    { return filepath.Join(archiDir(root), "map.md") }
func profilePath(r string) string   { return filepath.Join(archiDir(r), "profile.md") }
func configPath(root string) string { return filepath.Join(archiDir(root), "config.json") }

func loadConfig(root string) (*Config, error) {
	data, err := os.ReadFile(configPath(root))
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) save(root string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(root), append(data, '\n'), 0o644)
}

// isInitialized reports whether an .archi cache exists for root.
func isInitialized(root string) bool {
	_, err := os.Stat(configPath(root))
	return err == nil
}
