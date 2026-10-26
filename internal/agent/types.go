// Package agent is the privileged half of the panel. It runs as root, listens on a unix
// socket that only the panel's web process may use, and performs a fixed set of
// validated actions. The web process never touches Docker or the filesystem directly,
// so a bug in the web UI cannot be turned into arbitrary commands.
package agent

import (
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// Overview is the dashboard snapshot.
type Overview struct {
	SampledAt  time.Time       `json:"sampled_at"`
	System     SystemMetrics   `json:"system"`
	Containers []ContainerInfo `json:"containers"`
	Errors     []string        `json:"errors,omitempty"`
}

type SystemMetrics struct {
	Hostname   string        `json:"hostname"`
	CPUPercent float64       `json:"cpu_percent"`
	Load1      float64       `json:"load1"`
	Load5      float64       `json:"load5"`
	Load15     float64       `json:"load15"`
	MemTotal   uint64        `json:"mem_total"`
	MemUsed    uint64        `json:"mem_used"`
	SwapTotal  uint64        `json:"swap_total"`
	SwapUsed   uint64        `json:"swap_used"`
	DiskTotal  uint64        `json:"disk_total"`
	DiskUsed   uint64        `json:"disk_used"`
	Uptime     time.Duration `json:"uptime"`
}

type ContainerInfo struct {
	Name         string    `json:"name"`
	Image        string    `json:"image"`
	State        string    `json:"state"`  // running, exited, restarting, ...
	Health       string    `json:"health"` // healthy, unhealthy, starting, "" (no healthcheck)
	Project      string    `json:"project"`
	RestartCount int       `json:"restart_count"`
	OOMKilled    bool      `json:"oom_killed"`
	StartedAt    time.Time `json:"started_at"`
	CPUPercent   float64   `json:"cpu_percent"`
	MemUsage     uint64    `json:"mem_usage"`
	MemLimit     uint64    `json:"mem_limit"`
}

// Actor identifies who asked for an action; the web process fills it in.
type Actor struct {
	User string `json:"user"`
	IP   string `json:"ip"`
	Via  string `json:"via"` // "tunnel" or "ssh"
}

type RestartRequest struct {
	Actor Actor `json:"actor"`
}

type LogsResponse struct {
	Lines []string `json:"lines"`
}

// AuditEntry is one line of the audit log.
type AuditEntry struct {
	Time   time.Time `json:"time"`
	Actor  Actor     `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	OK     bool      `json:"ok"`
	Detail string    `json:"detail,omitempty"`
}

type errorResponse struct {
	Error    string   `json:"error"`
	Problems []string `json:"problems,omitempty"`
}

// CreateSiteRequest carries the site definition and its environment. Env values may be
// secrets: they are written to the site's compose file (0600) and never logged.
type CreateSiteRequest struct {
	Spec  sites.Spec        `json:"spec"`
	Env   map[string]string `json:"env"`
	Actor Actor             `json:"actor"`
}

type JobResponse struct {
	JobID string `json:"job_id"`
}

// SiteStatus is a site with its container's state.
type SiteStatus struct {
	Spec      sites.Spec     `json:"spec"`
	Container *ContainerInfo `json:"container,omitempty"`
}

type InstallCertRequest struct {
	Cert  string `json:"cert"`
	Key   string `json:"key"`
	Actor Actor  `json:"actor"`
}

// ServerInfo is what the panel needs for DNS instructions.
type ServerInfo struct {
	IPv4 []string `json:"ipv4"`
	IPv6 []string `json:"ipv6"`
}
