package install

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"

	"github.com/swbilisim/FootOps/internal/sysinfo"
)

var supportedUbuntu = map[string]bool{"22.04": true, "24.04": true}

const (
	minRAM      = 900 * sysinfo.MiB
	minFreeDisk = 8 * sysinfo.GiB
)

// Preflight refuses to run on hosts the installer was not written for.
func Preflight(h Host) (sysinfo.Memory, error) {
	if os.Geteuid() != 0 {
		return sysinfo.Memory{}, fmt.Errorf("root olarak çalıştırılmalı (sudo footops install)")
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return sysinfo.Memory{}, fmt.Errorf("desteklenmeyen mimari %s", runtime.GOARCH)
	}

	osr, err := h.ReadFile("/etc/os-release")
	if err != nil {
		return sysinfo.Memory{}, err
	}
	id, ver := osRelease(osr)
	if id != "ubuntu" || !supportedUbuntu[ver] {
		return sysinfo.Memory{}, fmt.Errorf("sadece Ubuntu 22.04 / 24.04 destekleniyor (bulunan: %s %s)", id, ver)
	}

	f, err := os.Open(h.Path("/proc/meminfo"))
	if err != nil {
		return sysinfo.Memory{}, err
	}
	defer f.Close()
	mem, err := sysinfo.ParseMeminfo(f)
	if err != nil {
		return sysinfo.Memory{}, err
	}
	if mem.Total < minRAM {
		return mem, fmt.Errorf("en az 1 GB RAM gerekli (bulunan %s)", sysinfo.HumanBytes(mem.Total))
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs(h.Path("/"), &st); err != nil {
		return mem, err
	}
	if free := uint64(st.Bavail) * uint64(st.Bsize); free < minFreeDisk {
		return mem, fmt.Errorf("diskte en az %s boş alan gerekli (bulunan %s)",
			sysinfo.HumanBytes(minFreeDisk), sysinfo.HumanBytes(free))
	}
	return mem, nil
}

func osRelease(s string) (id, version string) {
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		switch k {
		case "ID":
			id = v
		case "VERSION_ID":
			version = v
		}
	}
	return id, version
}
