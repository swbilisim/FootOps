package sysinfo

import (
	"strings"
	"testing"
)

func TestParseMeminfo(t *testing.T) {
	in := `MemTotal:        3906588 kB
MemFree:          120000 kB
MemAvailable:    2500000 kB
SwapTotal:       2097148 kB
SwapFree:        2097148 kB
`
	m, err := parseMeminfo(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != 3906588*1024 {
		t.Errorf("Total = %d", m.Total)
	}
	if m.Available != 2500000*1024 {
		t.Errorf("Available = %d", m.Available)
	}
	if m.SwapTotal != 2097148*1024 {
		t.Errorf("SwapTotal = %d", m.SwapTotal)
	}
}

func TestParseMeminfoMissingTotal(t *testing.T) {
	if _, err := parseMeminfo(strings.NewReader("MemFree: 1 kB\n")); err == nil {
		t.Fatal("expected error when MemTotal is missing")
	}
}

func TestRecommendSwap(t *testing.T) {
	cases := []struct {
		name string
		ram  uint64
		want uint64
	}{
		{"1 GB VM", 960 * MiB, 2 * GiB},
		{"2 GB VM", 1900 * MiB, 2 * GiB},
		{"4 GB VM (CX23 reports ~3.7 GiB)", 3906588 * 1024, 2 * GiB},
		{"8 GB VM", 7800 * MiB, 4 * GiB},
		{"16 GB VM", 15800 * MiB, 4 * GiB},
		{"64 GB VM", 64 * GiB, 4 * GiB},
	}
	for _, c := range cases {
		if got := RecommendSwap(c.ram); got != c.want {
			t.Errorf("%s: RecommendSwap = %s, want %s", c.name, HumanBytes(got), HumanBytes(c.want))
		}
	}
}

func TestRecommendMongoCacheGB(t *testing.T) {
	cases := []struct {
		ram  uint64
		want float64
	}{
		{3906588 * 1024, 0.5},
		{7800 * MiB, 1.0},
		{32 * GiB, 2.0},
	}
	for _, c := range cases {
		if got := RecommendMongoCacheGB(c.ram); got != c.want {
			t.Errorf("RecommendMongoCacheGB(%s) = %v, want %v", HumanBytes(c.ram), got, c.want)
		}
	}
}
