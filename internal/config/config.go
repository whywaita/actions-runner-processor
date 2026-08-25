// Package config provides configuration loading for actions-runner-processor.
package config

import (
	"fmt"
	"os"
	"runtime"
	"time"

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

	// ShutdownGraceTimeout is how long the processor keeps running to let
	// in-flight jobs finish after SIGTERM/SIGINT, before it force-kills the
	// remaining runner containers. Preserving jobs across a restart is what
	// makes the shutdown graceful -- without it, systemd SIGKILLs the whole
	// cgroup (including nspawn children) the moment the process exits.
	// Defaults to 10 minutes.
	ShutdownGraceTimeout Duration `yaml:"shutdown_grace_timeout"`

	// ImagePath is the root filesystem directory used as the custom runner
	// image for nspawn mode (a debootstrap / custom-built rootfs with
	// actions/runner preinstalled).
	ImagePath string `yaml:"image_path"`
	// Entrypoint is the absolute path (inside the container) of the command
	// that launches the runner. Defaults to /opt/actions-runner/run.sh.
	Entrypoint string `yaml:"entrypoint"`
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

// Duration wraps time.Duration so YAML can unmarshal human-readable forms
// such as "10m" or "90s".
type Duration struct {
	time.Duration
}

// UnmarshalYAML parses a duration string (e.g. "10m", "90s") into Duration.
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
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
	if cfg.Runner.ShutdownGraceTimeout.Duration == 0 {
		cfg.Runner.ShutdownGraceTimeout.Duration = 10 * time.Minute
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
