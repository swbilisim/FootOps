package sysinfo

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CPUTimes are cumulative jiffies from the "cpu" line of /proc/stat.
type CPUTimes struct {
	Busy, Total uint64
}

// ParseCPUTimes reads the aggregate "cpu" line of /proc/stat.
func ParseCPUTimes(r io.Reader) (CPUTimes, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var t CPUTimes
		for i, s := range f[1:] {
			// guest and guest_nice (fields 9, 10) are already included in user/nice.
			if i >= 8 {
				break
			}
			v, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return CPUTimes{}, err
			}
			t.Total += v
			// idle (3) and iowait (4) are not busy time.
			if i != 3 && i != 4 {
				t.Busy += v
			}
		}
		return t, nil
	}
	if err := sc.Err(); err != nil {
		return CPUTimes{}, err
	}
	return CPUTimes{}, errors.New("/proc/stat: no cpu line")
}

// CPUPercent returns the busy percentage between two samples (0-100, all cores).
func CPUPercent(prev, cur CPUTimes) float64 {
	if cur.Total <= prev.Total || cur.Busy < prev.Busy {
		return 0
	}
	return float64(cur.Busy-prev.Busy) / float64(cur.Total-prev.Total) * 100
}

// ReadCPUTimes reads /proc/stat.
func ReadCPUTimes() (CPUTimes, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return CPUTimes{}, err
	}
	defer f.Close()
	return ParseCPUTimes(f)
}

// Load is the 1, 5 and 15 minute load average.
type Load struct{ One, Five, Fifteen float64 }

func ParseLoadavg(s string) (Load, error) {
	f := strings.Fields(s)
	if len(f) < 3 {
		return Load{}, errors.New("/proc/loadavg: short")
	}
	var l Load
	var err error
	for i, dst := range []*float64{&l.One, &l.Five, &l.Fifteen} {
		if *dst, err = strconv.ParseFloat(f[i], 64); err != nil {
			return Load{}, err
		}
	}
	return l, nil
}

func ReadLoad() (Load, error) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return Load{}, err
	}
	return ParseLoadavg(string(b))
}

func ParseUptime(s string) (time.Duration, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, errors.New("/proc/uptime: empty")
	}
	sec, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(sec * float64(time.Second)), nil
}

func ReadUptime() (time.Duration, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	return ParseUptime(string(b))
}

// Disk is usage of the filesystem containing a path.
type Disk struct {
	Path        string
	Total, Used uint64
}

func (d Disk) Percent() float64 {
	if d.Total == 0 {
		return 0
	}
	return float64(d.Used) / float64(d.Total) * 100
}

// ReadDisk reports usage as df does: Used excludes reserved blocks from Total's
// perspective, so Used/(Used+Avail) is what a user can actually fill.
func ReadDisk(path string) (Disk, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Disk{}, err
	}
	bs := uint64(st.Bsize)
	used := (uint64(st.Blocks) - uint64(st.Bfree)) * bs
	avail := uint64(st.Bavail) * bs
	return Disk{Path: path, Total: used + avail, Used: used}, nil
}
