package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// Client is used by the web process to talk to the agent.
type Client struct {
	Doer interface {
		Do(*http.Request) (*http.Response, error)
	}
}

func NewClient(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{Doer: &http.Client{Transport: tr, Timeout: 60 * time.Second}}
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

func (c *Client) Overview(ctx context.Context) (Overview, error) {
	var o Overview
	err := c.call(ctx, http.MethodGet, "/v1/overview", nil, &o)
	return o, err
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

func (c *Client) Job(ctx context.Context, id string) (Job, error) {
	var j Job
	err := c.call(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(id), nil, &j)
	return j, err
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
