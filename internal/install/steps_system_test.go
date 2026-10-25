package install

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

func TestAptRetriesWhileLocked(t *testing.T) {
	noSleep(t)
	attempts := 0
	h, f := newTestHost(t, func(cmd string) (string, error) {
		if strings.HasPrefix(cmd, "apt-get") {
			attempts++
			if attempts < 3 {
				return "E: Could not get lock /var/lib/apt/lists/lock. It is held by process 812 (apt-get)", errExit
			}
		}
		return "", nil
	})
	if err := apt(ctx, h, "update"); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if !h.Exists(aptUpdateStamp) {
		t.Error("successful update not stamped")
	}
	if !strings.Contains(f.calls[0].cmd, "DPkg::Lock::Timeout=600") {
		t.Errorf("lock timeout missing: %s", f.calls[0].cmd)
	}
}

func TestAptDoesNotRetryOtherErrors(t *testing.T) {
	noSleep(t)
	h, f := newTestHost(t, func(string) (string, error) { return "E: Unable to locate package nope", errExit })
	if err := apt(ctx, h, "install", "nope"); err == nil {
		t.Fatal("expected error")
	}
	if len(f.calls) != 1 {
		t.Errorf("calls = %d, want 1", len(f.calls))
	}
}

func TestPackagesApplyRepairsDpkgFirst(t *testing.T) {
	h, f := newTestHost(t, nil)
	if err := (packagesStep{h}).Apply(ctx); err != nil {
		t.Fatal(err)
	}
	cmds := f.cmds()
	if !strings.HasPrefix(cmds[0], "dpkg --configure -a") {
		t.Errorf("first command = %q, want dpkg --configure -a", cmds[0])
	}
	last := cmds[len(cmds)-1]
	for _, p := range basePackages {
		if !strings.Contains(last, p) {
			t.Errorf("install command misses %s: %s", p, last)
		}
	}
}

func TestUpgradeCheck(t *testing.T) {
	sim := "Inst libc6 [2.39-0ubuntu8] (2.39-0ubuntu8.3 Ubuntu:24.04/noble-updates)\nConf libc6\nInst curl [8.5.0] (8.5.0-2ubuntu10.4)\n"
	h, _ := newTestHost(t, func(cmd string) (string, error) {
		if strings.Contains(cmd, "-s --with-new-pkgs upgrade") {
			return sim, nil
		}
		return "", nil
	})
	// No package cache: lists must be refreshed first, so the step is pending.
	if done, _ := (upgradeStep{h}).Check(ctx); done {
		t.Error("done without package cache")
	}
	put(t, h, "/var/cache/apt/pkgcache.bin", "x")
	if done, _ := (upgradeStep{h}).Check(ctx); done {
		t.Error("done with 2 pending upgrades")
	}
	sim = "Reading package lists...\n0 upgraded, 0 newly installed\n"
	if done, _ := (upgradeStep{h}).Check(ctx); !done {
		t.Error("pending with nothing to upgrade")
	}
	old := time.Now().Add(-2 * aptListsMaxAge)
	if err := os.Chtimes(h.Path("/var/cache/apt/pkgcache.bin"), old, old); err != nil {
		t.Fatal(err)
	}
	if done, _ := (upgradeStep{h}).Check(ctx); done {
		t.Error("done with stale package lists")
	}
	// Our own stamp counts even when apt's cache file is gone (Docker images delete it).
	_ = os.Remove(h.Path("/var/cache/apt/pkgcache.bin"))
	put(t, h, aptUpdateStamp, "now")
	if done, _ := (upgradeStep{h}).Check(ctx); !done {
		t.Error("fresh stamp ignored")
	}
}

func TestUpgradeApplyInstallsNewDependencies(t *testing.T) {
	h, f := newTestHost(t, nil)
	if err := (upgradeStep{h}).Apply(ctx); err != nil {
		t.Fatal(err)
	}
	last := f.calls[len(f.calls)-1].cmd
	if !strings.Contains(last, "--with-new-pkgs upgrade") || !strings.Contains(last, "--force-confold") {
		t.Errorf("upgrade command = %q", last)
	}
}

func TestWithHostsEntry(t *testing.T) {
	cases := []struct{ in, want string }{
		{"127.0.0.1 localhost\n127.0.1.1 ubuntu-4gb-nbg1-2 ubuntu-4gb-nbg1-2\n::1 ip6-localhost\n",
			"127.0.0.1 localhost\n127.0.1.1 h1\n::1 ip6-localhost\n"},
		{"127.0.0.1 localhost\n", "127.0.0.1 localhost\n127.0.1.1 h1\n"},
		{"", "127.0.0.1 localhost\n127.0.1.1 h1\n"},
		{"127.0.1.1 a\n127.0.1.1 b\n", "127.0.1.1 h1\n"},
	}
	for _, c := range cases {
		if got := withHostsEntry(c.in, "h1"); got != c.want {
			t.Errorf("withHostsEntry(%q) = %q, want %q", c.in, got, c.want)
		}
		// Idempotent: applying twice changes nothing.
		once := withHostsEntry(c.in, "h1")
		if withHostsEntry(once, "h1") != once {
			t.Errorf("not idempotent for %q", c.in)
		}
	}
}

func TestHostnameStep(t *testing.T) {
	h, f := newTestHost(t, nil)
	put(t, h, "/etc/hostname", "ubuntu-4gb-nbg1-2\n")
	put(t, h, "/etc/hosts", "127.0.0.1 localhost\n127.0.1.1 ubuntu-4gb-nbg1-2\n")
	put(t, h, cloudInitCfgDir+"/90-hetzner.cfg", "")
	s := hostnameStep{h, "h1"}
	if done, _ := s.Check(ctx); done {
		t.Fatal("done before apply")
	}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if done, err := s.Check(ctx); !done || err != nil {
		t.Fatalf("not done after apply: %v", err)
	}
	if read(t, h, cloudInitHostCfg) != cloudInitHost {
		t.Error("cloud-init would reset the hostname on reboot")
	}
	if !f.ran("hostname h1") {
		t.Error("running hostname not updated")
	}
}

func TestHostnameStepWithoutCloudInit(t *testing.T) {
	h, _ := newTestHost(t, nil)
	put(t, h, "/etc/hosts", "127.0.0.1 localhost\n")
	s := hostnameStep{h, "h1"}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if h.Exists(cloudInitHostCfg) {
		t.Error("cloud-init config written on a host without cloud-init")
	}
	if done, _ := s.Check(ctx); !done {
		t.Error("not done after apply")
	}
}

func TestTimezoneStep(t *testing.T) {
	h, _ := newTestHost(t, nil)
	s := timezoneStep{h, "Europe/Istanbul"}
	if err := s.Apply(ctx); err == nil {
		t.Fatal("applied a timezone whose zoneinfo file is missing")
	}
	put(t, h, "/usr/share/zoneinfo/Europe/Istanbul", "TZif")
	put(t, h, "/etc/localtime", "old regular file")
	if done, _ := s.Check(ctx); done {
		t.Fatal("done before apply")
	}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.Readlink("/etc/localtime"); got != "/usr/share/zoneinfo/Europe/Istanbul" {
		t.Errorf("localtime -> %q", got)
	}
	if done, _ := s.Check(ctx); !done {
		t.Error("not done after apply")
	}
}

// swapHost simulates the kernel: swapon/swapoff update /proc/swaps and /proc/meminfo.
func swapHost(t *testing.T, ramKiB, providerSwapKiB, swapfileKiB uint64) (Host, *fakeRunner) {
	t.Helper()
	var h Host
	var size uint64 // bytes requested by fallocate
	write := func(fileKiB uint64) {
		swaps := "Filename\tType\tSize\tUsed\tPriority\n"
		if providerSwapKiB > 0 {
			swaps += "/dev/sda2 partition " + itoa(providerSwapKiB) + " 0 -2\n"
		}
		if fileKiB > 0 {
			swaps += "/swapfile file " + itoa(fileKiB) + " 0 -3\n"
		}
		put(t, h, "/proc/swaps", swaps)
		put(t, h, "/proc/meminfo", "MemTotal: "+itoa(ramKiB)+" kB\nSwapTotal: "+itoa(providerSwapKiB+fileKiB)+" kB\n")
	}
	h, f := newTestHost(t, func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "fallocate -l "):
			size, _ = strconv.ParseUint(strings.Fields(cmd)[2], 10, 64)
		case cmd == "swapoff /swapfile":
			write(0)
		case cmd == "swapon /swapfile":
			write(size/1024 - 4) // mkswap header
		}
		return "", nil
	})
	write(swapfileKiB)
	return h, f
}

func TestSwapCreatesFile(t *testing.T) {
	h, f := swapHost(t, 3906588, 0, 0)
	s := swapStep{h, 2 << 30}
	if done, _ := s.Check(ctx); done {
		t.Fatal("done without swap")
	}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.ran("fallocate -l 2147483648 /swapfile") || !f.ran("mkswap /swapfile") || !f.ran("swapon /swapfile") {
		t.Errorf("unexpected commands: %v", f.cmds())
	}
	if !hasLine(read(t, h, "/etc/fstab"), fstabSwapLine) {
		t.Error("swap not persisted in fstab")
	}
	if read(t, h, swapSysctlPath) != swapSysctl {
		t.Error("swappiness not configured")
	}
	if done, _ := s.Check(ctx); !done {
		t.Error("not done after apply")
	}
}

func TestSwapResizesOwnFileOnly(t *testing.T) {
	// 1 GiB provider partition + 512 MiB swapfile, target 2 GiB -> swapfile becomes 1 GiB.
	h, f := swapHost(t, 3906588, 1<<20, 512<<10)
	put(t, h, "/etc/fstab", "UUID=abc / ext4 defaults 0 1\n/swapfile none swap sw 0 0\n")
	s := swapStep{h, 2 << 30}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.ran("swapoff /swapfile") {
		t.Error("old swapfile not turned off")
	}
	if !f.ran("fallocate -l 1073741824 /swapfile") {
		t.Errorf("swapfile not sized to the remaining 1 GiB: %v", f.cmds())
	}
	if strings.Count(read(t, h, "/etc/fstab"), "/swapfile") != 1 {
		t.Error("fstab swap line duplicated")
	}
}

func TestSwapEnoughProviderSwap(t *testing.T) {
	h, f := swapHost(t, 3906588, 2<<20, 0)
	s := swapStep{h, 2 << 30}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if f.ran("fallocate") || f.ran("dd") {
		t.Error("swapfile created although provider swap is enough")
	}
	if h.Exists("/etc/fstab") {
		t.Error("fstab touched without an active swapfile")
	}
}

func TestSwapFallsBackToDD(t *testing.T) {
	h, _ := swapHost(t, 3906588, 0, 0)
	var cmds []string
	inner := h.Run.(*fakeRunner).handle
	h.Run.(*fakeRunner).handle = func(cmd string) (string, error) {
		cmds = append(cmds, cmd)
		if strings.HasPrefix(cmd, "fallocate") {
			return "fallocate: fallocate failed: Operation not supported", errExit
		}
		if strings.HasPrefix(cmd, "dd ") {
			_, _ = inner("fallocate -l 2147483648 /swapfile")
		}
		return inner(cmd)
	}
	if err := (swapStep{h, 2 << 30}).Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cmds, "\n"), "dd if=/dev/zero of=/swapfile bs=1M count=2048") {
		t.Errorf("dd fallback not used: %v", cmds)
	}
}

func TestUnattendedStep(t *testing.T) {
	h, _ := newTestHost(t, nil)
	s := unattendedStep{h, true, "04:00"}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	conf := read(t, h, unattendedPath)
	if !strings.Contains(conf, `Automatic-Reboot "true"`) || !strings.Contains(conf, `Automatic-Reboot-Time "04:00"`) {
		t.Errorf("config = %s", conf)
	}
	if done, _ := s.Check(ctx); !done {
		t.Error("not done after apply")
	}
	if done, _ := (unattendedStep{h, false, "04:00"}).Check(ctx); done {
		t.Error("changed answer not detected")
	}
}

func itoa(v uint64) string { return strconv.FormatUint(v, 10) }
