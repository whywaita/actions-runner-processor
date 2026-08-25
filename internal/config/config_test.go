package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
github:
  client_id: "123456"
  private_key_path: /nonexistent
    `), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", cfgPath)
	cfg, err := Load()
	if err == nil {
		t.Errorf("expected error from missing private key, got nil")
		return
	}
	_ = cfg
}

func TestResolveMaxRunners(t *testing.T) {
	cfg := &Config{}
	if cfg.ResolveMaxRunners() == 0 {
		t.Error("ResolveMaxRunners with 0 should return NumCPU")
	}

	cfg.Runner.MaxRunners = 3
	if cfg.ResolveMaxRunners() != 3 {
		t.Error("ResolveMaxRunners should return explicit value")
	}
}

func TestDefaults(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyPath, []byte(`-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEA0Z3...
-----END RSA PRIVATE KEY-----`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
github:
  client_id: "123456"
  private_key_path: "`+keyPath+`"
    `), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", cfgPath)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ScaleSetName != "actions-runner-processor" {
		t.Errorf("ScaleSetName: want actions-runner-processor, got %s", cfg.ScaleSetName)
	}
	if cfg.Runner.Version != "latest" {
		t.Errorf("Version: want latest, got %s", cfg.Runner.Version)
	}
	if cfg.Runner.ImagePath != "/opt/runner/image" {
		t.Errorf("ImagePath: want /opt/runner/image, got %s", cfg.Runner.ImagePath)
	}
	if cfg.Runner.Entrypoint != "/opt/actions-runner/run.sh" {
		t.Errorf("Entrypoint: want /opt/actions-runner/run.sh, got %s", cfg.Runner.Entrypoint)
	}
	if cfg.Runner.ShutdownGraceTimeout.Duration != 10*time.Minute {
		t.Errorf("ShutdownGraceTimeout: want 10m, got %v", cfg.Runner.ShutdownGraceTimeout.Duration)
	}
	if cfg.GitHub.APIURL != "https://api.github.com" {
		t.Errorf("APIURL: want https://api.github.com, got %s", cfg.GitHub.APIURL)
	}
	if cfg.GitHub.URL != "https://github.com" {
		t.Errorf("URL: want https://github.com, got %s", cfg.GitHub.URL)
	}
}

func TestShutdownGraceTimeoutParsing(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyPath, []byte(`-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEA0Z3...
-----END RSA PRIVATE KEY-----`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
github:
  client_id: "123456"
  private_key_path: "`+keyPath+`"
runner:
  shutdown_grace_timeout: "2m"
    `), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", cfgPath)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Runner.ShutdownGraceTimeout.Duration != 2*time.Minute {
		t.Errorf("ShutdownGraceTimeout: want 2m, got %v", cfg.Runner.ShutdownGraceTimeout.Duration)
	}
}

func TestInvalidShutdownGraceTimeout(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyPath, []byte(`-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEA0Z3...
-----END RSA PRIVATE KEY-----`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
github:
  client_id: "123456"
  private_key_path: "`+keyPath+`"
runner:
  shutdown_grace_timeout: "not-a-duration"
    `), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", cfgPath)
	if _, err := Load(); err == nil {
		t.Fatal("Load() = nil error, want parse error for invalid duration")
	}
}

func TestGHESDefaults(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyPath, []byte(`-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEA0Z3...
-----END RSA PRIVATE KEY-----`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
github:
  client_id: "123456"
  private_key_path: "`+keyPath+`"
  api_url: "https://github.mycompany.com/api/v3"
  url: "https://github.mycompany.com"
    `), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", cfgPath)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.GitHub.APIURL != "https://github.mycompany.com/api/v3" {
		t.Errorf("APIURL: want GHES URL, got %s", cfg.GitHub.APIURL)
	}
	if cfg.GitHub.URL != "https://github.mycompany.com" {
		t.Errorf("URL: want GHES URL, got %s", cfg.GitHub.URL)
	}
}
