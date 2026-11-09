// Package monitor is the two small files the monitoring keeps next to each server's
// backups (panel-tasarim §27.8, §27.17): izleme/durum.json, which the server's agent
// writes every few minutes, and izleme/nobetci.json, the outside watcher's heartbeat.
// The agent writes the one and reads the other, the Mac's footops-mac reads both
// (§29.3); the watcher (worker/src/logic.js) has its own reading of the same formats.
// No secrets in either.
package monitor

import (
	"strings"
	"time"
	"unicode/utf8"
)

const (
	StatusKey = "izleme/durum.json"
	// StatusFormat is the status file's format: fields are only ever added to it, so
	// readers of format 1 keep working.
	StatusFormat = 1
	WatcherKey   = "izleme/nobetci.json"
)

// StatusFile is izleme/durum.json.
type StatusFile struct {
	Format  int          `json:"format"`
	Server  string       `json:"server"`
	FootOps string       `json:"footops"`
	Time    time.Time    `json:"time"`
	Every   int          `json:"every"` // seconds until the next one
	Sites   []StatusSite `json:"sites"`
	// SitesError: the site list could not be read; Sites is then empty and the watcher
	// keeps the domains it had (it would otherwise take every site for removed).
	SitesError string       `json:"sites_error,omitempty"`
	Backup     StatusBackup `json:"backup"`
	// Sys is the server itself as the agent last sampled it (Faz M: the Mac's menu bar
	// shows it); none before the first sample.
	Sys *StatusSys `json:"sys,omitempty"`
}

// StatusSite is a site and the state of its container.
type StatusSite struct {
	Name    string   `json:"name"`
	Domains []string `json:"domains"`
	// State: running, unhealthy, restarting, paused (frozen for a backup), stopped,
	// missing (no container) or parked (switched off on purpose, Faz P1b: the watcher
	// leaves it alone).
	State string `json:"state"`
}

// StatusBackup is how the backups stand.
type StatusBackup struct {
	Ready  bool `json:"ready"` // set up and the password confirmed
	Paused bool `json:"paused,omitempty"`
	// LastOK is the newest part saved; NightDone the last night run's date.
	LastOK    time.Time `json:"last_ok,omitzero"`
	NightDone string    `json:"night_done,omitempty"`
	// Problems are the backup alerts' titles.
	Problems []string `json:"problems,omitempty"`
}

// StatusSys is the host from the agent's latest sample (what /durum says too): nothing
// is measured for the file.
type StatusSys struct {
	Time       time.Time `json:"time"`        // when it was sampled
	CPUPercent float64   `json:"cpu_percent"` // of all cores
	Load1      float64   `json:"load1"`
	Load5      float64   `json:"load5"`
	Load15     float64   `json:"load15"`
	MemUsed    uint64    `json:"mem_used"` // bytes; used is total less available
	MemTotal   uint64    `json:"mem_total"`
	SwapUsed   uint64    `json:"swap_used"`
	SwapTotal  uint64    `json:"swap_total"`
	DiskUsed   uint64    `json:"disk_used"` // the root file system
	DiskTotal  uint64    `json:"disk_total"`
	Uptime     int64     `json:"uptime"` // seconds
}

// WatcherBeat is izleme/nobetci.json (worker/src/logic.js, beat()).
type WatcherBeat struct {
	Format int       `json:"format"`
	Time   time.Time `json:"time"`
	Every  int       `json:"every"`    // seconds until the next one
	Visits int       `json:"interval"` // seconds between visits of a working domain
	// Domains are this server's, as the watcher judges them.
	Domains []WatcherDomain `json:"domains"`
	// The watcher's own trouble in the run that wrote this: its messages refused (since
	// SendFailing), its state not saved.
	SendError   string    `json:"send_error,omitempty"`
	SendFailing time.Time `json:"send_failing,omitzero"`
	StateError  string    `json:"state_error,omitempty"`
}

// WatcherDomain is one domain seen from outside: OK when its last visit was answered;
// Down when it failed often enough for the chat to hear of it.
type WatcherDomain struct {
	Name   string    `json:"name"`
	Site   string    `json:"site"`
	OK     bool      `json:"ok"`
	Down   bool      `json:"down,omitempty"`
	Code   string    `json:"code,omitempty"`
	Reason string    `json:"reason,omitempty"`
	Since  time.Time `json:"since,omitzero"`
	Note   string    `json:"note,omitempty"` // e.g. a Cloudflare challenge answered the visit
}

// Failing lists the domains whose last visit was not answered (for the panel).
func (b *WatcherBeat) Failing() []WatcherDomain {
	var out []WatcherDomain
	for _, d := range b.Domains {
		if !d.OK {
			out = append(out, d)
		}
	}
	return out
}

// Minutes is how often a working domain is visited (for the panel); 0 if not said.
func (b *WatcherBeat) Minutes() int { return b.Visits / 60 }

// Blind lists the domains whose visits Cloudflare answered for the site (a challenge):
// up as far as anyone can tell, but not watched.
func (b *WatcherBeat) Blind() []WatcherDomain {
	var out []WatcherDomain
	for _, d := range b.Domains {
		if d.OK && d.Note != "" {
			out = append(out, d)
		}
	}
	return out
}

// Trouble is the watcher's own problem, in a few words ("" when none).
func (b *WatcherBeat) Trouble() string {
	var parts []string
	if b.SendError != "" {
		parts = append(parts, "Telegram'a mesaj gönderemiyor ("+clip(b.SendError, 200)+")")
	}
	if b.StateError != "" {
		parts = append(parts, "durumunu kaydedemiyor ("+clip(b.StateError, 200)+"), aynı haberi yeniden gönderebilir")
	}
	return strings.Join(parts, "; ")
}

// clip keeps at most n bytes of a message (whole characters).
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
