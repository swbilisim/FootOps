package install

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/sysinfo"
)

// basePackages are installed with --no-install-recommends, so every runtime
// dependency is listed explicitly (python3-systemd: fail2ban's systemd backend).
var basePackages = []string{
	"ca-certificates", "curl", "gnupg", "ufw", "fail2ban", "python3-systemd", "unattended-upgrades",
}

// ---- apt ----

// aptOpts: wait for the dpkg lock instead of failing (a fresh cloud server runs
// cloud-init and apt-daily right after boot), and keep existing config files on
// upgrade instead of prompting.
var aptOpts = []string{
	"-o", "DPkg::Lock::Timeout=600",
	"-o", "Dpkg::Options::=--force-confdef",
	"-o", "Dpkg::Options::=--force-confold",
}

const (
	aptLockRetries = 20
	aptLockWait    = 15 * time.Second
)

// sleep is replaced in tests.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// aptUpdateStamp records our last successful `apt-get update`. apt's own cache file
// cannot be used for this: some images (e.g. Docker's) delete it after every run.
const aptUpdateStamp = "/var/lib/footops/apt-update-stamp"

// apt runs apt-get, retrying while another apt process holds a lock. DPkg::Lock::Timeout
// covers the dpkg lock but not the package-lists lock taken by `apt-get update`.
func apt(ctx context.Context, h Host, args ...string) error {
	full := append(append([]string{}, aptOpts...), args...)
	for attempt := 1; ; attempt++ {
		out, err := h.Run.Run(ctx, "apt-get", full...)
		if err == nil && len(args) > 0 && args[0] == "update" {
			return h.WriteFile(aptUpdateStamp, time.Now().UTC().Format(time.RFC3339)+"\n", 0o644)
		}
		if err == nil || !isAptLockError(out) || attempt == aptLockRetries {
			return err
		}
		if err := sleep(ctx, aptLockWait); err != nil {
			return err
		}
	}
}

func isAptLockError(out string) bool {
	return strings.Contains(out, "Could not get lock") || strings.Contains(out, "Unable to acquire the dpkg frontend lock") ||
		strings.Contains(out, "Unable to lock directory")
}

// repairDpkg finishes a dpkg run that was interrupted earlier (e.g. a previous installer
// run that lost power). It is a no-op on a healthy system.
func repairDpkg(ctx context.Context, h Host) error {
	_, err := h.Run.Run(ctx, "dpkg", "--configure", "-a")
	return err
}

// ---- packages ----

type packagesStep struct{ h Host }

func (s packagesStep) Name() string     { return "packages" }
func (s packagesStep) Describe() string { return "Temel paketler: " + strings.Join(basePackages, ", ") }

func (s packagesStep) Check(ctx context.Context) (bool, error) {
	for _, p := range basePackages {
		out, err := s.h.Run.Run(ctx, "dpkg-query", "-W", "-f=${Status}", p)
		if err != nil || !strings.Contains(out, "install ok installed") {
			return false, nil
		}
	}
	return true, nil
}

func (s packagesStep) Apply(ctx context.Context) error {
	if err := repairDpkg(ctx, s.h); err != nil {
		return err
	}
	if err := apt(ctx, s.h, "update"); err != nil {
		return err
	}
	return apt(ctx, s.h, append([]string{"install", "-y", "--no-install-recommends"}, basePackages...)...)
}

// ---- upgrade ----

// aptListsMaxAge: package lists older than this are refreshed before deciding whether
// upgrades are pending; otherwise a stale cache would report "nothing to do".
const aptListsMaxAge = 6 * time.Hour

type upgradeStep struct{ h Host }

func (s upgradeStep) Name() string { return "upgrade" }
func (s upgradeStep) Describe() string {
	return "Bekleyen paket güncellemelerini kur (yeni bağımlılıklar dahil, paket silinmez)"
}

func (s upgradeStep) Check(ctx context.Context) (bool, error) {
	if !s.listsFresh() {
		return false, nil
	}
	out, err := s.h.Run.Run(ctx, "apt-get", "-s", "--with-new-pkgs", "upgrade")
	if err != nil {
		return false, nil
	}
	return pendingUpgrades(out) == 0, nil
}

// Apply uses --with-new-pkgs: plain `apt-get upgrade` keeps back updates that need a new
// package, which includes every kernel security update.
func (s upgradeStep) Apply(ctx context.Context) error {
	if err := repairDpkg(ctx, s.h); err != nil {
		return err
	}
	if err := apt(ctx, s.h, "update"); err != nil {
		return err
	}
	return apt(ctx, s.h, "-y", "--with-new-pkgs", "upgrade")
}

// listsFresh reports whether package lists were refreshed recently, by us or by apt.
func (s upgradeStep) listsFresh() bool {
	for _, p := range []string{aptUpdateStamp, "/var/cache/apt/pkgcache.bin"} {
		if st, err := os.Stat(s.h.Path(p)); err == nil && time.Since(st.ModTime()) <= aptListsMaxAge {
			return true
		}
	}
	return false
}

// pendingUpgrades counts "Inst " lines in simulated apt output.
func pendingUpgrades(simulated string) int {
	n := 0
	for _, l := range strings.Split(simulated, "\n") {
		if strings.HasPrefix(l, "Inst ") {
			n++
		}
	}
	return n
}

// ---- hostname ----

// Cloud images run cloud-init on every boot; without this it resets the hostname
// and /etc/hosts from the provider's metadata (e.g. "ubuntu-4gb-nbg1-2").
const (
	cloudInitCfgDir  = "/etc/cloud/cloud.cfg.d"
	cloudInitHostCfg = cloudInitCfgDir + "/99-footops-hostname.cfg"
	cloudInitHost    = "# Managed by FootOps: keep the hostname set by footops install\npreserve_hostname: true\nmanage_etc_hosts: false\n"
)

type hostnameStep struct {
	h    Host
	name string
}

func (s hostnameStep) Name() string     { return "hostname" }
func (s hostnameStep) Describe() string { return "Hostname: " + s.name }

func (s hostnameStep) Check(ctx context.Context) (bool, error) {
	cur, err := s.h.ReadFile("/etc/hostname")
	if err != nil {
		return false, err
	}
	hosts, err := s.h.ReadFile("/etc/hosts")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(cur) != s.name || hosts != withHostsEntry(hosts, s.name) {
		return false, nil
	}
	if s.h.Exists(cloudInitCfgDir) {
		return s.h.FileEquals(cloudInitHostCfg, cloudInitHost)
	}
	return true, nil
}

func (s hostnameStep) Apply(ctx context.Context) error {
	if s.h.Exists(cloudInitCfgDir) {
		if err := s.h.WriteFile(cloudInitHostCfg, cloudInitHost, 0o644); err != nil {
			return err
		}
	}
	if err := s.h.WriteFile("/etc/hostname", s.name+"\n", 0o644); err != nil {
		return err
	}
	hosts, err := s.h.ReadFile("/etc/hosts")
	if err != nil {
		return err
	}
	if err := s.h.WriteFile("/etc/hosts", withHostsEntry(hosts, s.name), 0o644); err != nil {
		return err
	}
	// Apply to the running kernel as well; hostnamectl needs systemd, `hostname` does not.
	_, err = s.h.Run.Run(ctx, "hostname", s.name)
	return err
}

// withHostsEntry returns hosts with exactly one "127.0.1.1 <name>" line, so sudo
// can resolve the new hostname.
func withHostsEntry(hosts, name string) string {
	want := "127.0.1.1 " + name
	trimmed := strings.TrimRight(hosts, "\n")
	if strings.TrimSpace(trimmed) == "" {
		return "127.0.0.1 localhost\n" + want + "\n"
	}
	var out []string
	found := false
	for _, l := range strings.Split(trimmed, "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == "127.0.1.1" {
			if !found {
				out = append(out, want)
				found = true
			}
			continue
		}
		out = append(out, l)
	}
	if !found {
		out = append(out, want)
	}
	return strings.Join(out, "\n") + "\n"
}

// ---- timezone ----

type timezoneStep struct {
	h  Host
	tz string
}

func (s timezoneStep) Name() string     { return "timezone" }
func (s timezoneStep) Describe() string { return "Saat dilimi: " + s.tz }

func (s timezoneStep) zoneFile() string { return "/usr/share/zoneinfo/" + s.tz }

func (s timezoneStep) Check(ctx context.Context) (bool, error) {
	if s.h.Readlink("/etc/localtime") != s.zoneFile() {
		return false, nil
	}
	return s.h.FileEquals("/etc/timezone", s.tz+"\n")
}

// Apply does what timedatectl does, without needing a running systemd.
func (s timezoneStep) Apply(ctx context.Context) error {
	if !s.h.Exists(s.zoneFile()) {
		return fmt.Errorf("saat dilimi bulunamadı: %s", s.tz)
	}
	if err := s.h.Symlink(s.zoneFile(), "/etc/localtime"); err != nil {
		return err
	}
	return s.h.WriteFile("/etc/timezone", s.tz+"\n", 0o644)
}

// ---- swap ----

const (
	swapFile       = "/swapfile"
	swapSysctlPath = "/etc/sysctl.d/99-footops.conf"
	swapSysctl     = "# Managed by FootOps\nvm.swappiness=10\n"
	fstabSwapLine  = "/swapfile none swap sw 0 0"
)

type swapStep struct {
	h    Host
	size uint64
}

func (s swapStep) Name() string { return "swap" }
func (s swapStep) Describe() string {
	return fmt.Sprintf("Swap: %s, vm.swappiness=10", sysinfo.HumanBytes(s.size))
}

// enough tolerates the few KiB mkswap reserves for its header.
func (s swapStep) enough(total uint64) bool {
	return total >= s.size-s.size/50
}

func (s swapStep) Check(ctx context.Context) (bool, error) {
	mem, err := s.mem()
	if err != nil {
		return false, err
	}
	sysctlOK, err := s.h.FileEquals(swapSysctlPath, swapSysctl)
	if err != nil {
		return false, err
	}
	fstab, err := s.h.ReadFile("/etc/fstab")
	if err != nil {
		return false, err
	}
	// Swap provided by something else (e.g. a provider partition) also counts; our own
	// swap file must survive a reboot.
	persistent := !swapfileActive(s.h) || hasLine(fstab, fstabSwapLine)
	return s.enough(mem.SwapTotal) && sysctlOK && persistent, nil
}

func (s swapStep) Apply(ctx context.Context) error {
	mem, err := s.mem()
	if err != nil {
		return err
	}
	if !s.enough(mem.SwapTotal) {
		// Only our own swap file is resized; provider swap partitions are left alone
		// and count towards the target.
		own, active := swapfileSize(s.h)
		otherSwap := mem.SwapTotal - min(own, mem.SwapTotal)
		if active {
			if _, err := s.h.Run.Run(ctx, "swapoff", swapFile); err != nil {
				return err
			}
		}
		// A stale file would keep its old size: fallocate never shrinks.
		if err := s.h.Remove(swapFile); err != nil {
			return err
		}
		if err := s.createSwapfile(ctx, s.size-min(otherSwap, s.size)); err != nil {
			return err
		}
	}
	// Only an active swap file goes into fstab; a stale one would fail at boot.
	if swapfileActive(s.h) {
		fstab, err := s.h.ReadFile("/etc/fstab")
		if err != nil {
			return err
		}
		if !hasLine(fstab, fstabSwapLine) {
			if fstab != "" && !strings.HasSuffix(fstab, "\n") {
				fstab += "\n"
			}
			if err := s.h.WriteFile("/etc/fstab", fstab+fstabSwapLine+"\n", 0o644); err != nil {
				return err
			}
		}
	}
	if err := s.h.WriteFile(swapSysctlPath, swapSysctl, 0o644); err != nil {
		return err
	}
	_, err = s.h.Run.Run(ctx, "sysctl", "-p", swapSysctlPath)
	return err
}

func (s swapStep) createSwapfile(ctx context.Context, size uint64) error {
	if _, err := s.h.Run.Run(ctx, "fallocate", "-l", strconv.FormatUint(size, 10), swapFile); err != nil {
		// Some filesystems cannot use fallocated files for swap; dd always works.
		mib := strconv.FormatUint((size+sysinfo.MiB-1)/sysinfo.MiB, 10)
		if _, err := s.h.Run.Run(ctx, "dd", "if=/dev/zero", "of="+swapFile, "bs=1M", "count="+mib, "status=none"); err != nil {
			return err
		}
	}
	for _, cmd := range [][]string{
		{"chmod", "600", swapFile},
		{"mkswap", swapFile},
		{"swapon", swapFile},
	} {
		if _, err := s.h.Run.Run(ctx, cmd[0], cmd[1:]...); err != nil {
			return err
		}
	}
	return nil
}

func (s swapStep) mem() (sysinfo.Memory, error) {
	f, err := os.Open(s.h.Path("/proc/meminfo"))
	if err != nil {
		return sysinfo.Memory{}, err
	}
	defer f.Close()
	return sysinfo.ParseMeminfo(f)
}

func swapfileActive(h Host) bool {
	_, active := swapfileSize(h)
	return active
}

// swapfileSize returns the size of /swapfile in bytes if it is active in /proc/swaps.
func swapfileSize(h Host) (uint64, bool) {
	swaps, _ := h.ReadFile("/proc/swaps")
	for _, l := range strings.Split(swaps, "\n") {
		// Filename Type Size(KiB) Used Priority
		f := strings.Fields(l)
		if len(f) >= 3 && f[0] == swapFile {
			kib, _ := strconv.ParseUint(f[2], 10, 64)
			return kib * 1024, true
		}
	}
	return 0, false
}

// hasLine reports whether content has line, ignoring differences in whitespace.
func hasLine(content, line string) bool {
	for _, l := range strings.Split(content, "\n") {
		if strings.Join(strings.Fields(l), " ") == line {
			return true
		}
	}
	return false
}

// ---- unattended-upgrades ----

const (
	autoUpgradesPath = "/etc/apt/apt.conf.d/20auto-upgrades"
	autoUpgrades     = "APT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n"
	unattendedPath   = "/etc/apt/apt.conf.d/52footops-unattended"
)

type unattendedStep struct {
	h          Host
	autoReboot bool
	rebootTime string
}

func (s unattendedStep) Name() string { return "unattended-upgrades" }
func (s unattendedStep) Describe() string {
	if s.autoReboot {
		return "Otomatik güvenlik güncellemeleri, gerekirse saat " + s.rebootTime + " reboot"
	}
	return "Otomatik güvenlik güncellemeleri, otomatik reboot yok"
}

func (s unattendedStep) config() string {
	return fmt.Sprintf(`// Managed by FootOps
Unattended-Upgrade::Automatic-Reboot "%t";
Unattended-Upgrade::Automatic-Reboot-Time "%s";
Unattended-Upgrade::Remove-Unused-Kernel-Packages "true";
`, s.autoReboot, s.rebootTime)
}

func (s unattendedStep) Check(ctx context.Context) (bool, error) {
	a, err := s.h.FileEquals(autoUpgradesPath, autoUpgrades)
	if err != nil {
		return false, err
	}
	b, err := s.h.FileEquals(unattendedPath, s.config())
	return a && b, err
}

func (s unattendedStep) Apply(ctx context.Context) error {
	if err := s.h.WriteFile(autoUpgradesPath, autoUpgrades, 0o644); err != nil {
		return err
	}
	return s.h.WriteFile(unattendedPath, s.config(), 0o644)
}
