// Package sysinfo reads host facts and derives sizing recommendations from them.
package sysinfo

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const (
	MiB = 1 << 20
	GiB = 1 << 30
)

// Memory is a snapshot of /proc/meminfo values in bytes.
type Memory struct {
	Total     uint64
	Available uint64
	SwapTotal uint64
	SwapFree  uint64
}

// ReadMemory parses /proc/meminfo.
func ReadMemory() (Memory, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return Memory{}, err
	}
	defer f.Close()
	return ParseMeminfo(f)
}

// ParseMeminfo parses the contents of /proc/meminfo.
func ParseMeminfo(r io.Reader) (Memory, error) {
	var m Memory
	fields := map[string]*uint64{
		"MemTotal":     &m.Total,
		"MemAvailable": &m.Available,
		"SwapTotal":    &m.SwapTotal,
		"SwapFree":     &m.SwapFree,
	}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		// Format: "MemTotal:        3906588 kB"
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		dst, wanted := fields[key]
		if !wanted {
			continue
		}
		parts := strings.Fields(rest)
		if len(parts) == 0 {
			continue
		}
		v, err := strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			return Memory{}, fmt.Errorf("meminfo %s: %w", key, err)
		}
		if len(parts) > 1 && parts[1] == "kB" {
			v *= 1024
		}
		*dst = v
	}
	if err := sc.Err(); err != nil {
		return Memory{}, err
	}
	if m.Total == 0 {
		return Memory{}, fmt.Errorf("meminfo: MemTotal not found")
	}
	return m, nil
}

// RecommendSwap returns the swap size for a host with the given RAM.
//
// Rule (kurulum-standardi.md): up to 2 GB RAM -> 2 GB, 4 GB -> 2 GB, 8 GB and above -> 4 GB.
// Providers report less than the nominal size (a "4 GB" VM shows ~3.7 GiB), so the
// thresholds sit between the nominal sizes.
func RecommendSwap(ram uint64) uint64 {
	if ram <= 6*GiB {
		return 2 * GiB
	}
	return 4 * GiB
}

// RecommendMongoCacheGB returns the WiredTiger cache size in GB for the given RAM.
// MongoDB's default (~50% of RAM minus 1 GB) starves application containers on small hosts.
func RecommendMongoCacheGB(ram uint64) float64 {
	switch {
	case ram <= 6*GiB:
		return 0.5
	case ram <= 12*GiB:
		return 1.0
	default:
		return 2.0
	}
}

// HumanBytes formats a byte count using binary units, e.g. "3.7 GiB".
func HumanBytes(b uint64) string {
	switch {
	case b >= GiB:
		return fmt.Sprintf("%.1f GiB", float64(b)/GiB)
	case b >= MiB:
		return fmt.Sprintf("%.0f MiB", float64(b)/MiB)
	default:
		return fmt.Sprintf("%d B", b)
	}
}
