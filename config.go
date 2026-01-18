package main

import (
	"encoding/json"
	"os"
)

type Config struct {
	ExitNode         string `json:"exit_node"`
	Hostname         string `json:"hostname"`
	AuthKey          string `json:"authkey"`
	ProxyPort        int    `json:"proxy_port"`
	Verbose          bool   `json:"verbose"`
	ExportListeners  bool   `json:"export_listeners"`
	ExportAllowPorts string `json:"export_allow_ports"`
	ExportDenyPorts  string `json:"export_deny_ports"`
	ExportMax        int    `json:"export_max"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	// Set defaults
	if config.Hostname == "" {
		config.Hostname = "tailproxy"
	}
	if config.ProxyPort == 0 {
		config.ProxyPort = 1080
	}
	if config.ExportMax == 0 {
		config.ExportMax = 32
	}

	return &config, nil
}

func (c *Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
