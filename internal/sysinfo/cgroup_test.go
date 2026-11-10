package sysinfo

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCgroup lays out the files ReadCgroup reads, as a container's scope has them.
func writeCgroup(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadCgroup(t *testing.T) {
	dir := t.TempDir()
	// As on H1 (2026-11-10): a site limited to 384 MB.
	writeCgroup(t, dir, map[string]string{
		"cpu.stat":       "usage_usec 24910249\nuser_usec 19139318\nsystem_usec 5770931\nnr_periods 0\n",
		"memory.current": "149852160\n",
		"memory.stat":    "anon 70914048\nfile 67198976\nactive_file 5226496\ninactive_file 61972480\n",
		"memory.max":     "402653184\n",
	})
	c, err := ReadCgroup(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.CPU != 24910249*time.Microsecond || c.MemUsage != 149852160-61972480 || c.MemLimit != 402653184 {
		t.Errorf("cgroup = %+v", c)
	}

	// No limit; a cache larger than the usage (it can be, briefly) is not taken off.
	writeCgroup(t, dir, map[string]string{"memory.max": "max\n", "memory.stat": "inactive_file 999999999999\n"})
	if c, err := ReadCgroup(dir); err != nil || c.MemLimit != 0 || c.MemUsage != 149852160 {
		t.Errorf("unlimited: %+v %v", c, err)
	}

	for name, text := range map[string]string{"cpu.stat": "user_usec 1\n", "memory.current": "çok\n", "memory.max": "yarım\n"} {
		d := t.TempDir()
		writeCgroup(t, d, map[string]string{"cpu.stat": "usage_usec 1\n", "memory.current": "1\n", "memory.stat": "", "memory.max": "max\n"})
		writeCgroup(t, d, map[string]string{name: text})
		if _, err := ReadCgroup(d); err == nil {
			t.Errorf("%s %q: read", name, text)
		}
	}
	if _, err := ReadCgroup(filepath.Join(dir, "gitti")); err == nil {
		t.Error("a cgroup that is gone: read")
	}
}
