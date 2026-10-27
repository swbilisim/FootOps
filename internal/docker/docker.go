// Package docker is a minimal Docker Engine API client over the local unix socket.
// It covers only what the agent needs, which keeps the dependency surface small.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const DefaultSocket = "/var/run/docker.sock"

// Client talks to dockerd. Doer is replaceable in tests.
type Client struct {
	Doer interface {
		Do(*http.Request) (*http.Response, error)
	}
}

// New returns a client for the unix socket at path.
func New(path string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Client{Doer: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
}

// APIError is a non-2xx response from dockerd.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("docker: %d %s", e.Status, e.Message) }

// IsNotFound reports whether err is a 404 from dockerd.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values) (*http.Response, error) {
	return c.doBody(ctx, method, path, q, nil)
}

func (c *Client) doBody(ctx context.Context, method, path string, q url.Values, body any) (*http.Response, error) {
	u := "http://docker" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Doer.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		var m struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&m)
		return nil, &APIError{Status: resp.StatusCode, Message: m.Message}
	}
	return resp, nil
}

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, v any) error {
	resp, err := c.do(ctx, http.MethodGet, path, q)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(v)
}

// Container is an entry of GET /containers/json.
type Container struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`  // running, exited, restarting, ...
	Status  string            `json:"Status"` // "Up 3 hours (healthy)"
	Created int64             `json:"Created"`
	Labels  map[string]string `json:"Labels"`
}

// Name returns the container name without the leading slash.
func (c Container) Name() string {
	if len(c.Names) == 0 {
		return c.ID[:min(12, len(c.ID))]
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// Containers lists all containers, including stopped ones.
func (c *Client) Containers(ctx context.Context) ([]Container, error) {
	var out []Container
	err := c.getJSON(ctx, "/containers/json", url.Values{"all": {"1"}}, &out)
	return out, err
}

// Inspect is the subset of GET /containers/{id}/json the agent uses.
type Inspect struct {
	ID           string `json:"Id"`
	Name         string `json:"Name"`
	RestartCount int    `json:"RestartCount"`
	State        struct {
		Status    string `json:"Status"`
		Running   bool   `json:"Running"`
		Pid       int    `json:"Pid"`
		StartedAt string `json:"StartedAt"`
		OOMKilled bool   `json:"OOMKilled"`
		ExitCode  int    `json:"ExitCode"`
		Health    *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image string `json:"Image"`
		Tty   bool   `json:"Tty"`
	} `json:"Config"`
	HostConfig struct {
		Memory int64 `json:"Memory"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

var reName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// ValidName reports whether s is a valid container name or ID. Names are always
// validated before they are put into a URL path.
func ValidName(s string) bool { return reName.MatchString(s) }

func (c *Client) Inspect(ctx context.Context, name string) (Inspect, error) {
	var out Inspect
	if !ValidName(name) {
		return out, fmt.Errorf("invalid container name %q", name)
	}
	err := c.getJSON(ctx, "/containers/"+name+"/json", nil, &out)
	return out, err
}

// Stats is a one-shot resource sample.
type Stats struct {
	CPUPercent float64
	MemUsage   uint64 // bytes, excluding page cache
	MemLimit   uint64
}

type rawStats struct {
	CPUStats    cpuStats `json:"cpu_stats"`
	PreCPUStats cpuStats `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
}

type cpuStats struct {
	CPUUsage struct {
		TotalUsage uint64 `json:"total_usage"`
	} `json:"cpu_usage"`
	SystemUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs  uint32 `json:"online_cpus"`
}

// Stats samples CPU and memory once (dockerd takes about a second to compute CPU usage).
func (c *Client) Stats(ctx context.Context, name string) (Stats, error) {
	if !ValidName(name) {
		return Stats{}, fmt.Errorf("invalid container name %q", name)
	}
	var r rawStats
	if err := c.getJSON(ctx, "/containers/"+name+"/stats", url.Values{"stream": {"false"}}, &r); err != nil {
		return Stats{}, err
	}
	return r.toStats(), nil
}

// toStats applies the same formulas as `docker stats`.
func (r rawStats) toStats() Stats {
	var s Stats
	cpuDelta := float64(r.CPUStats.CPUUsage.TotalUsage) - float64(r.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(r.CPUStats.SystemUsage) - float64(r.PreCPUStats.SystemUsage)
	if cpuDelta > 0 && sysDelta > 0 {
		s.CPUPercent = cpuDelta / sysDelta * float64(max(r.CPUStats.OnlineCPUs, 1)) * 100
	}
	used := r.MemoryStats.Usage
	// cgroup v2 reports page cache as inactive_file, v1 as total_inactive_file.
	cache := r.MemoryStats.Stats["inactive_file"]
	if cache == 0 {
		cache = r.MemoryStats.Stats["total_inactive_file"]
	}
	if cache < used {
		used -= cache
	}
	s.MemUsage, s.MemLimit = used, r.MemoryStats.Limit
	return s
}

// Restart restarts a container, giving it timeout to stop gracefully.
func (c *Client) Restart(ctx context.Context, name string, timeout time.Duration) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid container name %q", name)
	}
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+name+"/restart",
		url.Values{"t": {fmt.Sprint(int(timeout.Seconds()))}})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Logs returns the last tail lines of stdout and stderr, with timestamps.
func (c *Client) Logs(ctx context.Context, name string, tail int) ([]string, error) {
	in, err := c.Inspect(ctx, name)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+name+"/logs", url.Values{
		"stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}, "tail": {fmt.Sprint(tail)},
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, 8<<20)
	if in.Config.Tty {
		return splitLines(body)
	}
	return demux(body)
}

func splitLines(r io.Reader) ([]string, error) {
	var lines []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

// demux decodes Docker's multiplexed log stream: 8-byte headers (stream type, 3 zero
// bytes, big-endian payload length) followed by the payload.
func demux(r io.Reader) ([]string, error) {
	var buf strings.Builder
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		n := binary.BigEndian.Uint32(hdr[4:])
		if _, err := io.CopyN(&buf, r, int64(n)); err != nil {
			return nil, err
		}
	}
	return splitLines(strings.NewReader(buf.String()))
}

// ---- networks ----

func (c *Client) send(ctx context.Context, method, path string, body any) error {
	resp, err := c.doBody(ctx, method, path, nil, body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func checkNames(names ...string) error {
	for _, n := range names {
		if !ValidName(n) {
			return fmt.Errorf("invalid name %q", n)
		}
	}
	return nil
}

// CreateNetwork creates a bridge network. It is not an error if it already exists.
func (c *Client) CreateNetwork(ctx context.Context, name string, labels map[string]string) error {
	if err := checkNames(name); err != nil {
		return err
	}
	if _, ok, err := c.NetworkMembers(ctx, name); err != nil || ok {
		return err
	}
	return c.send(ctx, http.MethodPost, "/networks/create", map[string]any{
		"Name": name, "Driver": "bridge", "Labels": labels,
	})
}

// NetworkMembers returns the names of containers attached to a network and whether the
// network exists.
func (c *Client) NetworkMembers(ctx context.Context, name string) (map[string]bool, bool, error) {
	if err := checkNames(name); err != nil {
		return nil, false, err
	}
	var n struct {
		Containers map[string]struct {
			Name string `json:"Name"`
		} `json:"Containers"`
	}
	err := c.getJSON(ctx, "/networks/"+name, nil, &n)
	if IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	m := map[string]bool{}
	for _, ct := range n.Containers {
		m[ct.Name] = true
	}
	return m, true, nil
}

// Connect attaches a container to a network, with optional DNS aliases on it.
func (c *Client) Connect(ctx context.Context, network, container string, aliases ...string) error {
	if err := checkNames(append([]string{network, container}, aliases...)...); err != nil {
		return err
	}
	ep := map[string]any{}
	if len(aliases) > 0 {
		ep["Aliases"] = aliases
	}
	return c.send(ctx, http.MethodPost, "/networks/"+network+"/connect", map[string]any{
		"Container": container, "EndpointConfig": ep,
	})
}

// Disconnect detaches a container from a network. Missing networks or containers are fine.
func (c *Client) Disconnect(ctx context.Context, network, container string) error {
	if err := checkNames(network, container); err != nil {
		return err
	}
	err := c.send(ctx, http.MethodPost, "/networks/"+network+"/disconnect", map[string]any{
		"Container": container, "Force": true,
	})
	if IsNotFound(err) {
		return nil
	}
	return err
}

// RemoveNetwork deletes a network. A missing network is fine.
func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	if err := checkNames(name); err != nil {
		return err
	}
	err := c.send(ctx, http.MethodDelete, "/networks/"+name, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ContainerIP returns a container's address on a network.
func (c *Client) ContainerIP(ctx context.Context, container, network string) (string, error) {
	in, err := c.Inspect(ctx, container)
	if err != nil {
		return "", err
	}
	n, ok := in.NetworkSettings.Networks[network]
	if !ok || n.IPAddress == "" {
		return "", fmt.Errorf("%s, %s ağında değil", container, network)
	}
	return n.IPAddress, nil
}
