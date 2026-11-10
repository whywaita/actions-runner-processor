package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSystemdVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		out     string
		want    string
		wantErr bool
	}{
		{
			name: "modern banner",
			out:  "systemd 255\n+PAM +AUDIT +SELINUX -APPARMOR\n",
			want: "255",
		},
		{
			name: "no features line",
			out:  "systemd 252\n",
			want: "252",
		},
		{
			name: "systemd-nspawn banner",
			out:  "systemd-nspawn 254\n+PAM\n",
			want: "254",
		},
		{
			name:    "garbage",
			out:     "nspawn: some random tool\nversion 9\n",
			wantErr: true,
		},
		{
			name:    "empty",
			out:     "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseSystemdVersion(tt.out)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSystemdVersion(%q) = %q, want error", tt.out, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSystemdVersion(%q): %v", tt.out, err)
			}
			if got != tt.want {
				t.Fatalf("parseSystemdVersion(%q) = %q, want %q", tt.out, got, tt.want)
			}
		})
	}
}

func TestParseNspawnInfoJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		out                string
		wantVersion        string
		wantSystemdVersion string
	}{
		{
			name:               "nspawn 3.x info answer",
			out:                `{"architecture":"amd64","backend":"overlay","cpus":8,"images":3,"kernel":"6.8","log_driver":"local","machines":{"total":1,"running":1},"os":"Debian GNU/Linux 13","registry":"","state_dir":"/var/lib/nspawn","systemd_version":"255","version":"3.2.0"}`,
			wantVersion:        "3.2.0",
			wantSystemdVersion: "255",
		},
		{
			name:               "service cannot read systemd version",
			out:                `{"version":"3.0.0","systemd_version":""}`,
			wantVersion:        "3.0.0",
			wantSystemdVersion: "",
		},
		{
			name:               "unknown shape tolerated",
			out:                `{"unexpected":"shape"}`,
			wantVersion:        "",
			wantSystemdVersion: "",
		},
		{
			name:               "not json tolerated",
			out:                `nspawn: service did not answer`,
			wantVersion:        "",
			wantSystemdVersion: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			version, systemdVersion := parseNspawnInfoJSON([]byte(tt.out))
			if version != tt.wantVersion || systemdVersion != tt.wantSystemdVersion {
				t.Fatalf("parseNspawnInfoJSON(%q) = (%q, %q), want (%q, %q)",
					tt.out, version, systemdVersion, tt.wantVersion, tt.wantSystemdVersion)
			}
		})
	}
}

func TestAPISocketPresent(t *testing.T) {
	t.Parallel()

	if apiSocketPresent(filepath.Join(t.TempDir(), "missing.sock")) {
		t.Fatal("apiSocketPresent(missing) = true, want false")
	}
	present := filepath.Join(t.TempDir(), "api.sock")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !apiSocketPresent(present) {
		t.Fatal("apiSocketPresent(existing) = false, want true")
	}
}
