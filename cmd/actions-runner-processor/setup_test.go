package main

import "testing"

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"20G", 20 * 1024 * 1024 * 1024, false},
		{"20GiB", 20 * 1024 * 1024 * 1024, false},
		{"50g", 50 * 1024 * 1024 * 1024, false},
		{"500M", 500 * 1024 * 1024, false},
		{"1.5G", int64(1.5 * 1024 * 1024 * 1024), false},
		{"10737418240", 10 * 1024 * 1024 * 1024, false},
		{" 40G ", 40 * 1024 * 1024 * 1024, false},
		{"", 0, true},
		{"abc", 0, true},
		{"10X", 0, true},
	}
	for _, c := range cases {
		got, err := parseSize(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseSize(%q): expected error, got %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSize(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSize(%q): got %d, want %d", c.in, got, c.want)
		}
	}
}

func TestComputeFullSize(t *testing.T) {
	const G = int64(1024 * 1024 * 1024)

	// Tarball-derived estimate (C x 3) dominates the 80%-of-free suggestion.
	if got := computeFullSize(40*G, 100*G); got != 120*G {
		t.Errorf("computeFullSize(tarball-dominant): got %d, want %d", got, 120*G)
	}
	// 80% of a large free disk dominates the tarball estimate.
	if got := computeFullSize(40*G, 200*G); got != 160*G {
		t.Errorf("computeFullSize(free-dominant): got %d, want %d", got, 160*G)
	}
	// Floor: never below the lightweight 20G default.
	if got := computeFullSize(1*G, 5*G); got != 20*G {
		t.Errorf("computeFullSize(floor): got %d, want %d", got, 20*G)
	}
}
