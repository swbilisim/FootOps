package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Reading the chat (Faz P1a, panel-tasarim §28): the agent long-polls its bot for what
// the linked chat writes and answers commands (/durum, /yardim) and the buttons under
// its messages. Only the person who linked the chat counts; anyone else gets no answer
// at all. Telegram hands a bot's updates to one reader at a time — a second reader cuts
// the first off (409) —, so every server has a bot of its own; the outside watcher's
// bot only writes. Nothing done from Telegram deletes anything.
const (
	// pollWait is how long (seconds) Telegram holds a getUpdates open when nothing comes:
	// an idle reader makes about one request a minute. pollBatch bounds one answer (a
	// message can be tens of KB: a hundred, Telegram's default, could pass the 4 MB the
	// answer is read up to, and nothing would be read again).
	pollWait  = 50
	pollLimit = pollWait*time.Second + 20*time.Second
	pollBatch = 10
	// allowedUpdates is what the bot is asked for — the link's read too: Telegram keeps
	// the last list asked for, and a channel is linked by a channel post.
	allowedUpdates = `["message","callback_query","channel_post"]`
	// readBackoff is the first wait after a failed read; it doubles up to readBackoffMax.
	readBackoff    = 2 * time.Second
	readBackoffMax = 5 * time.Minute
	// readQuick: an empty answer sooner than this waits out the rest (Telegram holds an
	// empty read; anything that does not must not make a busy loop of it).
	readQuick = time.Second
	// readIdle is how long the reader sleeps when it has nothing to read (no chat, a
	// link being made, a rehearsal), unless woken.
	readIdle = time.Minute
	// readAlertAfter: reading failing this long is an alert.
	readAlertAfter = 10 * time.Minute
	// staleCommand: a message found waiting (the reader was away: restarted, cut off)
	// older than this is left alone — it was meant for then.
	staleCommand = 10 * time.Minute
	// commandsPerMinute bounds what the chat can make the agent do.
	commandsPerMinute = 20
	// strangerLogEvery: messages from others are logged at most this often.
	strangerLogEvery = 10 * time.Minute
	// buttonTTL is how long a button under a message works; buttonsMax how many are kept
	// (a /siteler can make a few dozen).
	buttonTTL     = 24 * time.Hour
	buttonsMax    = 100
	buttonCodeLen = 12
)

// readState is how reading goes. It lives in memory: Telegram itself keeps what was
// read — an update counts as read once a later getUpdates asks past it (offset); a
// read that comes back empty has confirmed everything, and the next ones ask from the
// oldest unread (offset 0): after a quiet week Telegram numbers updates anew, and an
// offset kept from before could pass over them.
type readState struct {
	token  string // the bot offset belongs to
	offset int64
	cancel context.CancelFunc // the read under way
	// live: a read worked since the reader started on this bot or last failed — what
	// comes now comes as it happens, not from a backlog.
	live    bool
	lastOK  time.Time
	lastErr string
	errAt   time.Time
	failing time.Time
	retryAt time.Time
	backoff time.Duration
	// menuSet is the bot and chat whose command menu (setMyCommands) was set.
	menuSet   string
	lastCmd   string
	lastCmdAt time.Time
	cmdTimes  []time.Time // the last minute's commands, for the limit
	strangers time.Time   // when a stranger's message was last logged
}

// CommandsStatus is the Bildirimler page's view of the commands.
type CommandsStatus struct {
	// User may give commands: the person who linked the chat.
	User string `json:"user,omitempty"`
	// Off says why commands do not work in this chat, when they do not.
	Off string `json:"off,omitempty"`
	// Reading: the bot is being read (a read worked since the last failure); Listening:
	// the first read waits at Telegram (none has come back yet, none failed); Paused: the
	// reader stands aside while a link is made.
	Reading       bool      `json:"reading"`
	Listening     bool      `json:"listening,omitempty"`
	Paused        bool      `json:"paused,omitempty"`
	LastOK        time.Time `json:"last_ok,omitzero"`
	LastErr       string    `json:"last_error,omitempty"`
	LastErrAt     time.Time `json:"last_error_at,omitzero"`
	Failing       time.Time `json:"failing,omitzero"`
	LastCommand   string    `json:"last_command,omitempty"`
	LastCommandAt time.Time `json:"last_command_at,omitzero"`
}

// tgUpdate is the part of an update the reader looks at.
type tgUpdate struct {
	UpdateID      int64       `json:"update_id"`
	Message       *tgMessage  `json:"message"`
	CallbackQuery *tgCallback `json:"callback_query"`
}

// tgCallback is a press on a button under one of the bot's messages.
type tgCallback struct {
	ID      string     `json:"id"`
	From    tgUser     `json:"from"`
	Message *tgMessage `json:"message"` // none for an inline message
	Data    string     `json:"data"`
}

// tgButton is a button the agent put under a message: what a press does, in which chat
// and under which message it counts, until when. Once: it does something (P1b's
// [Evet]) — used up by its first press, before the work starts, and never taken from
// a backlog (a press found waiting may be hours old: Telegram does not say when it was
// made); such a button should ask again before it acts.
type tgButton struct {
	// Kind: "refresh" (/durum again, in place), "site" (proposes Act for Site: asks to
	// confirm) or "confirm" (the question's [Evet] — data <code>/y — and [Vazgeç] — /n).
	Kind    string    `json:"kind"`
	Chat    int64     `json:"chat"`
	Msg     int64     `json:"msg,omitempty"` // 0: the message's id was not learnt
	Once    bool      `json:"once,omitempty"`
	Expires time.Time `json:"expires"`
	// A site's button (notify_sites.go): the action ("park", "extend", "unpark"), its
	// months (extend, unpark; 0: no end), the site's end date and parking when the button
	// was made (a press after they changed does nothing), and the end date it gives.
	Site      string `json:"site,omitempty"`
	Act       string `json:"act,omitempty"`
	Months    int    `json:"months,omitempty"`
	Was       string `json:"was,omitempty"`
	WasParked bool   `json:"was_parked,omitempty"`
	To        string `json:"to,omitempty"`
}

// commander is whose commands count: the person who sent the link code; for a link
// made before P1a, the person of a private chat (its id is theirs). off says why
// nobody's do — a channel, or a group linked before P1a.
func (c telegramConfig) commander() (int64, string) {
	switch {
	case c.UserID != 0:
		return c.UserID, ""
	case c.ChatID > 0:
		return c.ChatID, ""
	}
	return 0, "sohbeti kimin bağladığı bilinmiyor (kanal ya da eski bir grup bağlantısı): komutlar için özel sohbette ya da grupta yeniden bağla"
}

// commanderName is the commander as the chat and the panel call them.
func (c telegramConfig) commanderName() string {
	if c.User != "" {
		return c.User
	}
	return c.Chat // a private chat linked before P1a: its name is the person's
}

// allowed tells whether a message or a press is the commander's, in the linked chat.
func (c telegramConfig) allowed(chat int64, from *tgUser) bool {
	who, off := c.commander()
	return off == "" && chat == c.ChatID && from != nil && !from.IsBot && from.ID == who
}

// ---- the loop ----

// Read answers the chat's commands and buttons until ctx is done. On the way out it
// tells Telegram what was handled, so that an agent restart does not answer it again.
func (n *Notifier) Read(ctx context.Context) {
	n.init()
	for ctx.Err() == nil {
		if wait := n.readOnce(ctx); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
			case <-t.C:
			case <-n.readWake:
			}
			t.Stop()
		}
	}
	n.mu.Lock()
	token, offset := n.rd.token, n.rd.offset
	n.mu.Unlock()
	if token != "" && offset != 0 {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = n.callFor(cctx, 3*time.Second, token, "getUpdates", url.Values{"offset": {strconv.FormatInt(offset, 10)},
			"timeout": {"0"}, "limit": {"1"}, "allowed_updates": {allowedUpdates}}, nil)
	}
}

func (n *Notifier) wakeReader() {
	n.init()
	select {
	case n.readWake <- struct{}{}:
	default:
	}
}

// readOnce reads the bot once and handles what came; it returns how long to wait
// before the next read.
func (n *Notifier) readOnce(ctx context.Context) time.Duration {
	n.init()
	if n.Rehearsal.Active() {
		return readIdle
	}
	now := n.now()
	n.mu.Lock()
	if l := n.link; l != nil && now.Before(l.expires) {
		n.mu.Unlock()
		return min(readIdle, l.expires.Sub(now)+time.Second)
	}
	// The settings are read under mu, after the link check: FinishLink saves them before
	// it clears the link and points the reader past the code, in one hold of mu.
	cfg, err := n.config()
	if err != nil {
		n.mu.Unlock()
		return readIdle // no chat: linking one wakes the reader
	}
	if now.Before(n.rd.retryAt) {
		d := n.rd.retryAt.Sub(now)
		n.mu.Unlock()
		return d
	}
	if n.rd.token != cfg.Token {
		n.rd = readState{token: cfg.Token}
	}
	offset, live := n.rd.offset, n.rd.live
	_, off := cfg.commander()
	menu := off == "" && n.rd.menuSet != menuKey(cfg)
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Under mu with the link check: StartLink either sees this read and cuts it off, or
	// is seen by the next one.
	n.rd.cancel = cancel
	n.mu.Unlock()

	if menu {
		n.setMenu(rctx, cfg)
	}
	start := time.Now()
	params := url.Values{"timeout": {strconv.Itoa(pollWait)}, "limit": {strconv.Itoa(pollBatch)}, "allowed_updates": {allowedUpdates}}
	if offset != 0 {
		params.Set("offset", strconv.FormatInt(offset, 10))
	}
	var raws []json.RawMessage
	err = n.callFor(rctx, pollLimit, cfg.Token, "getUpdates", params, &raws)
	took := time.Since(start)
	n.mu.Lock()
	n.rd.cancel = nil
	same := n.rd.token == cfg.Token
	n.mu.Unlock()
	if ctx.Err() != nil || rctx.Err() != nil || !same {
		return 0 // stopped, or cut off for a link or a removal: look again
	}
	if err != nil {
		return n.readFailed(err)
	}
	// Answered at once by a reader that was away: these were waiting (a backlog). An
	// answer Telegram held brought them as they came.
	backlog := !live && took < readQuick
	ups := decodeUpdates(raws, n.logf)
	n.mu.Lock()
	n.rd.lastOK, n.rd.live = n.now(), true
	n.rd.lastErr, n.rd.errAt, n.rd.failing, n.rd.retryAt, n.rd.backoff = "", time.Time{}, time.Time{}, time.Time{}, 0
	if len(raws) == 0 {
		n.rd.offset = 0 // everything before it is confirmed
	}
	n.mu.Unlock()
	fresh := 0
	for _, u := range ups {
		n.mu.Lock()
		if n.rd.token != cfg.Token {
			n.mu.Unlock()
			return 0 // linked anew meanwhile: the rest belongs to the old bot
		}
		if n.rd.offset != 0 && u.UpdateID < n.rd.offset {
			n.mu.Unlock()
			continue // read already
		}
		n.rd.offset = u.UpdateID + 1
		n.mu.Unlock()
		fresh++
		n.handle(ctx, cfg, u, backlog)
	}
	if fresh == 0 && took < readQuick {
		return readQuick - took
	}
	return 0
}

// decodeUpdates reads an answer's updates one by one, oldest first: one that does not
// decode is read past (by its number) instead of holding up the rest for good.
func decodeUpdates(raws []json.RawMessage, logf func(string, ...any)) []tgUpdate {
	out := make([]tgUpdate, 0, len(raws))
	bad := 0
	for _, raw := range raws {
		var u tgUpdate
		if err := json.Unmarshal(raw, &u); err != nil {
			var id struct {
				UpdateID int64 `json:"update_id"`
			}
			if json.Unmarshal(raw, &id) != nil || id.UpdateID == 0 {
				bad++
				continue
			}
			u, bad = tgUpdate{UpdateID: id.UpdateID}, bad+1
		}
		out = append(out, u)
	}
	if bad > 0 && logf != nil {
		logf("Telegram'dan %d güncelleme okunamadı, atlandı", bad)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdateID < out[j].UpdateID })
	return out
}

// readFailed keeps the error for the panel and works out the wait before the next read.
func (n *Notifier) readFailed(err error) time.Duration {
	now := n.now()
	var te *telegramError
	errors.As(err, &te)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.rd.backoff == 0 {
		n.rd.backoff = readBackoff
	} else {
		n.rd.backoff = min(n.rd.backoff*2, readBackoffMax)
	}
	if te != nil {
		switch {
		case te.code == 401 || te.code == 404:
			n.rd.backoff = readBackoffMax // the token is gone: sending says so too
		case te.code == 409:
			n.rd.backoff = max(n.rd.backoff, 30*time.Second)
		case te.code == 429 && te.retryAfter > 0:
			n.rd.backoff = max(n.rd.backoff, time.Duration(te.retryAfter)*time.Second)
		}
	}
	if n.rd.lastErr != err.Error() {
		n.logf("Telegram okunamadı (komutlar): %v", err)
	}
	n.rd.lastErr, n.rd.errAt, n.rd.live = err.Error(), now, false
	if n.rd.failing.IsZero() {
		n.rd.failing = now
	}
	n.rd.retryAt = now.Add(n.rd.backoff)
	return n.rd.backoff
}

// setMenu gives the linked chat the bot's command menu (Telegram's "/" list; nobody
// else sees it); once per chat and start, failing quietly — commands work without it.
func (n *Notifier) setMenu(ctx context.Context, cfg telegramConfig) {
	cmds, _ := json.Marshal([]map[string]string{
		{"command": "durum", "description": "Sunucunun durumu: işlemci, bellek, disk, siteler, yedek"},
		{"command": "siteler", "description": "Süreli ve parktaki siteler"},
		{"command": "yardim", "description": "Komutlar"},
	})
	scope, _ := json.Marshal(map[string]any{"type": "chat", "chat_id": cfg.ChatID})
	if err := n.call(ctx, cfg.Token, "setMyCommands", url.Values{"commands": {string(cmds)}, "scope": {string(scope)}}, nil); err != nil {
		if ctx.Err() != nil {
			return // cut off: tried again with the next read
		}
		n.logf("Telegram komut menüsü ayarlanamadı (komutlar yine çalışır): %v", err)
	}
	n.mu.Lock()
	if n.rd.token == cfg.Token {
		n.rd.menuSet = menuKey(cfg)
	}
	n.mu.Unlock()
}

func menuKey(cfg telegramConfig) string { return cfg.Bot + "/" + strconv.FormatInt(cfg.ChatID, 10) }

// ---- what came ----

// handle answers one update; backlog: it was found waiting, not seen as it came.
func (n *Notifier) handle(ctx context.Context, cfg telegramConfig, u tgUpdate, backlog bool) {
	switch {
	case u.CallbackQuery != nil:
		n.handleButton(ctx, cfg, u.CallbackQuery, backlog)
	case u.Message != nil:
		n.handleMessage(ctx, cfg, u.Message, backlog)
	}
}

// parseCommand reads "/durum" or "/durum@bot args": cmd is the command in lower case,
// empty when it is meant for another bot; ok is false for text that is no command.
func parseCommand(text, bot string) (cmd string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") {
		return "", false
	}
	first := strings.Fields(t)[0][1:]
	name, to, addressed := strings.Cut(first, "@")
	if addressed && !strings.EqualFold(to, bot) {
		return "", true
	}
	return strings.ToLower(name), true
}

func (n *Notifier) handleMessage(ctx context.Context, cfg telegramConfig, m *tgMessage, backlog bool) {
	if !cfg.allowed(m.Chat.ID, m.From) {
		n.stranger(m)
		return
	}
	now := n.now()
	// A forwarded message is someone's words passed on, not a command; one that waited
	// too long was meant for then (only a backlog is judged by its date: the server's
	// clock decides nothing about what comes as it happens).
	if m.forwarded() || (backlog && m.Date > 0 && now.Sub(time.Unix(m.Date, 0)) > staleCommand) {
		return
	}
	cmd, isCmd := parseCommand(m.Text, cfg.Bot)
	if !isCmd {
		// Plain text: a hint in a private chat; a group's talk is not for the bot.
		if m.Chat.Type == "private" && strings.TrimSpace(m.Text) != "" && n.allowCommand(now) {
			n.reply(ctx, cfg, n.helpText(""))
		}
		return
	}
	if cmd == "" || !n.allowCommand(now) {
		return
	}
	n.noteCommand(cfg, "/"+cmd, now)
	switch cmd {
	case "durum", "status":
		n.sendReport(ctx, cfg)
	case "siteler", "sites":
		n.sendSites(ctx, cfg)
	case "start", "yardim", "yardım", "help":
		n.reply(ctx, cfg, n.helpText(""))
	default:
		n.reply(ctx, cfg, n.helpText(cmd))
	}
}

func (n *Notifier) handleButton(ctx context.Context, cfg telegramConfig, q *tgCallback, backlog bool) {
	if q.Message == nil || !cfg.allowed(q.Message.Chat.ID, &q.From) {
		// The press only stops spinning: nothing is said to someone else.
		n.answerButton(ctx, cfg, q.ID, "")
		return
	}
	now := n.now()
	if !n.allowCommand(now) {
		n.answerButton(ctx, cfg, q.ID, "Çok sık; biraz sonra yeniden dene.")
		return
	}
	// A question's two answers share its code: "<code>/y", "<code>/n".
	code, answer, _ := strings.Cut(q.Data, "/")
	b, ok, waited := n.takeButton(code, q.Message.Chat.ID, q.Message.MessageID, now, backlog)
	switch {
	case waited:
		n.answerButton(ctx, cfg, q.ID, "Sunucu yeni açıldı ya da bağlantısı koptu; bu basış eski olabilir. İstiyorsan yeniden bas.")
		return
	case !ok:
		n.answerButton(ctx, cfg, q.ID, "Bu düğmenin süresi doldu: /durum ya da /siteler yaz.")
		return
	}
	switch b.Kind {
	case "refresh":
		n.noteCommand(cfg, "Yenile", now)
		err := n.editMessage(ctx, cfg, q.Message.MessageID, n.reportText(ctx), refreshMarkup(code))
		if err != nil {
			n.logf("Telegram mesajı güncellenemedi: %v", err)
			n.answerButton(ctx, cfg, q.ID, "Güncellenemedi; /durum yaz.")
			return
		}
		n.renewButton(code, now.Add(buttonTTL))
		n.answerButton(ctx, cfg, q.ID, "Güncellendi")
	case "site":
		n.noteCommand(cfg, actLabel(b), now)
		n.propose(ctx, cfg, q, b)
	case "confirm":
		what := "Vazgeç"
		if answer == "y" {
			what = "Evet"
		}
		n.noteCommand(cfg, actLabel(b)+" → "+what, now)
		n.confirm(ctx, cfg, q, b, answer == "y")
	default:
		n.answerButton(ctx, cfg, q.ID, "Bu düğme bu sürümde çalışmıyor.")
	}
}

// stranger notes, now and then, that someone else wrote to the bot (never what).
func (n *Notifier) stranger(m *tgMessage) {
	now := n.now()
	n.mu.Lock()
	log := now.Sub(n.rd.strangers) >= strangerLogEvery
	if log {
		n.rd.strangers = now
	}
	n.mu.Unlock()
	if log {
		n.logf("Telegram: bağlı olmayan bir sohbetten ya da kişiden mesaj geldi (sohbet %d), yok sayıldı", m.Chat.ID)
	}
}

// allowCommand counts a command against the limit; false: over it, left alone.
func (n *Notifier) allowCommand(now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	keep := n.rd.cmdTimes[:0]
	for _, t := range n.rd.cmdTimes {
		if now.Sub(t) < time.Minute && !t.After(now) {
			keep = append(keep, t)
		}
	}
	n.rd.cmdTimes = keep
	if len(keep) >= commandsPerMinute {
		return false
	}
	n.rd.cmdTimes = append(n.rd.cmdTimes, now)
	return true
}

// noteCommand keeps the last command for the panel and records it in the audit log:
// Telegram is a way into the server, so what came through it is on record.
func (n *Notifier) noteCommand(cfg telegramConfig, what string, now time.Time) {
	n.mu.Lock()
	n.rd.lastCmd, n.rd.lastCmdAt = what, now
	n.mu.Unlock()
	if n.Audit == nil {
		return
	}
	if err := n.Audit.Record(AuditEntry{Actor: Actor{User: cfg.commanderName(), Via: "telegram"}, Action: "telegram.command", Target: what, OK: true}); err != nil {
		n.logf("denetim kaydı yazılamadı: %v", err)
	}
}

func (n *Notifier) helpText(unknown string) string {
	var b strings.Builder
	if unknown != "" {
		fmt.Fprintf(&b, "Bilinmeyen komut: /%s\n\n", clipMsg(strings.ToValidUTF8(unknown, "?"), 40))
	}
	fmt.Fprintf(&b, "%s Komutlar:\n/durum — sunucunun durumu: işlemci, bellek, disk, siteler, son yedek, açık uyarılar\n"+
		"/siteler — süreli ve parktaki siteler; süre uzatma, parka alma, yeniden açma\n/yardim — bu liste\n\n"+
		"Uyarılar ve sitelerin süre hatırlatmaları buraya kendiliğinden gelir.", n.prefix())
	return b.String()
}

func (n *Notifier) reply(ctx context.Context, cfg telegramConfig, text string) {
	if err := n.send(ctx, cfg, text); err != nil {
		n.logf("Telegram'a cevap gönderilemedi: %v", err)
	}
}

func (n *Notifier) reportText(ctx context.Context) string {
	if n.Report == nil {
		return n.prefix() + " Sunucunun durumu bu kurulumda alınamıyor."
	}
	return clipMsg(renderReport(n.prefix(), n.Report(ctx), n.now()), telegramMax)
}

// sendReport answers /durum: the status with a button that brings it up to date.
func (n *Notifier) sendReport(ctx context.Context, cfg telegramConfig) {
	text := n.reportText(ctx)
	code, err := n.newButton(tgButton{Kind: "refresh", Chat: cfg.ChatID, Expires: n.now().Add(buttonTTL)})
	markup := ""
	if err == nil {
		markup = refreshMarkup(code)
	} else {
		n.logf("Telegram düğmesi kaydedilemedi (durum düğmesiz gidiyor): %v", err)
	}
	id, err := n.sendMarkup(ctx, cfg, text, markup)
	switch {
	case err != nil:
		n.logf("Telegram'a cevap gönderilemedi: %v", err)
		n.dropButton(code)
	case markup != "" && id != 0:
		n.bindButton(code, id)
	}
}

func refreshMarkup(code string) string {
	b, _ := json.Marshal(map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "🔄 Yenile", "callback_data": code}}}})
	return string(b)
}

// editMessage puts text (and markup's buttons; "": none) in place of a message's.
func (n *Notifier) editMessage(ctx context.Context, cfg telegramConfig, msg int64, text, markup string) error {
	params := url.Values{
		"chat_id":              {strconv.FormatInt(cfg.ChatID, 10)},
		"message_id":           {strconv.FormatInt(msg, 10)},
		"text":                 {text},
		"link_preview_options": {`{"is_disabled":true}`},
	}
	if markup != "" {
		params.Set("reply_markup", markup) // left out, the buttons go
	}
	err := n.call(ctx, cfg.Token, "editMessageText", params, nil)
	var te *telegramError
	if errors.As(err, &te) && te.code == 400 && strings.Contains(strings.ToLower(te.desc), "not modified") {
		return nil // pressed twice within a second: it is up to date
	}
	return err
}

func (n *Notifier) answerButton(ctx context.Context, cfg telegramConfig, id, text string) {
	if id == "" {
		return
	}
	params := url.Values{"callback_query_id": {id}}
	if text != "" {
		params.Set("text", text)
	}
	if err := n.call(ctx, cfg.Token, "answerCallbackQuery", params, nil); err != nil {
		n.logf("Telegram düğmesi cevaplanamadı: %v", err)
	}
}

// ---- the panel ----

func (n *Notifier) commandsStatus(cfg telegramConfig) *CommandsStatus {
	out := &CommandsStatus{}
	if _, off := cfg.commander(); off != "" {
		out.Off = off
	} else {
		out.User = cfg.commanderName()
	}
	now := n.now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.rd.token == cfg.Token {
		out.LastOK, out.LastErr, out.LastErrAt, out.Failing = n.rd.lastOK, n.rd.lastErr, n.rd.errAt, n.rd.failing
		out.LastCommand, out.LastCommandAt = n.rd.lastCmd, n.rd.lastCmdAt
		// Just linked or started: the first read waits at Telegram for up to pollWait.
		out.Listening = out.LastOK.IsZero() && out.Failing.IsZero() && n.rd.cancel != nil
	}
	out.Reading = !out.LastOK.IsZero() && out.Failing.IsZero()
	out.Paused = n.link != nil && now.Before(n.link.expires)
	return out
}

// readAlerts is reading failing for a while (not while a link is made: the reader
// stands aside then).
func (n *Notifier) readAlerts(cfg telegramConfig, now time.Time) []Alert {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.rd.token != cfg.Token || n.rd.failing.IsZero() || now.Sub(n.rd.failing) < readAlertAfter ||
		(n.link != nil && now.Before(n.link.expires)) {
		return nil
	}
	return []Alert{{Key: "telegram:read", Level: "warn", Title: "Telegram komutları okunamıyor",
		Detail: n.rd.lastErr, Link: "/notify", Since: n.rd.failing}}
}

// ---- buttons ----

// The buttons live in a file of their own next to the notifier's state: a press can
// come after an agent restart. Their codes are no secret on their own — a press counts
// only from the commander, in the chat, under the message it was made for —, but the
// file is root-only all the same.

func (n *Notifier) buttonsPath() string {
	return filepath.Join(filepath.Dir(n.StatePath), "telegram-buttons.json")
}

// loadButtons reads the file the first time (btnMu held).
func (n *Notifier) loadButtons() {
	if n.btnLoaded {
		return
	}
	n.btnLoaded = true
	n.buttons = map[string]tgButton{}
	if data, err := os.ReadFile(n.buttonsPath()); err == nil {
		_ = json.Unmarshal(data, &n.buttons) // a broken file: the old buttons say "expired"
	}
	if n.buttons == nil {
		n.buttons = map[string]tgButton{}
	}
}

// saveButtons writes them through, without the expired ones (btnMu held).
func (n *Notifier) saveButtons(now time.Time) error {
	for c, b := range n.buttons {
		if !now.Before(b.Expires) {
			delete(n.buttons, c)
		}
	}
	data, err := json.MarshalIndent(n.buttons, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(n.buttonsPath()), 0o700); err != nil {
		return err
	}
	return writeSynced(n.buttonsPath(), append(data, '\n'))
}

// newButton keeps b under a fresh code; past buttonsMax others go (evictBefore).
func (n *Notifier) newButton(b tgButton) (string, error) {
	codes, err := n.newButtons([]tgButton{b})
	if err != nil {
		return "", err
	}
	return codes[0], nil
}

// newButtons keeps every one of list under a fresh code, in one write.
func (n *Notifier) newButtons(list []tgButton) ([]string, error) {
	codes := make([]string, len(list))
	for i := range list {
		code, err := randomCode(buttonCodeLen)
		if err != nil {
			return nil, err
		}
		codes[i] = code
	}
	now := n.now()
	n.btnMu.Lock()
	defer n.btnMu.Unlock()
	// Not for a chat removed (or relinked) meanwhile: Remove clears the file after the
	// settings are gone.
	cfg, err := n.config()
	if err != nil {
		return nil, errNoNotify
	}
	for _, b := range list {
		if cfg.ChatID != b.Chat {
			return nil, errNoNotify
		}
	}
	n.loadButtons()
	for len(n.buttons)+len(list) > buttonsMax && len(n.buttons) > 0 {
		first := ""
		for c, x := range n.buttons {
			if first == "" || evictBefore(x, n.buttons[first]) {
				first = c
			}
		}
		delete(n.buttons, first)
	}
	for i, b := range list {
		n.buttons[codes[i]] = b
	}
	if err := n.saveButtons(now); err != nil {
		for _, c := range codes {
			delete(n.buttons, c)
		}
		return nil, err
	}
	return codes, nil
}

// takeButton finds the button a press is for: it must not have expired and must be
// pressed in its chat, under its message. A Once button is used up here, before any
// work, and is not taken from a backlog (waited: it stays, to be pressed again).
func (n *Notifier) takeButton(code string, chat, msg int64, now time.Time, backlog bool) (b tgButton, ok, waited bool) {
	n.btnMu.Lock()
	defer n.btnMu.Unlock()
	n.loadButtons()
	b, ok = n.buttons[code]
	if !ok || !now.Before(b.Expires) || b.Chat != chat || (b.Msg != 0 && b.Msg != msg) {
		return tgButton{}, false, false
	}
	if b.Once {
		if backlog {
			return tgButton{}, false, true
		}
		delete(n.buttons, code)
		if err := n.saveButtons(now); err != nil {
			// Not used up on disk: refused, so that a restart cannot make it work twice.
			n.buttons[code] = b
			n.logf("Telegram düğmeleri kaydedilemedi: %v", err)
			return tgButton{}, false, false
		}
	}
	return b, true, false
}

// evictBefore: a goes before b when the buttons are too many — anything before a
// question's answers (they expire soonest, and are the ones someone is about to press),
// then the one closest to expiring.
func evictBefore(a, b tgButton) bool {
	if ac, bc := a.Kind == "confirm", b.Kind == "confirm"; ac != bc {
		return !ac
	}
	return a.Expires.Before(b.Expires)
}

func (n *Notifier) bindButton(code string, msg int64) {
	n.bindButtons([]string{code}, msg)
}

// bindButtons ties buttons to the message they went under, in one write.
func (n *Notifier) bindButtons(codes []string, msg int64) {
	if len(codes) == 0 {
		return
	}
	n.btnMu.Lock()
	defer n.btnMu.Unlock()
	n.loadButtons()
	for _, c := range codes {
		if b, ok := n.buttons[c]; ok {
			b.Msg = msg
			n.buttons[c] = b
		}
	}
	if err := n.saveButtons(n.now()); err != nil {
		n.logf("Telegram düğmeleri kaydedilemedi: %v", err)
	}
}

func (n *Notifier) renewButton(code string, until time.Time) {
	n.editButton(code, func(b *tgButton) { b.Expires = until })
}

func (n *Notifier) editButton(code string, fn func(*tgButton)) {
	n.btnMu.Lock()
	defer n.btnMu.Unlock()
	n.loadButtons()
	b, ok := n.buttons[code]
	if !ok {
		return
	}
	fn(&b)
	n.buttons[code] = b
	if err := n.saveButtons(n.now()); err != nil {
		n.logf("Telegram düğmeleri kaydedilemedi: %v", err)
	}
}

func (n *Notifier) dropButton(code string) {
	if code == "" {
		return
	}
	n.dropButtons([]string{code})
}

// dropButtons forgets buttons whose message did not go, in one write.
func (n *Notifier) dropButtons(codes []string) {
	if len(codes) == 0 {
		return
	}
	n.btnMu.Lock()
	defer n.btnMu.Unlock()
	n.loadButtons()
	gone := false
	for _, c := range codes {
		if _, ok := n.buttons[c]; ok {
			delete(n.buttons, c)
			gone = true
		}
	}
	if gone {
		if err := n.saveButtons(n.now()); err != nil {
			n.logf("Telegram düğmeleri kaydedilemedi: %v", err)
		}
	}
}

// clearButtons forgets every button (another chat, or none).
func (n *Notifier) clearButtons() {
	n.btnMu.Lock()
	defer n.btnMu.Unlock()
	n.buttons, n.btnLoaded = map[string]tgButton{}, true
	if err := os.Remove(n.buttonsPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		n.logf("Telegram düğmeleri silinemedi: %v", err)
	}
}
