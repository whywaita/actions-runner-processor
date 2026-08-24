// Package config provides configuration loading for actions-runner-processor.
package config

import (
	"fmt"
	"os"
	"runtime"

	"gopkg.in/yaml.v3"
)

// Config represents the top-level configuration.
type Config struct {
	GitHub       GitHubConfig  `yaml:"github"`
	ScaleSetName string        `yaml:"scale_set_name"`
	Runner       RunnerConfig  `yaml:"runner"`
	Metrics      MetricsConfig `yaml:"metrics"`
	WebUI        WebUIConfig   `yaml:"webui"`
	LogFormat    string        `yaml:"log_format"` // "text" (default) or "json"
}

// GitHubConfig holds GitHub App authentication parameters and GHES endpoint overrides.
type GitHubConfig struct {
	ClientID       string `yaml:"client_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
	// PrivateKey is loaded from PrivateKeyPath at startup.
	PrivateKey string `yaml:"-"`

	// GHES support: override default github.com endpoints.
	// api_url defaults to "https://api.github.com".
	// url is the base URL for the GitHub instance (e.g. "https://github.mycompany.com").
	APIURL string `yaml:"api_url"`
	URL    string `yaml:"url"`
}

// RunnerConfig holds runner binary settings.
type RunnerConfig struct {
	Version    string `yaml:"version"`
	MaxRunners int    `yaml:"max_runners"`
	MinRunners int    `yaml:"min_runners"`

	// ImagePath is the root filesystem directory used as the custom runner
	// image for nspawn mode (a debootstrap / custom-built rootfs with
	// actions/runner preinstalled).
	ImagePath string `yaml:"image_path"`
	// Entrypoint is the absolute path (inside the container) of the command
	// that launches the runner. Defaults to /opt/actions-runner/run.sh.
	Entrypoint string `yaml:"entrypoint"`
	// WorkspacePath is the host directory (per-runner subdirectory) that is
	// bind-mounted into the container at /opt/actions-runner/_work and
	// /opt/actions-runner/_tool. It must live on real disk (not the
	// volatile-overlay tmpfs) or toolchain extraction fills the RAM-backed
	// overlay with ENOSPC. Defaults to /opt/runner/workspaces.
	WorkspacePath string `yaml:"workspace_path"`
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
		path = "/etc/actions-runner-processor/config.yaml"
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
		cfg.ScaleSetName = "actions-runner-processor"
	}
	if cfg.Runner.Version == "" {
		cfg.Runner.Version = "latest"
	}
	// The runner boots from a custom image (nspawn); provide a conventional
	// default path reachable from the deploy layout.
	if cfg.Runner.ImagePath == "" {
		cfg.Runner.ImagePath = "/opt/runner/image"
	}
	if cfg.Runner.Entrypoint == "" {
		cfg.Runner.Entrypoint = "/opt/actions-runner/run.sh"
	}
	if cfg.Runner.WorkspacePath == "" {
		cfg.Runner.WorkspacePath = "/opt/runner/workspaces"
	}
	if cfg.GitHub.APIURL == "" {
		cfg.GitHub.APIURL = "https://api.github.com"
	}
	if cfg.GitHub.URL == "" {
		cfg.GitHub.URL = "https://github.com"
	}
	if cfg.LogFormat == "" {
		cfg.LogFormat = "json"
	}

	return &cfg, nil
}
