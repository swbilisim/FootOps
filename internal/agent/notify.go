package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/core"
)

// Notifications (panel-tasarim §27.8) take the server's alerts to a Telegram chat. A
// message goes out when an alert opens, gets worse (warn → bad) or clears for good;
// wording that changes while an alert stays open sends nothing. What the chat was told
// is kept on disk, so an agent restart neither repeats an open alert nor forgets to say
// that one cleared while the agent was away.
//
// The chat is linked with a one-time code: the panel shows a t.me link that sends
// "/start <code>" to the bot, and the agent looks for exactly that text among the bot's
// pending updates. Anyone can find a bot and write to it, so "the first chat that
// writes" would not do.
const (
	DefaultNotifySecret = core.NotifySecretDir + "/telegram.json"
	DefaultNotifyState  = "/var/lib/footops/notify.json"
	TelegramAPI         = "https://api.telegram.org"

	// notifyEvery is how often the active alerts are compared with what the chat knows.
	notifyEvery = 15 * time.Second
	// clearHold: an alert must stay gone this long before the chat hears it cleared.
	clearHold = 5 * time.Minute
	// startHold: nothing is reported cleared this soon after the agent starts. Some
	// alerts need a history that a restart loses (restarts within 15 minutes, the status
	// file failing for 20) and come back only once it has built up again.
	startHold = 25 * time.Minute
	// linkTTL is how long a link code is good for.
	linkTTL = 30 * time.Minute
	// sendBackoff is the first wait after a failed send; it doubles up to sendBackoffMax.
	sendBackoff    = time.Minute
	sendBackoffMax = 30 * time.Minute
	// telegramMax is a message's length in characters (Telegram allows 4096).
	telegramMax = 4000
	// detailMax is how much of an alert's detail a message carries.
	detailMax = 400
)

var (
	reBotToken = regexp.MustCompile(`^[0-9]{5,16}:[A-Za-z0-9_-]{30,80}$`)
	// errNoNotify: no chat is linked.
	errNoNotify = errors.New("Telegram bağlı değil: önce Bildirimler sayfasından bağla")
)

// telegramConfig is the bot and the chat: root-only (0600), the token runs the bot.
type telegramConfig struct {
	Token  string    `json:"token"`
	Bot    string    `json:"bot"` // the bot's user name, without @
	ChatID int64     `json:"chat_id"`
	Chat   string    `json:"chat"` // the chat's name, for the panel
	Linked time.Time `json:"linked"`
	By     string    `json:"by,omitempty"`
}

// sentAlert is an alert the chat was told about and not yet told it cleared.
type sentAlert struct {
	Level string    `json:"level"`
	Title string    `json:"title"`
	Link  string    `json:"link,omitempty"`
	Since time.Time `json:"since"`
	Sent  time.Time `json:"sent"`
}

// notifyState is what the notifier keeps between restarts (no secrets).
type notifyState struct {
	// Chat is the chat Sent belongs to: another chat starts afresh.
	Chat      int64                `json:"chat,omitempty"`
	Sent      map[string]sentAlert `json:"sent,omitempty"`
	LastSent  time.Time            `json:"last_sent,omitempty"`
	LastErr   string               `json:"last_error,omitempty"`
	LastErrAt time.Time            `json:"last_error_at,omitempty"`
	// Failing is when sends started failing (zero once one gets through).
	Failing time.Time `json:"failing,omitempty"`
}

// telegramLink is a link in progress. It lives in memory only (its token too): an agent
// restart just means starting the link again.
type telegramLink struct {
	token   string
	bot     string
	code    string
	expires time.Time
}

// Notifier owns the Telegram settings and sends the alerts.
type Notifier struct {
	SecretPath string
	StatePath  string
	// Server is the server code: messages start with "[H1]".
	Server string
	// PanelHost makes the links in messages (https://<host><alert link>); empty: none.
	PanelHost string
	// API is Telegram's Bot API address; tests stand in a fake.
	API  string
	HTTP *http.Client
	// Active returns the active alerts (the Alerter's).
	Active func() []Alert
	Log    *log.Logger

	now      func() time.Time
	initOnce sync.Once
	// work serializes what talks to Telegram or changes the files: the loop's sends,
	// linking, a test message, removing the chat.
	work sync.Mutex
	// mu guards the fields below (status reads do not wait for a send).
	mu      sync.Mutex
	link    *telegramLink
	missing map[string]time.Time // sent alerts not active since
	retryAt time.Time
	backoff time.Duration
	wake    chan struct{}
	started time.Time
	// st is the state, read from StatePath once: the file is written through but never
	// read back, so a disk that cannot take it (full, read-only) does not make the chat
	// hear the same alerts again; saveErr keeps that from filling the log.
	st       notifyState
	stLoaded bool
	saveErr  string
}

func (n *Notifier) init() {
	n.initOnce.Do(func() {
		if n.now == nil {
			n.now = time.Now
		}
		if n.API == "" {
			n.API = TelegramAPI
		}
		if n.HTTP == nil {
			n.HTTP = &http.Client{Timeout: 20 * time.Second}
		}
		n.missing = map[string]time.Time{}
		n.wake = make(chan struct{}, 1)
		n.started = n.now()
	})
}

func (n *Notifier) logf(format string, args ...any) {
	if n.Log != nil {
		n.Log.Printf(format, args...)
	}
}

// ---- files ----

func (n *Notifier) config() (telegramConfig, error) {
	var c telegramConfig
	data, err := os.ReadFile(n.SecretPath)
	if errors.Is(err, fs.ErrNotExist) {
		return c, errNoNotify
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil || !reBotToken.MatchString(c.Token) || c.ChatID == 0 {
		return telegramConfig{}, errors.New("Telegram ayarları bozuk; Bildirimler sayfasından yeniden bağla")
	}
	return c, nil
}

func (n *Notifier) saveConfig(c telegramConfig) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(n.SecretPath), 0o700); err != nil {
		return err
	}
	return writeSynced(n.SecretPath, append(data, '\n'))
}

// state returns a copy of the state (read from the file the first time).
func (n *Notifier) state() notifyState {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.stLoaded {
		if data, err := os.ReadFile(n.StatePath); err == nil {
			_ = json.Unmarshal(data, &n.st) // a broken file starts afresh: at worst an alert is told twice
		}
		n.stLoaded = true
	}
	st := n.st
	st.Sent = make(map[string]sentAlert, len(n.st.Sent))
	for k, v := range n.st.Sent {
		st.Sent[k] = v
	}
	return st
}

// saveState makes st the state and writes it through to the file; a write that fails
// is logged once until one works again.
func (n *Notifier) saveState(st notifyState) {
	n.mu.Lock()
	n.st, n.stLoaded = st, true
	n.mu.Unlock()
	data, err := json.MarshalIndent(st, "", "  ")
	if err == nil {
		err = writeSynced(n.StatePath, append(data, '\n'))
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	switch {
	case err != nil && n.saveErr != err.Error():
		n.saveErr = err.Error()
		n.logf("bildirim durumu kaydedilemedi (bellekte tutuluyor): %v", err)
	case err == nil:
		n.saveErr = ""
	}
}

// ---- the loop ----

// Run sends the alerts' changes until ctx is done.
func (n *Notifier) Run(ctx context.Context) {
	n.init()
	t := time.NewTicker(notifyEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-n.wake:
		}
		n.Tick(ctx)
	}
}

func (n *Notifier) poke() {
	n.init()
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// alertEvent is one change the chat hears about.
type alertEvent struct {
	kind  string // "open", "worse" or "clear"
	alert Alert
	took  time.Duration // how long a cleared alert lasted
}

// Tick compares the active alerts with what the chat was told and sends the difference
// in one message (or a few, if long): each one that gets through is recorded at once,
// so a failure halfway repeats nothing that went.
func (n *Notifier) Tick(ctx context.Context) {
	n.init()
	n.work.Lock()
	defer n.work.Unlock()
	cfg, err := n.config()
	if err != nil {
		return // not linked (or the file is broken: the page says so)
	}
	now := n.now()
	n.mu.Lock()
	wait := now.Before(n.retryAt)
	n.mu.Unlock()
	if wait {
		return
	}
	var active []Alert
	if n.Active != nil {
		active = n.Active()
	}
	st := n.state()
	if st.Chat != cfg.ChatID {
		st = notifyState{Chat: cfg.ChatID, Sent: map[string]sentAlert{}}
	}
	evs := n.events(st.Sent, active, now)
	if len(evs) == 0 {
		// Nothing waits to be sent, so nothing fails to be: an earlier failure is over.
		if !st.Failing.IsZero() {
			st.Failing, st.LastErr, st.LastErrAt = time.Time{}, "", time.Time{}
			n.saveState(st)
		}
		return
	}
	for _, b := range n.messages(evs) {
		if err = n.send(ctx, cfg, b.text); err != nil {
			break
		}
		for _, e := range b.evs {
			record(st.Sent, e, now)
			if e.kind == "clear" {
				n.mu.Lock()
				delete(n.missing, e.alert.Key)
				n.mu.Unlock()
			}
		}
		st.LastSent = now
	}
	var te *telegramError
	if errors.As(err, &te) && te.migrateTo != 0 {
		// A group became a supergroup under a new id: the same chat, told the same.
		cfg.ChatID, st.Chat = te.migrateTo, te.migrateTo
		if serr := n.saveConfig(cfg); serr == nil {
			n.logf("Telegram sohbeti süper gruba taşındı; yeni kimlikle devam ediliyor")
			n.saveState(st)
			n.poke()
			return
		}
	}
	n.mu.Lock()
	if err != nil {
		if n.backoff == 0 {
			n.backoff = sendBackoff
		} else if n.backoff *= 2; n.backoff > sendBackoffMax {
			n.backoff = sendBackoffMax
		}
		if te != nil && te.retryAfter > 0 {
			n.backoff = max(n.backoff, time.Duration(te.retryAfter)*time.Second)
		}
		n.retryAt = now.Add(n.backoff)
	} else {
		n.backoff, n.retryAt = 0, time.Time{}
	}
	n.mu.Unlock()
	if err != nil {
		if st.LastErr != err.Error() {
			n.logf("bildirim gönderilemedi: %v", err)
		}
		if st.Failing.IsZero() {
			st.Failing = now
		}
		st.LastErr, st.LastErrAt = err.Error(), now
	} else {
		st.Failing, st.LastErr, st.LastErrAt = time.Time{}, "", time.Time{}
	}
	n.saveState(st)
}

// record writes into sent what the chat now knows after hearing e.
func record(sent map[string]sentAlert, e alertEvent, now time.Time) {
	a := e.alert
	switch e.kind {
	case "open":
		sent[a.Key] = sentAlert{Level: a.Level, Title: a.Title, Link: a.Link, Since: a.Since, Sent: now}
	case "worse":
		since := sent[a.Key].Since
		sent[a.Key] = sentAlert{Level: a.Level, Title: a.Title, Link: a.Link, Since: since, Sent: now}
	case "clear":
		delete(sent, a.Key)
	}
}

// events works out what changed since the chat was last told. Alerts that the chat does
// not hear about (Notifiable) are left out from the start; one it heard about stays its
// business until it clears.
func (n *Notifier) events(sent map[string]sentAlert, active []Alert, now time.Time) []alertEvent {
	var evs []alertEvent
	seen := map[string]bool{}
	sortAlerts(active)
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, a := range active {
		seen[a.Key] = true
		delete(n.missing, a.Key)
		prev, told := sent[a.Key]
		switch {
		case told && prev.Level != "bad" && a.Level == "bad":
			evs = append(evs, alertEvent{kind: "worse", alert: a})
		case told:
			// Same or milder, or only worded differently: the chat keeps what it was told,
			// and hears it cleared in the same words.
		case Notifiable(a):
			evs = append(evs, alertEvent{kind: "open", alert: a})
		}
	}
	keys := make([]string, 0, len(sent))
	for k := range sent {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if seen[k] {
			continue
		}
		prev := sent[k]
		gone, ok := n.missing[k]
		if !ok || gone.After(now) {
			n.missing[k] = now
			continue
		}
		if now.Sub(gone) < clearHold || now.Sub(n.started) < startHold {
			continue
		}
		took := gone.Sub(prev.Since)
		if prev.Since.IsZero() || took < 0 {
			took = 0
		}
		evs = append(evs, alertEvent{kind: "clear", alert: Alert{Key: k, Level: prev.Level, Title: prev.Title, Link: prev.Link}, took: took})
	}
	for k := range n.missing {
		if _, ok := sent[k]; !ok {
			delete(n.missing, k)
		}
	}
	return evs
}

// Notifiable tells which alerts go to the chat. Left out: what flaps or only matters on
// the dashboard (CPU, a container close to its memory limit, RAM or swap that is high
// but not critical), the notifier's own trouble (it could not send it anyway), and
// sites that are down — the outside watcher (Y4b) reports those as visitors see them,
// so the chat does not hear it twice. The server's own containers (MongoDB, Caddy, the
// tunnel; site names cannot start with "footops") do go: nothing else would tell.
func Notifiable(a Alert) bool {
	kind, name, _ := strings.Cut(a.Key, ":")
	switch kind {
	case "cpu", "limit", "notify":
		return false
	case "memory", "swap":
		return a.Level == "bad"
	case "down", "health":
		return strings.HasPrefix(name, "footops")
	}
	return true
}

// message is one Telegram message and the events it tells.
type message struct {
	text string
	evs  []alertEvent
}

// messages renders events as Telegram messages (plain text: Telegram makes the links),
// each within telegramMax.
func (n *Notifier) messages(evs []alertEvent) []message {
	var out []message
	var cur message
	var b strings.Builder
	for _, e := range evs {
		block := clipMsg(n.render(e), telegramMax)
		if b.Len() > 0 && b.Len()+2+len(block) > telegramMax {
			cur.text = b.String()
			out = append(out, cur)
			cur, b = message{}, strings.Builder{}
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(block)
		cur.evs = append(cur.evs, e)
	}
	if b.Len() > 0 {
		cur.text = b.String()
		out = append(out, cur)
	}
	return out
}

func (n *Notifier) render(e alertEvent) string {
	a := e.alert
	// Alert texts carry other programs' words (restic, mongodump): Telegram refuses a
	// message that is not valid UTF-8, and would refuse it every time.
	a.Title, a.Detail = strings.ToValidUTF8(a.Title, "?"), strings.ToValidUTF8(a.Detail, "?")
	mark := "🟡"
	if a.Level == "bad" {
		mark = "🔴"
	}
	var b strings.Builder
	switch e.kind {
	case "clear":
		fmt.Fprintf(&b, "✅ %s Düzeldi: %s", n.prefix(), a.Title)
		if e.took >= time.Minute {
			fmt.Fprintf(&b, " (%s sürdü)", spanTR(e.took))
		}
		return b.String()
	case "worse":
		fmt.Fprintf(&b, "%s %s Kötüleşti: %s", mark, n.prefix(), a.Title)
	default:
		fmt.Fprintf(&b, "%s %s %s", mark, n.prefix(), a.Title)
	}
	if d := strings.TrimSpace(a.Detail); d != "" {
		b.WriteString("\n" + clipMsg(d, detailMax))
	}
	if u := n.panelURL(a.Link); u != "" {
		b.WriteString("\n" + u)
	}
	return b.String()
}

func (n *Notifier) prefix() string {
	if n.Server == "" {
		return "[FootOps]"
	}
	return "[" + strings.ToUpper(n.Server) + "]"
}

// panelURL is an alert's panel page as a full address, when the panel has a host name.
func (n *Notifier) panelURL(link string) string {
	if n.PanelHost == "" || !strings.HasPrefix(link, "/") || strings.HasPrefix(link, "//") {
		return ""
	}
	return "https://" + n.PanelHost + link
}

// spanTR is a duration in Turkish: "3 dk", "2 sa 5 dk", "3 gün 4 sa".
func spanTR(d time.Duration) string {
	m := int(d / time.Minute)
	switch {
	case m < 60:
		return fmt.Sprintf("%d dk", max(m, 1))
	case m < 24*60:
		if m%60 == 0 {
			return fmt.Sprintf("%d sa", m/60)
		}
		return fmt.Sprintf("%d sa %d dk", m/60, m%60)
	}
	h := m / 60
	if h%24 == 0 {
		return fmt.Sprintf("%d gün", h/24)
	}
	return fmt.Sprintf("%d gün %d sa", h/24, h%24)
}

// ---- Telegram's Bot API ----

// telegramError is the API's own refusal.
type telegramError struct {
	code       int
	desc       string
	retryAfter int
	migrateTo  int64 // a group's new id after it became a supergroup
}

func (e *telegramError) Error() string {
	low := strings.ToLower(e.desc)
	switch {
	case e.code == 401 || e.code == 404:
		return "Telegram token'ı geçersiz (BotFather → /mybots → bot → API Token'ı yeniden kopyala)"
	case e.code == 429:
		return fmt.Sprintf("Telegram çok sık mesaj gönderildiğini söyledi; %d sn sonra yeniden denenecek", e.retryAfter)
	case e.code == 403 && strings.Contains(low, "blocked"):
		return "bot bu sohbette engellenmiş: Telegram'da botu aç, engeli kaldır (ya da yeniden bağla)"
	case e.code == 403:
		return "bot bu sohbete yazamıyor (sohbetten çıkarılmış olabilir): yeniden bağla"
	case e.code == 400 && strings.Contains(low, "chat not found"):
		return "sohbet bulunamadı: yeniden bağla"
	case e.code == 409:
		return "botun bir webhook'u var, mesajları okunamıyor: başka bir uygulama bu botu kullanıyor olabilir (ayrı bir bot aç)"
	}
	return fmt.Sprintf("Telegram hatası %d: %s", e.code, clipMsg(e.desc, 200))
}

// call runs one Bot API method. The token sits in the address, so no error that comes
// back carries the address: Go's client errors would show it.
func (n *Notifier) call(ctx context.Context, token, method string, params url.Values, out any) error {
	n.init()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.API+"/bot"+token+"/"+method, strings.NewReader(params.Encode()))
	if err != nil {
		return errors.New("Telegram isteği hazırlanamadı")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := n.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("Telegram'a ulaşılamadı: %s", scrubToken(err.Error(), token))
	}
	defer drain(resp.Body)
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
		Parameters  struct {
			RetryAfter int   `json:"retry_after"`
			MigrateTo  int64 `json:"migrate_to_chat_id"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return fmt.Errorf("Telegram'ın cevabı okunamadı (HTTP %d)", resp.StatusCode)
	}
	if !r.OK {
		code := r.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return &telegramError{code: code, desc: scrubToken(r.Description, token), retryAfter: r.Parameters.RetryAfter,
			migrateTo: r.Parameters.MigrateTo}
	}
	if out != nil {
		if err := json.Unmarshal(r.Result, out); err != nil {
			return errors.New("Telegram'ın cevabı beklenen biçimde değil")
		}
	}
	return nil
}

// scrubToken keeps a bot token out of a message (and its secret half, should only that
// show).
func scrubToken(s, token string) string {
	if token == "" {
		return s
	}
	s = strings.ReplaceAll(s, token, "<token>")
	if _, secret, ok := strings.Cut(token, ":"); ok && len(secret) >= 8 {
		s = strings.ReplaceAll(s, secret, "<token>")
	}
	return s
}

func (n *Notifier) send(ctx context.Context, cfg telegramConfig, text string) error {
	return n.call(ctx, cfg.Token, "sendMessage", url.Values{
		"chat_id":              {strconv.FormatInt(cfg.ChatID, 10)},
		"text":                 {text},
		"link_preview_options": {`{"is_disabled":true}`},
	}, nil)
}

// ---- linking a chat (the panel's Bildirimler page) ----

// NotifyLink is a link in progress: the panel shows it until the code arrives.
type NotifyLink struct {
	Bot     string    `json:"bot"`
	Code    string    `json:"code"`
	URL     string    `json:"url"` // t.me link that sends the code
	Expires time.Time `json:"expires"`
}

// NotifyStatus is the Bildirimler page (no token).
type NotifyStatus struct {
	Linked bool        `json:"linked"`
	Bot    string      `json:"bot,omitempty"`
	Chat   string      `json:"chat,omitempty"`
	Since  time.Time   `json:"since,omitempty"`
	By     string      `json:"by,omitempty"`
	Link   *NotifyLink `json:"link,omitempty"`
	// Told is how many open alerts the chat knows about.
	Told      int       `json:"told"`
	LastSent  time.Time `json:"last_sent,omitempty"`
	LastErr   string    `json:"last_error,omitempty"`
	LastErrAt time.Time `json:"last_error_at,omitempty"`
	Failing   time.Time `json:"failing,omitempty"`
	RetryAt   time.Time `json:"retry_at,omitempty"`
	Error     string    `json:"error,omitempty"` // the settings could not be read
	// StatusFile is how writing the outside watcher's status file goes (nil: off).
	StatusFile *StatusFileState `json:"status_file,omitempty"`
}

// Status reports the chat and how sending goes.
func (n *Notifier) Status() NotifyStatus {
	n.init()
	var out NotifyStatus
	cfg, err := n.config()
	switch {
	case err == nil:
		st := n.state()
		out = NotifyStatus{Linked: true, Bot: cfg.Bot, Chat: cfg.Chat, Since: cfg.Linked, By: cfg.By}
		if st.Chat == cfg.ChatID {
			out.Told, out.LastSent, out.LastErr, out.LastErrAt, out.Failing = len(st.Sent), st.LastSent, st.LastErr, st.LastErrAt, st.Failing
		}
	case !errors.Is(err, errNoNotify):
		out.Error = err.Error()
	}
	n.mu.Lock()
	if l := n.link; l != nil && n.now().Before(l.expires) {
		out.Link = l.view()
	}
	if out.Linked && n.now().Before(n.retryAt) {
		out.RetryAt = n.retryAt
	}
	n.mu.Unlock()
	return out
}

func (l *telegramLink) view() *NotifyLink {
	return &NotifyLink{Bot: l.bot, Code: l.code, URL: "https://t.me/" + l.bot + "?start=" + l.code, Expires: l.expires}
}

// linkAlphabet leaves out look-alikes (0/O, 1/I/L), for typing the code by hand.
const linkAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func newLinkCode() (string, error) {
	b := make([]byte, 10)
	r := make([]byte, 64)
	if _, err := randRead(r); err != nil {
		return "", err
	}
	j := 0
	for i := range b {
		for {
			if j >= len(r) {
				if _, err := randRead(r); err != nil {
					return "", err
				}
				j = 0
			}
			c := r[j]
			j++
			if int(c) < 256-256%len(linkAlphabet) {
				b[i] = linkAlphabet[int(c)%len(linkAlphabet)]
				break
			}
		}
	}
	return string(b), nil
}

// StartLink checks a bot token with Telegram (getMe) and starts a link: the panel
// shows the code until it arrives. keep uses the token already saved (another chat).
func (n *Notifier) StartLink(ctx context.Context, token string, keep bool) (NotifyLink, error) {
	n.init()
	n.work.Lock()
	defer n.work.Unlock()
	token = strings.TrimSpace(token)
	if keep {
		cfg, err := n.config()
		if err != nil {
			return NotifyLink{}, err
		}
		token = cfg.Token
	} else if !reBotToken.MatchString(token) {
		return NotifyLink{}, errors.New("bot token'ı 123456789:ABC… biçiminde olmalı (BotFather'ın verdiği API Token)")
	}
	// The chat is greeted before it is saved: find out now that it could be saved.
	if err := writableDir(filepath.Dir(n.SecretPath)); err != nil {
		return NotifyLink{}, fmt.Errorf("Telegram ayarı bu sunucuda kaydedilemiyor (%v): kurulum yarım kalmış olabilir, sudo footops install ile tamamla", err)
	}
	var me struct {
		IsBot    bool   `json:"is_bot"`
		Username string `json:"username"`
	}
	if err := n.call(ctx, token, "getMe", url.Values{}, &me); err != nil {
		return NotifyLink{}, err
	}
	if !me.IsBot || !reBotName.MatchString(me.Username) {
		return NotifyLink{}, errors.New("bu token bir bota ait değil")
	}
	code, err := newLinkCode()
	if err != nil {
		return NotifyLink{}, err
	}
	l := &telegramLink{token: token, bot: me.Username, code: code, expires: n.now().Add(linkTTL)}
	n.mu.Lock()
	n.link = l
	n.mu.Unlock()
	return *l.view(), nil
}

var reBotName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{3,31}$`)

// writableDir reports whether a file can be created in dir (the agent's unit makes /etc
// read-only but for the directories it names).
func writableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".yazma-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// tgMessage is the part of a Telegram message the link looks at.
type tgMessage struct {
	Text string `json:"text"`
	Chat struct {
		ID        int64  `json:"id"`
		Type      string `json:"type"`
		Title     string `json:"title"`
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	} `json:"chat"`
}

func (m tgMessage) chatName() string {
	c := m.Chat
	name := strings.TrimSpace(c.Title)
	if name == "" {
		name = strings.TrimSpace(c.FirstName + " " + c.LastName)
	}
	if c.Username != "" {
		if name == "" {
			return "@" + c.Username
		}
		name += " (@" + c.Username + ")"
	}
	if name == "" {
		name = strconv.FormatInt(c.ID, 10)
	}
	return clipMsg(name, 80)
}

// errCodeNotSeen: the code has not reached the bot yet.
var errCodeNotSeen = errors.New("kod henüz bota ulaşmadı: Telegram'da bağlantıyı açıp Başlat'a bas (ya da kodu bota yaz), sonra tekrar dene")

// FinishLink looks for the code among the bot's latest updates and, once found, greets
// the chat and saves it. A new chat then hears every open alert afresh; the same chat
// linked again keeps what it was told. offset=-100 asks for the newest hundred (and
// drops older ones nobody reads: the bot is only read while linking; another server
// linking at the same time finds its own code among the newest).
func (n *Notifier) FinishLink(ctx context.Context, by string) (NotifyStatus, error) {
	n.init()
	n.work.Lock()
	n.mu.Lock()
	l := n.link
	n.mu.Unlock()
	if l == nil || !n.now().Before(l.expires) {
		n.work.Unlock()
		return NotifyStatus{}, errors.New("bağlantı kodunun süresi doldu ya da bağlantı başlatılmadı: yeniden başla")
	}
	var updates []struct {
		UpdateID    int64      `json:"update_id"`
		Message     *tgMessage `json:"message"`
		ChannelPost *tgMessage `json:"channel_post"`
	}
	if err := n.call(ctx, l.token, "getUpdates", url.Values{"offset": {"-100"}, "timeout": {"0"}, "limit": {"100"}}, &updates); err != nil {
		n.work.Unlock()
		return NotifyStatus{}, err
	}
	var found *tgMessage
	for i := len(updates) - 1; i >= 0 && found == nil; i-- {
		for _, m := range []*tgMessage{updates[i].Message, updates[i].ChannelPost} {
			if m != nil && m.Chat.ID != 0 && strings.Contains(strings.ToUpper(m.Text), l.code) {
				found = m
				break
			}
		}
	}
	if found == nil {
		n.work.Unlock()
		return NotifyStatus{}, errCodeNotSeen
	}
	cfg := telegramConfig{Token: l.token, Bot: l.bot, ChatID: found.Chat.ID, Chat: found.chatName(), Linked: n.now().UTC(), By: by}
	welcome := fmt.Sprintf("✅ %s FootOps bu sohbete bağlandı. Sunucunun uyarıları (yedek, disk, sertifika, MongoDB…) buraya gelecek: açılınca, kötüleşince ve düzelince.", n.prefix())
	if err := n.send(ctx, cfg, welcome); err != nil {
		n.work.Unlock()
		return NotifyStatus{}, fmt.Errorf("sohbet bulundu ama mesaj gönderilemedi: %w", err)
	}
	old, _ := n.config()
	if err := n.saveConfig(cfg); err != nil {
		n.work.Unlock()
		return NotifyStatus{}, err
	}
	st := n.state()
	if old.ChatID != cfg.ChatID || st.Chat != cfg.ChatID {
		st = notifyState{Chat: cfg.ChatID, Sent: map[string]sentAlert{}}
	}
	st.LastSent, st.Failing, st.LastErr, st.LastErrAt = n.now(), time.Time{}, "", time.Time{}
	n.saveState(st)
	n.mu.Lock()
	n.link, n.retryAt, n.backoff = nil, time.Time{}, 0
	n.missing = map[string]time.Time{}
	n.mu.Unlock()
	n.work.Unlock()
	n.poke() // the open alerts go out right away
	return n.Status(), nil
}

// CancelLink forgets a link in progress.
func (n *Notifier) CancelLink() {
	n.init()
	n.mu.Lock()
	n.link = nil
	n.mu.Unlock()
}

// Test sends a test message to the linked chat.
func (n *Notifier) Test(ctx context.Context) error {
	n.init()
	n.work.Lock()
	defer n.work.Unlock()
	cfg, err := n.config()
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("🔔 %s Deneme mesajı: FootOps bildirimleri bu sohbete ulaşıyor (%s).", n.prefix(), n.now().Local().Format("02.01.2006 15:04"))
	if err := n.send(ctx, cfg, msg); err != nil {
		return err
	}
	// A message got through: whatever waits for a retry goes now (and if nothing waits,
	// nothing is failing).
	if st := n.state(); st.Chat == cfg.ChatID && !st.Failing.IsZero() {
		st.Failing, st.LastErr, st.LastErrAt = time.Time{}, "", time.Time{}
		n.saveState(st)
	}
	n.mu.Lock()
	n.retryAt, n.backoff = time.Time{}, 0
	n.mu.Unlock()
	n.poke()
	return nil
}

// Remove forgets the chat (and the token): nothing is sent any more.
func (n *Notifier) Remove() error {
	n.init()
	n.work.Lock()
	defer n.work.Unlock()
	if err := os.Remove(n.SecretPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Remove(n.StatePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		n.logf("bildirim durumu silinemedi: %v", err)
	}
	n.mu.Lock()
	n.link, n.retryAt, n.backoff = nil, time.Time{}, 0
	n.missing = map[string]time.Time{}
	n.st, n.stLoaded = notifyState{}, true
	n.mu.Unlock()
	return nil
}

// Alerts lists the notifier's own trouble for the panel (never sent: see Notifiable):
// messages failing for a while.
func (n *Notifier) Alerts(now time.Time) []Alert {
	n.init()
	cfg, err := n.config()
	if errors.Is(err, errNoNotify) {
		return nil
	}
	if err != nil {
		return []Alert{{Key: "notify:telegram", Level: "warn", Title: "Telegram ayarları okunamadı", Detail: err.Error(), Link: "/notify", Since: now}}
	}
	st := n.state()
	if st.Chat != cfg.ChatID || st.Failing.IsZero() || now.Sub(st.Failing) < 10*time.Minute {
		return nil
	}
	return []Alert{{Key: "notify:telegram", Level: "warn", Title: "Bildirimler Telegram'a gönderilemiyor",
		Detail: st.LastErr, Link: "/notify", Since: st.Failing}}
}
