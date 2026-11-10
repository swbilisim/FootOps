package sysinfo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Cgroup is what a cgroup (v2) directory tells about the processes in it: their CPU
// time so far, their memory as `docker stats` counts it (usage less the page cache the
// kernel would drop first, inactive_file) and the memory limit (0: none).
type Cgroup struct {
	CPU      time.Duration
	MemUsage uint64
	MemLimit uint64
}

// ReadCgroup reads a cgroup v2 directory: cpu.stat, memory.current, memory.stat and
// memory.max. These are the files dockerd itself reads for a container's stats.
func ReadCgroup(dir string) (Cgroup, error) {
	var c Cgroup
	b, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return c, err
	}
	usec, ok := keyedValue(string(b), "usage_usec")
	if !ok {
		return c, errors.New("cpu.stat: usage_usec yok")
	}
	c.CPU = time.Duration(usec) * time.Microsecond
	b, err = os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return c, err
	}
	used, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return c, fmt.Errorf("memory.current: %w", err)
	}
	b, err = os.ReadFile(filepath.Join(dir, "memory.stat"))
	if err != nil {
		return c, err
	}
	if cache, ok := keyedValue(string(b), "inactive_file"); ok && cache < used {
		used -= cache
	}
	c.MemUsage = used
	b, err = os.ReadFile(filepath.Join(dir, "memory.max"))
	if err != nil {
		return c, err
	}
	if v := strings.TrimSpace(string(b)); v != "max" {
		if c.MemLimit, err = strconv.ParseUint(v, 10, 64); err != nil {
			return c, fmt.Errorf("memory.max: %w", err)
		}
	}
	return c, nil
}

// keyedValue finds "key value" in a flat-keyed cgroup file such as cpu.stat.
func keyedValue(text, key string) (uint64, bool) {
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, key+" "); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}
