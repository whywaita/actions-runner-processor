package main

import "testing"

func TestResolveReleaseTag(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"v0.0.4", "v0.0.4"},
		{"0.0.4", "0.0.4"},
		{"https://github.com/whywaita/actions-runner-processor/releases/tag/v0.0.4", "v0.0.4"},
		{"https://github.com/whywaita/actions-runner-processor/releases/tag/v0.0.5-rc2", "v0.0.5-rc2"},
	}
	for _, tc := range tests {
		if got := resolveReleaseTag(tc.in); got != tc.want {
			t.Errorf("resolveReleaseTag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFullImageAssetPrefix(t *testing.T) {
	if got := fullImageAssetPrefix("amd64"); got != "actions-runner-image-full-amd64.tar.gz.part-" {
		t.Errorf("fullImageAssetPrefix(amd64) = %q", got)
	}
}
