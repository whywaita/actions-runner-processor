// Package config provides configuration loading for runner-listener.
package config

import (
	"fmt"
	"os"
	"runtime"

	"gopkg.in/yaml.v3"
)

// Config represents the top-level configuration.
type Config struct {
	GitHub       GitHubConfig `yaml:"github"`
	ScaleSetName string       `yaml:"scale_set_name"`
	Runner       RunnerConfig `yaml:"runner"`
	Metrics      MetricsConfig `yaml:"metrics"`
	WebUI        WebUIConfig  `yaml:"webui"`
}

// GitHubConfig holds GitHub App authentication parameters.
type GitHubConfig struct {
	ClientID       string `yaml:"client_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
	// PrivateKey is loaded from PrivateKeyPath at startup.
	PrivateKey string `yaml:"-"`
}

// RunnerConfig holds runner binary settings.
type RunnerConfig struct {
	Version            string `yaml:"version"`
	ActionsRunnerPath  string `yaml:"actions_runner_path"`
	WorkspaceRoot      string `yaml:"workspace_root"`
	MaxRunners         int    `yaml:"max_runners"`
	MinRunners         int    `yaml:"min_runners"`
}

// MetricsConfig holds Prometheus exporter settings.
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"`
}

// WebUIConfig holds dashboard settings.
type WebUIConfig struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"`
}

// ResolveMaxRunners returns the effective max_runners (0 = NumCPU).
func (c *Config) ResolveMaxRunners() int {
	if c.Runner.MaxRunners == 0 {
		return runtime.NumCPU()
	}
	return c.Runner.MaxRunners
}

// Load reads and parses the config from CONFIG_PATH env or default path.
func Load() (*Config, error) {
	path := os.Getenv("CONFIG_PATH")
	if path == "" {
		path = "/etc/runner-listener/config.yaml"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Load private key from file
	if cfg.GitHub.PrivateKeyPath != "" {
		key, err := os.ReadFile(cfg.GitHub.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read private key: %w", err)
		}
		cfg.GitHub.PrivateKey = string(key)
	}

	// Defaults
	if cfg.ScaleSetName == "" {
		cfg.ScaleSetName = "runner-listener"
	}
	if cfg.Runner.Version == "" {
		cfg.Runner.Version = "latest"
	}
	if cfg.Runner.ActionsRunnerPath == "" {
		cfg.Runner.ActionsRunnerPath = "/opt/runner/actions-runner"
	}
	if cfg.Runner.WorkspaceRoot == "" {
		cfg.Runner.WorkspaceRoot = "/opt/runner/workspaces"
	}

	return &cfg, nil
}
