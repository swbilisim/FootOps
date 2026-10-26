package sysinfo

import (
	"strings"
	"testing"
	"time"
)

func TestCPUPercent(t *testing.T) {
	// user nice system idle iowait irq softirq steal guest guest_nice
	a, err := ParseCPUTimes(strings.NewReader("cpu  100 0 100 700 100 0 0 0 50 0\ncpu0 1 2 3 4 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Total != 1000 || a.Busy != 200 {
		t.Fatalf("times = %+v (guest must not be counted twice)", a)
	}
	b, _ := ParseCPUTimes(strings.NewReader("cpu  200 0 200 1300 300 0 0 0 0 0\n"))
	if got := CPUPercent(a, b); got != 20 {
		t.Errorf("cpu = %v, want 20", got)
	}
	if CPUPercent(b, a) != 0 {
		t.Error("counter going backwards must give 0")
	}
	if _, err := ParseCPUTimes(strings.NewReader("intr 1 2 3\n")); err == nil {
		t.Error("missing cpu line accepted")
	}
}

func TestParseLoadAndUptime(t *testing.T) {
	l, err := ParseLoadavg("0.52 0.31 0.20 1/234 5678\n")
	if err != nil || l.One != 0.52 || l.Fifteen != 0.20 {
		t.Errorf("load = %+v, %v", l, err)
	}
	u, err := ParseUptime("3600.50 7000.00\n")
	if err != nil || u != time.Hour+500*time.Millisecond {
		t.Errorf("uptime = %v, %v", u, err)
	}
}

func TestReadDisk(t *testing.T) {
	d, err := ReadDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if d.Total == 0 || d.Used > d.Total || d.Percent() < 0 || d.Percent() > 100 {
		t.Errorf("disk = %+v", d)
	}
}
