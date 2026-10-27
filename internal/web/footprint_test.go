package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/history"
)

// busyAgent reports 16 containers with a full day of history.
type busyAgent struct{ fakeAgent }

func (b *busyAgent) Overview(context.Context) (agent.Overview, error) {
	o := agent.Overview{SampledAt: time.Now(), System: agent.SystemMetrics{MemTotal: 4 << 30, MemUsed: 2 << 30, DiskTotal: 40 << 30, SwapTotal: 2 << 30}}
	for i := 0; i < 16; i++ {
		o.Containers = append(o.Containers, agent.ContainerInfo{Name: fmt.Sprintf("site-%02d", i), Image: "ghcr.io/x/y:main", State: "running", Project: "p", MemUsage: 200 << 20, MemLimit: 512 << 20})
	}
	return o, nil
}

func (b *busyAgent) History(_ context.Context, rng, name string) (agent.History, error) {
	r, _ := history.RangeByName(rng)
	now := time.Now()
	h := agent.History{Range: r.Name, Step: int64(r.Step / time.Second), From: now.Add(-r.Span), To: now, Containers: map[string][]history.Point{}}
	n := int(r.Span / r.Step)
	for i := 0; i < n; i++ {
		p := history.Point{T: now.Add(-r.Span + time.Duration(i)*r.Step).Unix(), CPU: float64(i % 37), CPUMax: 50, Mem: uint64(1000+i) << 20, MemMax: 2 << 30}
		h.Host = append(h.Host, p)
		for c := 0; c < 16; c++ {
			h.Containers[fmt.Sprintf("site-%02d", c)] = append(h.Containers[fmt.Sprintf("site-%02d", c)], p)
		}
	}
	return h, nil
}

// TestPageCost keeps page rendering cheap. Run with -v to see the numbers.
func TestPageCost(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement")
	}
	e := newEnv(t)
	e.srv.agent = &busyAgent{}
	c := e.client(t, false)
	e.login(t, c)
	for _, tc := range []struct{ name, path, live string }{
		{"dashboard (full)", "/?range=24h", ""},
		{"dashboard (light refresh)", "/", "1"},
		{"dashboard 7d", "/?range=7d", "full"},
		{"container page", "/containers/site-01", "full"},
	} {
		c.header = nil
		if tc.live != "" {
			c.header = http.Header{liveHeader: {tc.live}}
		}
		var size int
		n := 50
		start := time.Now()
		for i := 0; i < n; i++ {
			_, body := c.do("GET", tc.path, nil)
			size = len(body)
		}
		t.Logf("%-26s %6.1f KB  %v per request (incl. test client)", tc.name, float64(size)/1024, (time.Since(start) / time.Duration(n)).Round(10*time.Microsecond))
	}
}

// bigDownload serves a 64 MB copy without holding it anywhere.
type bigDownload struct{ fakeAgent }

const bigSize = 64 << 20

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func (b *bigDownload) DownloadSnapshot(context.Context, string, string, agent.Actor, http.Header) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Length": {fmt.Sprint(bigSize)}},
		Body: io.NopCloser(io.LimitReader(zeros{}, bigSize))}, nil
}

// TestDownloadStreams guards rule K7: the panel passes a download through without
// buffering it. Allocations stay a tiny fraction of the file.
func TestDownloadStreams(t *testing.T) {
	e := newEnv(t)
	e.srv.agent = &bigDownload{}
	c := e.client(t, false)
	e.login(t, c)
	req, _ := http.NewRequest("GET", c.base+"/sites/enisparta/snapshots/20261027-110000/download", nil)
	req.Host = c.host
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	resp, err := c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("64 MB download: %d bytes allocated in the whole process", alloc)
	if n != bigSize || alloc > 4<<20 {
		t.Errorf("read %d bytes, allocated %d (limit 4 MB)", n, alloc)
	}
}
