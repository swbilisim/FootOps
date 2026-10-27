package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/run"
)

func TestSiteShowsSnapshots(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	e.login(t, c)

	_, body := c.do("GET", "/sites/enisparta", nil)
	if strings.Contains(body, "Veritabanı kopyaları") {
		t.Error("copies section on a site without a database")
	}
	e.agent.dbSite = true
	_, body = c.do("GET", "/sites/enisparta", nil)
	if !strings.Contains(body, "Veritabanı kopyaları") || !strings.Contains(body, "Henüz kopya yok") ||
		!strings.Contains(body, `action="/sites/enisparta/snapshots"`) {
		t.Errorf("empty section missing:\n%s", body)
	}
	e.agent.snapshots = []agent.Snapshot{{ID: "20261027-110000", Site: "enisparta", Created: time.Now().Add(-time.Hour),
		Bytes: 8806, Documents: 89, Collections: map[string]int64{"a": 80, "b": 9}, Actor: "furkan",
		Usage: run.Usage{PeakMem: 40 << 20, Wall: 1500 * time.Millisecond}}}
	_, body = c.do("GET", "/sites/enisparta", nil)
	for _, want := range []string{"2 koleksiyon, 89 belge", "8.6 KiB", "1 sa önce", "1.5 sn", "en çok 40.0 MiB",
		`href="/sites/enisparta/snapshots/20261027-110000/download"`} {
		if !strings.Contains(body, want) {
			t.Errorf("site page lacks %q", want)
		}
	}
}

func TestTakeSnapshotAndCancel(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	e.agent.dbSite = true

	resp, _ := c.do("POST", "/sites/enisparta/snapshots", url.Values{})
	if resp.StatusCode != http.StatusForbidden || len(e.agent.taken) != 0 {
		t.Fatalf("no CSRF: %d", resp.StatusCode)
	}
	resp, _ = c.do("POST", "/sites/enisparta/snapshots", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/jobs/jobsnap" || e.agent.taken[0].User != "furkan" {
		t.Fatalf("take: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	e.agent.job = &agent.Job{ID: "jobsnap", Kind: "db.snapshot", Target: "enisparta", Cancellable: true, Bytes: 3 << 20,
		Started: time.Now(), Steps: []string{"… mongodump çalışıyor"}}
	_, body := c.do("GET", "/jobs/jobsnap", nil)
	if !strings.Contains(body, "Veritabanı kopyası: enisparta") || !strings.Contains(body, "3.0 MiB") ||
		!strings.Contains(body, `action="/jobs/jobsnap/cancel"`) || !strings.Contains(body, `href="/sites/enisparta"`) {
		t.Errorf("running job page:\n%s", body)
	}
	resp, _ = c.do("POST", "/jobs/jobsnap/cancel", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusSeeOther || len(e.agent.cancelled) != 1 {
		t.Errorf("cancel: %d %v", resp.StatusCode, e.agent.cancelled)
	}
	resp, _ = c.do("POST", "/jobs/..%2fx/cancel", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusNotFound || len(e.agent.cancelled) != 1 {
		t.Errorf("bad id: %d", resp.StatusCode)
	}

	finished := time.Now()
	e.agent.job = &agent.Job{ID: "jobsnap", Kind: "db.snapshot", Target: "enisparta", Cancellable: true, Done: true,
		Started: finished.Add(-2 * time.Second), Finished: finished, Result: "20261027-110000",
		Usage: run.Usage{PeakMem: 30 << 20, CPU: 400 * time.Millisecond}}
	_, body = c.do("GET", "/jobs/jobsnap", nil)
	if !strings.Contains(body, "tamamlandı") || !strings.Contains(body, "/sites/enisparta/snapshots/20261027-110000/download") ||
		!strings.Contains(body, "en çok 30.0 MiB bellek, 400 ms CPU") || strings.Contains(body, "/cancel") {
		t.Errorf("finished job page:\n%s", body)
	}
	e.agent.job.Cancelled, e.agent.job.Err, e.agent.job.Result = true, "iptal edildi", ""
	if _, body = c.do("GET", "/jobs/jobsnap", nil); !strings.Contains(body, "iptal edildi") || strings.Contains(body, "/download") {
		t.Errorf("cancelled job page:\n%s", body)
	}
}

func TestSnapshotDownloadStreamsWithRange(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	e.login(t, c)

	resp, body := c.do("GET", "/sites/enisparta/snapshots/20261027-110000/download", nil)
	if resp.StatusCode != 200 || body != snapshotBody || resp.Header.Get("Content-Type") != "application/gzip" ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") || resp.Header.Get("Accept-Ranges") != "bytes" ||
		resp.Header.Get("X-Content-Sha256") != "abc" || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("download: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if e.agent.dlActors[0].User != "furkan" {
		t.Errorf("actor = %+v", e.agent.dlActors[0])
	}
	c.header = http.Header{"Range": {"bytes=10-"}}
	resp, body = c.do("GET", "/sites/enisparta/snapshots/20261027-110000/download", nil)
	c.header = nil
	if resp.StatusCode != http.StatusPartialContent || body != snapshotBody[10:] || resp.Header.Get("Content-Range") != "bytes 10-19/20" {
		t.Errorf("range: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp, _ := c.do("HEAD", "/sites/enisparta/snapshots/20261027-110000/download", nil); resp.StatusCode != http.StatusMethodNotAllowed || len(e.agent.downloads) != 2 {
		t.Errorf("HEAD: %d, %d agent calls", resp.StatusCode, len(e.agent.downloads))
	}
	for _, p := range []string{"/sites/enisparta/snapshots/20200101-000000/download", "/sites/enisparta/snapshots/..%2f..%2fx/download", "/sites/yok/snapshots/20261027-110000/download"} {
		if resp, _ := c.do("GET", p, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
}
