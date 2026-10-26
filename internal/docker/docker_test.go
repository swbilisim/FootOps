package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// handlerDoer sends requests straight to an http.Handler.
type handlerDoer struct{ h http.Handler }

func (d handlerDoer) Do(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	d.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func frame(stream byte, s string) []byte {
	b := make([]byte, 8+len(s))
	b[0] = stream
	binary.BigEndian.PutUint32(b[4:], uint32(len(s)))
	copy(b[8:], s)
	return b
}

func TestDemux(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(1, "2026-10-26T01:00:00Z hello\n"))
	in.Write(frame(2, "2026-10-26T01:00:01Z oops "))
	in.Write(frame(2, "split\n"))
	lines, err := demux(&in)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-10-26T01:00:00Z hello", "2026-10-26T01:00:01Z oops split"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Errorf("lines = %q", lines)
	}
	// Truncated frame.
	if _, err := demux(bytes.NewReader(frame(1, "abc")[:9])); err == nil {
		t.Error("truncated frame accepted")
	}
}

func TestStatsFormula(t *testing.T) {
	var r rawStats
	r.CPUStats.CPUUsage.TotalUsage = 2_000_000_000
	r.PreCPUStats.CPUUsage.TotalUsage = 1_000_000_000
	r.CPUStats.SystemUsage = 20_000_000_000
	r.PreCPUStats.SystemUsage = 10_000_000_000
	r.CPUStats.OnlineCPUs = 2
	r.MemoryStats.Usage = 300 << 20
	r.MemoryStats.Limit = 1 << 30
	r.MemoryStats.Stats = map[string]uint64{"inactive_file": 100 << 20}
	s := r.toStats()
	if s.CPUPercent != 20 {
		t.Errorf("cpu = %v, want 20", s.CPUPercent)
	}
	if s.MemUsage != 200<<20 || s.MemLimit != 1<<30 {
		t.Errorf("mem = %d / %d", s.MemUsage, s.MemLimit)
	}
}

func TestClientRejectsInvalidNames(t *testing.T) {
	called := false
	c := &Client{Doer: handlerDoer{http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })}}
	ctx := context.Background()
	for _, bad := range []string{"../images/json", "a/b", "", "-x", "name?x=1", strings.Repeat("a", 200)} {
		if err := c.Restart(ctx, bad, time.Second); err == nil {
			t.Errorf("Restart(%q) accepted", bad)
		}
		if _, err := c.Logs(ctx, bad, 10); err == nil {
			t.Errorf("Logs(%q) accepted", bad)
		}
	}
	if called {
		t.Error("an invalid name reached dockerd")
	}
}

func TestClientRequests(t *testing.T) {
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.String())
		io.WriteString(w, `[{"Id":"abc123","Names":["/footops-caddy"],"State":"running","Status":"Up 1 hour (healthy)"}]`)
	})
	mux.HandleFunc("/containers/footops-caddy/restart", func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.String())
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/containers/missing/json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"No such container: missing"}`)
	})
	c := &Client{Doer: handlerDoer{mux}}
	ctx := context.Background()

	cs, err := c.Containers(ctx)
	if err != nil || len(cs) != 1 || cs[0].Name() != "footops-caddy" {
		t.Fatalf("containers = %+v, %v", cs, err)
	}
	if err := c.Restart(ctx, "footops-caddy", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Inspect(ctx, "missing"); !IsNotFound(err) {
		t.Errorf("err = %v, want not found", err)
	}
	want := []string{"GET http://docker/containers/json?all=1", "POST http://docker/containers/footops-caddy/restart?t=10"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s", strings.Join(got, "\n"))
	}
}

func TestNetworkRequests(t *testing.T) {
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("/networks/prj_musteri_a", func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"network not found"}`)
		}
	})
	mux.HandleFunc("/networks/create", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, "POST create "+string(b))
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/networks/prj_musteri_a/connect", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, "POST connect "+string(b))
	})
	c := &Client{Doer: handlerDoer{mux}}
	ctx := context.Background()
	if err := c.CreateNetwork(ctx, "prj_musteri_a", map[string]string{"footops.site": "a"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(ctx, "prj_musteri_a", "footops-mongo", "mongo"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveNetwork(ctx, "prj_musteri_a"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{`"Driver":"bridge"`, `"footops.site":"a"`, `"Container":"footops-mongo"`, `"Aliases":["mongo"]`, "DELETE /networks/prj_musteri_a"} {
		if !strings.Contains(joined, want) {
			t.Errorf("requests miss %s:\n%s", want, joined)
		}
	}
	if err := c.Connect(ctx, "../x", "a"); err == nil {
		t.Error("invalid network name accepted")
	}
}
