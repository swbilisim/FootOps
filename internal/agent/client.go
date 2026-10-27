package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/swbilisim/FootOps/internal/datafs"
	"github.com/swbilisim/FootOps/internal/sites"
)

// Doer sends HTTP requests; *http.Client implements it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client is used by the web process to talk to the agent.
type Client struct {
	Doer Doer
	// Long is used for downloads, which may take longer than Doer's timeout; the
	// caller's context bounds them instead. Nil uses Doer.
	Long Doer
}

func NewClient(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{Doer: &http.Client{Transport: tr, Timeout: 60 * time.Second}, Long: &http.Client{Transport: tr}}
}

// Error is an error reported by the agent. Problems lists validation issues.
type Error struct {
	Status   int
	Message  string
	Problems []string
}

func (e *Error) Error() string { return e.Message }

func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Doer.Do(req)
	if err != nil {
		return fmt.Errorf("agent'a ulaşılamadı: %w", err)
	}
	return decode(resp, out)
}

// decode reads an agent response into out, or its error.
func decode(resp *http.Response, out any) error {
	defer drain(resp.Body)
	if resp.StatusCode/100 != 2 {
		var e errorResponse
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return &Error{Status: resp.StatusCode, Message: e.Error, Problems: e.Problems}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) long() Doer {
	if c.Long != nil {
		return c.Long
	}
	return c.Doer
}

// send streams body (length bytes, -1 if unknown) to the agent without buffering it.
// The caller's context bounds it, not a client timeout: a chunk may take minutes.
func (c *Client) send(ctx context.Context, method, path string, hdr http.Header, body io.Reader, length int64, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, body)
	if err != nil {
		return err
	}
	req.ContentLength = length
	if length == 0 {
		req.Body = http.NoBody
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := c.long().Do(req)
	if err != nil {
		return fmt.Errorf("agent'a ulaşılamadı: %w", err)
	}
	return decode(resp, out)
}

func (c *Client) Overview(ctx context.Context) (Overview, error) {
	var o Overview
	err := c.call(ctx, http.MethodGet, "/v1/overview", nil, &o)
	return o, err
}

// History returns metric history for a range ("1h", "6h", "24h", "7d"). With a container
// name it returns only that container.
func (c *Client) History(ctx context.Context, rng, container string) (History, error) {
	q := url.Values{"range": {rng}}
	if container != "" {
		q.Set("container", container)
	}
	var h History
	err := c.call(ctx, http.MethodGet, "/v1/history?"+q.Encode(), nil, &h)
	return h, err
}

// Alerts returns the active alerts, most severe first.
func (c *Client) Alerts(ctx context.Context) ([]Alert, error) {
	var out []Alert
	err := c.call(ctx, http.MethodGet, "/v1/alerts", nil, &out)
	return out, err
}

func (c *Client) Restart(ctx context.Context, name string, a Actor) error {
	return c.call(ctx, http.MethodPost, "/v1/containers/"+url.PathEscape(name)+"/restart", RestartRequest{Actor: a}, nil)
}

func (c *Client) Logs(ctx context.Context, name string, tail int) ([]string, error) {
	var r LogsResponse
	err := c.call(ctx, http.MethodGet, "/v1/containers/"+url.PathEscape(name)+"/logs?tail="+strconv.Itoa(tail), nil, &r)
	return r.Lines, err
}

func (c *Client) RecordAuth(ctx context.Context, e AuditEntry) error {
	return c.call(ctx, http.MethodPost, "/v1/audit", e, nil)
}

func (c *Client) AuditTail(ctx context.Context, limit int) ([]AuditEntry, error) {
	var out []AuditEntry
	err := c.call(ctx, http.MethodGet, "/v1/audit?limit="+strconv.Itoa(limit), nil, &out)
	return out, err
}

func (c *Client) Sites(ctx context.Context) ([]SiteStatus, error) {
	var out []SiteStatus
	err := c.call(ctx, http.MethodGet, "/v1/sites", nil, &out)
	return out, err
}

func (c *Client) CreateSite(ctx context.Context, req CreateSiteRequest) (string, error) {
	var r JobResponse
	err := c.call(ctx, http.MethodPost, "/v1/sites", req, &r)
	return r.JobID, err
}

// SiteJob starts "redeploy" or "remove" for a site.
func (c *Client) SiteJob(ctx context.Context, name, verb string, a Actor) (string, error) {
	var r JobResponse
	err := c.call(ctx, http.MethodPost, "/v1/sites/"+url.PathEscape(name)+"/"+verb, RestartRequest{Actor: a}, &r)
	return r.JobID, err
}

// UpdateSite starts a site update job.
func (c *Client) UpdateSite(ctx context.Context, name string, req UpdateSiteRequest) (string, error) {
	var r JobResponse
	err := c.call(ctx, http.MethodPost, "/v1/sites/"+url.PathEscape(name)+"/update", req, &r)
	return r.JobID, err
}

func (c *Client) Job(ctx context.Context, id string) (Job, error) {
	var j Job
	err := c.call(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(id), nil, &j)
	return j, err
}

// Snapshots lists a site's database copies, newest first.
func (c *Client) Snapshots(ctx context.Context, site string) ([]Snapshot, error) {
	var out []Snapshot
	err := c.call(ctx, http.MethodGet, "/v1/sites/"+url.PathEscape(site)+"/snapshots", nil, &out)
	return out, err
}

// TakeSnapshot starts a database copy job.
func (c *Client) TakeSnapshot(ctx context.Context, site string, a Actor) (string, error) {
	var r JobResponse
	err := c.call(ctx, http.MethodPost, "/v1/sites/"+url.PathEscape(site)+"/snapshots", RestartRequest{Actor: a}, &r)
	return r.JobID, err
}

// DownloadSnapshot opens a copy for streaming. Range and If-Range from hdr are passed on,
// so a broken download can resume. The caller closes the body.
func (c *Client) DownloadSnapshot(ctx context.Context, site, id string, a Actor, hdr http.Header) (*http.Response, error) {
	return c.download(ctx, "/v1/sites/"+url.PathEscape(site)+"/snapshots/"+url.PathEscape(id)+"/download", a, hdr)
}

// DownloadExport opens an export for streaming, like DownloadSnapshot.
func (c *Client) DownloadExport(ctx context.Context, site, id string, a Actor, hdr http.Header) (*http.Response, error) {
	return c.download(ctx, "/v1/sites/"+url.PathEscape(site)+"/exports/"+url.PathEscape(id)+"/download", a, hdr)
}

func (c *Client) download(ctx context.Context, path string, a Actor, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent"+path, nil)
	if err != nil {
		return nil, err
	}
	for _, k := range []string{"Range", "If-Range"} {
		if v := hdr.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	actor, _ := json.Marshal(a)
	req.Header.Set(ActorHeader, string(actor))
	resp, err := c.long().Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent'a ulaşılamadı: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		defer drain(resp.Body)
		var e errorResponse
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return nil, &Error{Status: resp.StatusCode, Message: e.Error}
	}
	return resp, nil
}

// CreateUpload announces a file to be sent in chunks.
func (c *Client) CreateUpload(ctx context.Context, site, name string, size int64, a Actor) (UploadStatus, error) {
	var st UploadStatus
	err := c.call(ctx, http.MethodPost, "/v1/sites/"+url.PathEscape(site)+"/uploads", CreateUploadRequest{Name: name, Size: size, Actor: a}, &st)
	return st, err
}

// Upload returns an upload's state (which chunks are missing).
func (c *Client) Upload(ctx context.Context, id string) (UploadStatus, error) {
	var st UploadStatus
	err := c.call(ctx, http.MethodGet, "/v1/uploads/"+url.PathEscape(id), nil, &st)
	return st, err
}

// PutChunk streams chunk n (length bytes) with its hex SHA-256.
func (c *Client) PutChunk(ctx context.Context, id string, n int, sum string, body io.Reader, length int64) (UploadStatus, error) {
	var st UploadStatus
	err := c.send(ctx, http.MethodPut, "/v1/uploads/"+url.PathEscape(id)+"/chunks/"+strconv.Itoa(n),
		http.Header{ChunkSumHeader: {sum}}, body, length, &st)
	return st, err
}

// FinishUpload closes an upload whose chunks are all in.
func (c *Client) FinishUpload(ctx context.Context, id string, a Actor) (UploadStatus, error) {
	var st UploadStatus
	err := c.call(ctx, http.MethodPost, "/v1/uploads/"+url.PathEscape(id)+"/finish", RestartRequest{Actor: a}, &st)
	return st, err
}

// RemoveUpload deletes an upload.
func (c *Client) RemoveUpload(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodDelete, "/v1/uploads/"+url.PathEscape(id), nil, nil)
}

// SingleUpload streams a whole file in one request (the panel without JavaScript).
func (c *Client) SingleUpload(ctx context.Context, site, name string, body io.Reader, a Actor) (UploadStatus, error) {
	var st UploadStatus
	actor, _ := json.Marshal(a)
	err := c.send(ctx, http.MethodPost, "/v1/sites/"+url.PathEscape(site)+"/uploads/single?"+url.Values{"name": {name}}.Encode(),
		http.Header{ActorHeader: {string(actor)}}, body, -1, &st)
	return st, err
}

// CheckImport starts checking an upload for the site's database.
func (c *Client) CheckImport(ctx context.Context, site, upload, sourceDB string, a Actor) (string, error) {
	var r JobResponse
	err := c.call(ctx, http.MethodPost, "/v1/sites/"+url.PathEscape(site)+"/imports",
		ImportRequest{Upload: upload, SourceDB: sourceDB, Actor: a}, &r)
	return r.JobID, err
}

// Import returns the result of an upload's latest check.
func (c *Client) Import(ctx context.Context, site, upload string) (ImportCheck, error) {
	var ic ImportCheck
	err := c.call(ctx, http.MethodGet, "/v1/sites/"+url.PathEscape(site)+"/imports/"+url.PathEscape(upload), nil, &ic)
	return ic, err
}

// ApplyImport starts applying a checked upload; confirm is the site's name as typed.
func (c *Client) ApplyImport(ctx context.Context, site, upload string, opt ApplyOptions, confirm string, a Actor) (string, error) {
	var r JobResponse
	err := c.call(ctx, http.MethodPost, "/v1/sites/"+url.PathEscape(site)+"/imports/"+url.PathEscape(upload)+"/apply",
		ApplyImportRequest{Options: opt, Confirm: confirm, Actor: a}, &r)
	return r.JobID, err
}

// FilesBackups lists the site's previous data directories.
func (c *Client) FilesBackups(ctx context.Context, site string) ([]FilesBackup, error) {
	var out []FilesBackup
	err := c.call(ctx, http.MethodGet, "/v1/sites/"+url.PathEscape(site)+"/files/backups", nil, &out)
	return out, err
}

// RestoreFiles starts putting a previous data directory back.
func (c *Client) RestoreFiles(ctx context.Context, site, id, confirm string, a Actor) (string, error) {
	var r JobResponse
	err := c.call(ctx, http.MethodPost, "/v1/sites/"+url.PathEscape(site)+"/files/backups/"+url.PathEscape(id)+"/restore",
		ConfirmRequest{Confirm: confirm, Actor: a}, &r)
	return r.JobID, err
}

// RestoreSnapshot starts putting one of the site's copies back.
func (c *Client) RestoreSnapshot(ctx context.Context, site, id, confirm string, a Actor) (string, error) {
	var r JobResponse
	err := c.call(ctx, http.MethodPost, "/v1/sites/"+url.PathEscape(site)+"/snapshots/"+url.PathEscape(id)+"/restore",
		ConfirmRequest{Confirm: confirm, Actor: a}, &r)
	return r.JobID, err
}

// Export starts making a FootOps archive of the site.
func (c *Client) Export(ctx context.Context, site string, a Actor) (string, error) {
	var r JobResponse
	err := c.call(ctx, http.MethodPost, "/v1/sites/"+url.PathEscape(site)+"/exports", RestartRequest{Actor: a}, &r)
	return r.JobID, err
}

// Exports lists the site's archives waiting for download.
func (c *Client) Exports(ctx context.Context, site string) ([]ExportInfo, error) {
	var out []ExportInfo
	err := c.call(ctx, http.MethodGet, "/v1/sites/"+url.PathEscape(site)+"/exports", nil, &out)
	return out, err
}

// CancelJob asks a running job to stop.
func (c *Client) CancelJob(ctx context.Context, id string, a Actor) error {
	return c.call(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/cancel", RestartRequest{Actor: a}, nil)
}

func (c *Client) Certs(ctx context.Context) ([]sites.Cert, error) {
	var out []sites.Cert
	err := c.call(ctx, http.MethodGet, "/v1/certs", nil, &out)
	return out, err
}

func (c *Client) InstallCert(ctx context.Context, certPEM, keyPEM string, a Actor) (sites.Cert, error) {
	var out sites.Cert
	err := c.call(ctx, http.MethodPost, "/v1/certs", InstallCertRequest{Cert: certPEM, Key: keyPEM, Actor: a}, &out)
	return out, err
}

func (c *Client) RemoveCert(ctx context.Context, id string, a Actor) error {
	return c.call(ctx, http.MethodPost, "/v1/certs/"+url.PathEscape(id)+"/remove", RestartRequest{Actor: a}, nil)
}

func (c *Client) Server(ctx context.Context) (ServerInfo, error) {
	var out ServerInfo
	err := c.call(ctx, http.MethodGet, "/v1/server", nil, &out)
	return out, err
}

// ---- file manager ----

func filesPath(site, rest string) string { return "/v1/sites/" + url.PathEscape(site) + rest }

// Files returns one page of a folder of the site's data directory.
func (c *Client) Files(ctx context.Context, site, dir string, opt datafs.ListOptions) (datafs.Listing, error) {
	q := url.Values{"dir": {dir}, "sort": {opt.Sort}, "offset": {strconv.Itoa(opt.Offset)}, "limit": {strconv.Itoa(opt.Limit)}}
	if opt.DirsOnly {
		q.Set("dirs", "1")
	}
	var l datafs.Listing
	err := c.call(ctx, http.MethodGet, filesPath(site, "/files?"+q.Encode()), nil, &l)
	return l, err
}

// FileDirs lists every folder of the site's data directory.
func (c *Client) FileDirs(ctx context.Context, site string) ([]string, error) {
	var d []string
	err := c.call(ctx, http.MethodGet, filesPath(site, "/files/dirs"), nil, &d)
	return d, err
}

// DownloadFile streams a file of the data directory (ranges pass through).
func (c *Client) DownloadFile(ctx context.Context, site, p string, a Actor, hdr http.Header) (*http.Response, error) {
	return c.download(ctx, filesPath(site, "/files/download?"+url.Values{"path": {p}}.Encode()), a, hdr)
}

// ZipFiles streams items of a folder (all of it when names is empty) as a zip.
func (c *Client) ZipFiles(ctx context.Context, site, dir string, names []string, a Actor) (*http.Response, error) {
	b, err := json.Marshal(FilesChange{Op: "zip", Dir: dir, Names: names, Actor: a})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent"+filesPath(site, "/files/zip"), bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.long().Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent'a ulaşılamadı: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, decode(resp, nil)
	}
	return resp, nil
}

// ChangeFiles makes a folder, renames, moves or deletes to the trash (the entry).
func (c *Client) ChangeFiles(ctx context.Context, site string, ch FilesChange) (*datafs.TrashEntry, error) {
	var r struct {
		Entry *datafs.TrashEntry `json:"entry"`
	}
	err := c.call(ctx, http.MethodPost, filesPath(site, "/files/change"), ch, &r)
	return r.Entry, err
}

// PlaceUpload puts a finished upload into a folder. A copy may take a while (when it
// cannot be linked), so no client timeout: the caller's context bounds it.
func (c *Client) PlaceUpload(ctx context.Context, site string, req PlaceRequest) (datafs.Placed, error) {
	var p datafs.Placed
	b, err := json.Marshal(req)
	if err != nil {
		return p, err
	}
	err = c.send(ctx, http.MethodPost, filesPath(site, "/files/place"), http.Header{"Content-Type": {"application/json"}},
		bytes.NewReader(b), int64(len(b)), &p)
	return p, err
}

// Unpack starts opening an archive of the data directory.
func (c *Client) Unpack(ctx context.Context, site string, req UnpackRequest) (string, error) {
	var r JobResponse
	err := c.call(ctx, http.MethodPost, filesPath(site, "/files/unpack"), req, &r)
	return r.JobID, err
}

// Trash lists the site's trash.
func (c *Client) Trash(ctx context.Context, site string) ([]datafs.TrashEntry, error) {
	var l []datafs.TrashEntry
	err := c.call(ctx, http.MethodGet, filesPath(site, "/trash"), nil, &l)
	return l, err
}

// RestoreTrash puts a trash entry back.
func (c *Client) RestoreTrash(ctx context.Context, site, id string, a Actor) (datafs.Restored, error) {
	var r datafs.Restored
	err := c.call(ctx, http.MethodPost, filesPath(site, "/trash/"+url.PathEscape(id)+"/restore"), RestartRequest{Actor: a}, &r)
	return r, err
}

// EmptyTrash deletes the site's trash for good; confirm is the site's name.
func (c *Client) EmptyTrash(ctx context.Context, site, confirm string, a Actor) (EmptyTrashResult, error) {
	var r EmptyTrashResult
	err := c.call(ctx, http.MethodPost, filesPath(site, "/trash/empty"), ConfirmRequest{Confirm: confirm, Actor: a}, &r)
	return r, err
}

// SetDataURL records where the site serves its data directory's files.
func (c *Client) SetDataURL(ctx context.Context, site, u string, a Actor) error {
	return c.call(ctx, http.MethodPost, filesPath(site, "/files/url"), DataURLRequest{URL: u, Actor: a}, nil)
}
