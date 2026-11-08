package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// Sites from the chat (Faz P1b, panel-tasarim §28.3): the end dates' messages, /siteler,
// and their buttons. A button under such a message only proposes (park, more time,
// back into service); what it proposes is asked once more, with its exact effect, and
// only that answer's [Evet] acts — once, within confirmTTL. Every button carries the
// site as it was when the button was made: pressed after the site changed (parked,
// given another end date), it does nothing, so an old [+1 yıl] cannot extend twice.

// siteDoer is what the chat can do with sites (*Expiry): look them up, park one,
// bring one back or give it more time — nothing that deletes.
type siteDoer interface {
	Term(site string) (SiteTerm, bool)
	Terms() ([]SiteTerm, error)
	SetExpiry(site, to string, was *string, actor Actor) error
	StartPark(site string, actor Actor, done func(Job)) (string, error)
	StartUnpark(site, to string, actor Actor, done func(Job)) (string, error)
}

// Terms lists the sites' end dates and parking (for the chat's /siteler).
func (e *Expiry) Terms() ([]SiteTerm, error) { return e.Sites.Terms() }

const (
	// confirmTTL is how long an [Evet] works: an answer, not a standing order.
	confirmTTL = 5 * time.Minute
	// sitesRowsMax bounds /siteler's buttons (a row per site that needs a decision).
	sitesRowsMax = 10
	// sitesListMax bounds the sites /siteler names.
	sitesListMax = 40
)

// The actions a site's button proposes.
const (
	actPark   = "park"
	actExtend = "extend"
	actUnpark = "unpark"
)

// siteAct is a proposal under a message.
type siteAct struct {
	Act    string
	Months int // extend, unpark: 1 or 12; unpark 0: with no end date
	Label  string
}

// actsFor is what can be proposed for a site as it stands, left days before its end
// (ok: it has one): bringing a parked one back; for one ending within a week, more
// time; from its last day on, parking too.
func actsFor(t SiteTerm, left int, ok bool, named bool) []siteAct {
	name, park, open := "", "⏸ Parka al", "▶ Yayına al"
	if named {
		name = t.Name + ": "
		park, open = "⏸ "+name+"parka al", "▶ "+name+"yayına al"
	}
	switch {
	case t.Parked != nil && !ok:
		return []siteAct{{Act: actUnpark, Label: open}}
	case t.Parked != nil:
		return []siteAct{{Act: actUnpark, Months: 1, Label: "▶ " + name + "+1 ay ile aç"}, {Act: actUnpark, Months: 12, Label: "▶ " + name + "+1 yıl ile aç"}}
	case !ok || left > remindStages[0]:
		return nil
	}
	more := []siteAct{{Act: actExtend, Months: 1, Label: name + "+1 ay"}, {Act: actExtend, Months: 12, Label: name + "+1 yıl"}}
	if left <= 0 {
		return append([]siteAct{{Act: actPark, Label: park}}, more...)
	}
	return more
}

// SendSiteNotice tells the chat of a site's end date (Expiry.Check): left days before
// it — 7, 3, the day itself (park?) or past it (park? again) — with buttons.
func (n *Notifier) SendSiteNotice(ctx context.Context, t SiteTerm, left int, now time.Time) error {
	n.init()
	if n.Rehearsal.Active() {
		return errNoNotify // a rehearsal speaks for nobody
	}
	cfg, err := n.config()
	if err != nil {
		return errNoNotify
	}
	var b strings.Builder
	switch {
	case left > 0:
		fmt.Fprintf(&b, "⏳ %s %s sitesinin süresi %d gün sonra doluyor: %s.", n.prefix(), t.Name, left, dayLongTR(t.Expires))
	case left == 0:
		fmt.Fprintf(&b, "⌛ %s %s sitesinin süresi bugün doluyor (%s). Parka alınsın mı?", n.prefix(), t.Name, dayLongTR(t.Expires))
	default:
		fmt.Fprintf(&b, "⌛ %s %s sitesinin süresi %d gün önce doldu (%s); site hâlâ açık. Parka alınsın mı?", n.prefix(), t.Name, -left, dayTR(t.Expires))
	}
	b.WriteString("\n" + clipMsg(strings.Join(t.Domains, ", "), 300))
	if left <= 0 {
		b.WriteString("\nParka alınırsa container durur, ziyaretçiler nötr bir bakım sayfası görür; veri ve yedekler silinmez. " +
			"Onay gelmezse site açık kalır, her gün " + fmt.Sprintf("%02d:00", remindHour) + "'da yeniden sorulur.")
	}
	if u := n.panelURL("/sites/" + t.Name); u != "" {
		b.WriteString("\n" + u)
	}
	acts := actsFor(t, left, true, false)
	_, err = n.sendActs(ctx, cfg, b.String(), t, [][]siteAct{acts}, now)
	return err
}

// sendActs sends text with rows of buttons, all for site t; it returns the message's id.
func (n *Notifier) sendActs(ctx context.Context, cfg telegramConfig, text string, t SiteTerm, rows [][]siteAct, now time.Time) (int64, error) {
	return n.sendRows(ctx, cfg, text, rows, func(int) SiteTerm { return t }, now)
}

// sendRows sends text with rows of buttons; site(i) is the site of row i.
func (n *Notifier) sendRows(ctx context.Context, cfg telegramConfig, text string, rows [][]siteAct, site func(i int) SiteTerm, now time.Time) (int64, error) {
	markup, codes, err := n.actButtons(cfg, rows, site, now)
	if err != nil {
		n.logf("Telegram düğmeleri kaydedilemedi (mesaj düğmesiz gidiyor): %v", err)
		markup, codes = "", nil
	}
	id, err := n.sendMarkup(ctx, cfg, text, markup)
	if err != nil {
		n.dropButtons(codes)
		return 0, err
	}
	if id != 0 {
		n.bindButtons(codes, id)
	}
	return id, nil
}

// actButtons keeps a proposal button for every act and renders the keyboard.
func (n *Notifier) actButtons(cfg telegramConfig, rows [][]siteAct, site func(i int) SiteTerm, now time.Time) (string, []string, error) {
	var list []tgButton
	for i, row := range rows {
		t := site(i)
		for _, a := range row {
			list = append(list, tgButton{Kind: "site", Chat: cfg.ChatID, Site: t.Name, Act: a.Act, Months: a.Months,
				Was: t.Expires, WasParked: t.Parked != nil, Expires: now.Add(buttonTTL)})
		}
	}
	if len(list) == 0 {
		return "", nil, nil
	}
	codes, err := n.newButtons(list)
	if err != nil {
		return "", nil, err
	}
	kb := [][]map[string]string{}
	k := 0
	for _, row := range rows {
		var line []map[string]string
		for _, a := range row {
			line = append(line, map[string]string{"text": a.Label, "callback_data": codes[k]})
			k++
		}
		if len(line) > 0 {
			kb = append(kb, line)
		}
	}
	b, _ := json.Marshal(map[string]any{"inline_keyboard": kb})
	return string(b), codes, nil
}

// sendSites answers /siteler: sites with an end date, soonest first, and parked ones,
// with a row of buttons for each that needs a decision.
func (n *Notifier) sendSites(ctx context.Context, cfg telegramConfig) {
	if n.Sites == nil {
		n.reply(ctx, cfg, n.prefix()+" Siteler bu kurulumda Telegram'dan görülemiyor.")
		return
	}
	list, err := n.Sites.Terms()
	if err != nil {
		n.reply(ctx, cfg, n.prefix()+" Siteler okunamadı: "+clipMsg(err.Error(), 300))
		return
	}
	now := n.now()
	local := now.Local()
	list = append([]SiteTerm(nil), list...)
	sortTerms(list, local)
	var b strings.Builder
	fmt.Fprintf(&b, "📅 %s Süreli ve parktaki siteler · %s", n.prefix(), local.Format("02.01.2006"))
	var rows [][]siteAct
	var rowSite []SiteTerm
	shown, open := 0, 0
	for _, t := range list {
		left, ok := sites.DaysLeft(t.Expires, local)
		if t.Parked == nil && !ok {
			open++
			continue
		}
		if shown == sitesListMax {
			b.WriteString("\n…")
			break
		}
		shown++
		switch {
		case t.Parked != nil:
			fmt.Fprintf(&b, "\n⏸ %s — parkta (%s tarihinden beri)", t.Name, t.Parked.At.Local().Format("02.01.2006"))
			if ok {
				fmt.Fprintf(&b, "; bitişi %s", dayTR(t.Expires))
			}
		case left <= 0:
			fmt.Fprintf(&b, "\n⌛ %s — süresi %s (%s)", t.Name, leftTR(left), dayTR(t.Expires))
		case left <= remindStages[0]:
			fmt.Fprintf(&b, "\n⏳ %s — %s (%s)", t.Name, leftTR(left), dayLongTR(t.Expires))
		default:
			fmt.Fprintf(&b, "\n• %s — %s (%s)", t.Name, leftTR(left), dayTR(t.Expires))
		}
		if acts := actsFor(t, left, ok, true); len(acts) > 0 && len(rows) < sitesRowsMax {
			rows, rowSite = append(rows, acts), append(rowSite, t)
		}
	}
	if shown == 0 {
		b.Reset()
		fmt.Fprintf(&b, "📅 %s Süreli ya da parktaki site yok. Bitiş tarihi panelde, sitenin sayfasından verilir.", n.prefix())
	} else if open > 0 {
		fmt.Fprintf(&b, "\nSüresiz: %d site", open)
	}
	if u := n.panelURL("/sites"); u != "" {
		b.WriteString("\n" + u)
	}
	if _, err := n.sendRows(ctx, cfg, clipMsg(b.String(), telegramMax), rows, func(i int) SiteTerm { return rowSite[i] }, now); err != nil {
		n.logf("Telegram'a cevap gönderilemedi: %v", err)
	}
}

// stale: the site is no longer as it was when the button was made.
func (b tgButton) stale(t SiteTerm) bool {
	return t.Expires != b.Was || (t.Parked != nil) != b.WasParked
}

// actLabel names a site button's action for the panel's "last command" and the audit.
func actLabel(b tgButton) string {
	switch b.Act {
	case actPark:
		return b.Site + ": parka al"
	case actExtend:
		return fmt.Sprintf("%s: %s", b.Site, monthsTR(b.Months))
	case actUnpark:
		if b.Months == 0 {
			return b.Site + ": yayına al"
		}
		return fmt.Sprintf("%s: %s ile aç", b.Site, monthsTR(b.Months))
	}
	return b.Site + ": " + b.Act
}

func monthsTR(m int) string {
	if m == 12 {
		return "+1 yıl"
	}
	return fmt.Sprintf("+%d ay", m)
}

// propose answers a press on a proposal: the action is asked once more, with what it
// will do, in a message of its own.
func (n *Notifier) propose(ctx context.Context, cfg telegramConfig, q *tgCallback, b tgButton) {
	if n.Sites == nil {
		n.answerButton(ctx, cfg, q.ID, "Bu sunucuda site işlemleri Telegram'dan yapılamıyor.")
		return
	}
	t, ok := n.Sites.Term(b.Site)
	switch {
	case !ok:
		n.answerButton(ctx, cfg, q.ID, b.Site+" adında bir site artık yok.")
		return
	case b.stale(t):
		n.answerButton(ctx, cfg, q.ID, "Bu düğme eski: site o mesajdan sonra değişti. /siteler yaz.")
		return
	}
	now := n.now()
	c := tgButton{Kind: "confirm", Once: true, Chat: cfg.ChatID, Site: t.Name, Act: b.Act, Months: b.Months,
		Was: t.Expires, WasParked: t.Parked != nil, Expires: now.Add(confirmTTL)}
	switch {
	case b.Act == actExtend && t.Parked == nil && t.Expires != "",
		b.Act == actUnpark && t.Parked != nil:
		if b.Months > 0 {
			c.To = sites.Extend(t.Expires, b.Months, now.Local())
		}
	case b.Act == actPark && t.Parked == nil:
	default:
		n.answerButton(ctx, cfg, q.ID, "Bu düğme bu sürümde çalışmıyor.")
		return
	}
	if err := n.askConfirm(ctx, cfg, c, t, ""); err != nil {
		n.logf("Telegram onay mesajı gönderilemedi: %v", err)
		n.answerButton(ctx, cfg, q.ID, "Onay mesajı gönderilemedi; biraz sonra yeniden dene.")
		return
	}
	n.answerButton(ctx, cfg, q.ID, "Onayın bekleniyor")
}

// confirmText is the question an action asks before it acts, and its [Evet].
func (n *Notifier) confirmText(c tgButton, t SiteTerm) (string, string) {
	var b strings.Builder
	yes := "✅ Evet"
	domains := clipMsg(strings.Join(t.Domains, ", "), 300)
	switch c.Act {
	case actPark:
		fmt.Fprintf(&b, "❓ %s %s parka alınsın mı?\nContainer durur; %s bakım sayfası gösterir (HTTP 503). Veri ve yedekler silinmez; "+
			"Telegram'dan ya da panelden yeniden açılabilir.", n.prefix(), t.Name, domains)
		yes = "⏸ Evet, parka al"
	case actExtend:
		fmt.Fprintf(&b, "❓ %s %s sitesinin süresi uzatılsın mı?\nŞu anki bitiş: %s → yeni bitiş: %s (%s).", n.prefix(), t.Name,
			dayTR(t.Expires), dayLongTR(c.To), monthsTR(c.Months))
		yes = "✅ Evet, uzat"
	case actUnpark:
		until := "süresiz"
		if c.To != "" {
			until = "yeni bitiş " + dayLongTR(c.To) + " (" + monthsTR(c.Months) + ")"
		}
		fmt.Fprintf(&b, "❓ %s %s yeniden yayına alınsın mı?\nContainer başlar, %s yeniden siteyi gösterir; %s.", n.prefix(), t.Name, domains, until)
		yes = "▶ Evet, yayına al"
	}
	fmt.Fprintf(&b, "\nBu onay %d dakika geçerli.", int(confirmTTL/time.Minute))
	return b.String(), yes
}

// askConfirm keeps c (fresh code) and asks: in a new message, or — edit set — in place
// of that message (after a refusal that can be tried again), with lead in front.
func (n *Notifier) askConfirm(ctx context.Context, cfg telegramConfig, c tgButton, t SiteTerm, lead string, edit ...int64) error {
	code, err := n.newButton(c)
	if err != nil {
		return err
	}
	text, yes := n.confirmText(c, t)
	if lead != "" {
		text = lead + "\n\n" + text
	}
	kb, _ := json.Marshal(map[string]any{"inline_keyboard": [][]map[string]string{{
		{"text": yes, "callback_data": code + "/y"}, {"text": "Vazgeç", "callback_data": code + "/n"}}}})
	if len(edit) > 0 {
		if err := n.editMessage(ctx, cfg, edit[0], text, string(kb)); err != nil {
			n.dropButton(code)
			return err
		}
		n.bindButton(code, edit[0])
		return nil
	}
	id, err := n.sendMarkup(ctx, cfg, text, string(kb))
	if err != nil {
		n.dropButton(code)
		return err
	}
	if id != 0 {
		n.bindButton(code, id)
	}
	return nil
}

// confirm answers [Evet] (yes) or [Vazgeç] under a question: the button is used up
// already (Once). The site must still be as the question saw it.
func (n *Notifier) confirm(ctx context.Context, cfg telegramConfig, q *tgCallback, c tgButton, yes bool) {
	msg := q.Message.MessageID
	edit := func(text, markup string) {
		if err := n.editMessage(ctx, cfg, msg, text, markup); err != nil {
			n.logf("Telegram mesajı güncellenemedi: %v", err)
		}
	}
	if !yes {
		edit(fmt.Sprintf("Vazgeçildi: %s için bir şey yapılmadı.", c.Site), "")
		n.answerButton(ctx, cfg, q.ID, "Vazgeçildi")
		return
	}
	if n.Sites == nil {
		n.answerButton(ctx, cfg, q.ID, "Bu sunucuda site işlemleri Telegram'dan yapılamıyor.")
		return
	}
	t, ok := n.Sites.Term(c.Site)
	if !ok || c.stale(t) {
		edit(fmt.Sprintf("%s bu arada değişti; bir şey yapılmadı. /siteler yaz.", c.Site), "")
		n.answerButton(ctx, cfg, q.ID, "Bir şey yapılmadı")
		return
	}
	actor := Actor{User: cfg.commanderName(), Via: "telegram"}
	again := func(lead string) {
		c.Expires = n.now().Add(confirmTTL)
		if err := n.askConfirm(ctx, cfg, c, t, lead, msg); err != nil {
			edit(lead+" /siteler ile yeniden dene.", "")
		}
	}
	switch c.Act {
	case actExtend:
		was := c.Was // what the question was asked about: changed since, nothing is done
		err := n.Sites.SetExpiry(c.Site, c.To, &was, actor)
		switch {
		case errors.Is(err, errExpiryChanged):
			edit(fmt.Sprintf("%s bu arada değişti; bir şey yapılmadı. /siteler yaz.", c.Site), "")
			n.answerButton(ctx, cfg, q.ID, "Bir şey yapılmadı")
		case errors.Is(err, errSiteBusy):
			again("⏳ Şu an bu sunucuda bir site işlemi sürüyor; birazdan yeniden dene.")
			n.answerButton(ctx, cfg, q.ID, "Şu an olmadı")
		case err != nil:
			edit(fmt.Sprintf("✗ %s %s sitesinin süresi uzatılamadı: %s", n.prefix(), c.Site, clipMsg(err.Error(), 600)), "")
			n.answerButton(ctx, cfg, q.ID, "Uzatılamadı")
		default:
			edit(fmt.Sprintf("✅ %s %s: yeni bitiş %s (%s).", n.prefix(), c.Site, dayLongTR(c.To), monthsTR(c.Months)), "")
			n.answerButton(ctx, cfg, q.ID, "Uzatıldı")
		}
	case actPark, actUnpark:
		done := func(j Job) { n.jobDone(cfg.ChatID, msg, c, j) }
		var err error
		if c.Act == actPark {
			_, err = n.Sites.StartPark(c.Site, actor, done)
		} else {
			_, err = n.Sites.StartUnpark(c.Site, c.To, actor, done)
		}
		switch {
		case errors.Is(err, ErrBusy) || errors.Is(err, errHeld):
			again("⏳ Şu an başka bir işlem sürüyor (ör. yedek); bitince yeniden dene.")
			n.answerButton(ctx, cfg, q.ID, "Şu an olmadı")
		case err != nil:
			edit(fmt.Sprintf("✗ %s %s: %s", n.prefix(), c.Site, clipMsg(err.Error(), 600)), "")
			n.answerButton(ctx, cfg, q.ID, "Başlatılamadı")
		case c.Act == actPark:
			edit(fmt.Sprintf("⏳ %s %s parka alınıyor…", n.prefix(), c.Site), "")
			n.answerButton(ctx, cfg, q.ID, "Parka alınıyor…")
		default:
			edit(fmt.Sprintf("⏳ %s %s yeniden yayına alınıyor…", n.prefix(), c.Site), "")
			n.answerButton(ctx, cfg, q.ID, "Yayına alınıyor…")
		}
	default:
		n.answerButton(ctx, cfg, q.ID, "Bu düğme bu sürümde çalışmıyor.")
	}
}

// jobDone tells, in the question's place, how a park or an unpark ended; a parked
// site gets the buttons that bring it back.
func (n *Notifier) jobDone(chat, msg int64, c tgButton, j Job) {
	cfg, err := n.config()
	if err != nil || cfg.ChatID != chat {
		return // the chat was removed or relinked meanwhile
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var text, markup string
	var codes []string
	switch {
	case j.Err != "" && c.Act == actPark:
		text = fmt.Sprintf("✗ %s %s parka alınamadı: %s", n.prefix(), c.Site, clipMsg(j.Err, 1500))
	case j.Err != "":
		text = fmt.Sprintf("✗ %s %s yayına alınamadı, site parkta kaldı: %s", n.prefix(), c.Site, clipMsg(j.Err, 1500))
	case c.Act == actPark:
		text = fmt.Sprintf("✅ %s %s parka alındı: container durdu, alan adları bakım sayfasını gösteriyor. Veri ve yedekler yerinde.", n.prefix(), c.Site)
		if t, ok := n.Sites.Term(c.Site); ok && t.Parked != nil {
			left, has := sites.DaysLeft(t.Expires, n.now().Local())
			markup, codes, err = n.actButtons(cfg, [][]siteAct{actsFor(t, left, has, false)}, func(int) SiteTerm { return t }, n.now())
			if err != nil {
				n.logf("Telegram düğmeleri kaydedilemedi: %v", err)
			}
		}
	default:
		until := "süresiz"
		if c.To != "" {
			until = "bitiş " + dayLongTR(c.To)
		}
		text = fmt.Sprintf("✅ %s %s yeniden yayında (%s).", n.prefix(), c.Site, until)
	}
	if err := n.editMessage(ctx, cfg, msg, text, markup); err != nil {
		// The question may be gone (deleted in the chat): the news goes as a message.
		n.logf("Telegram mesajı güncellenemedi (yeni mesaj gidiyor): %v", err)
		id, serr := n.sendMarkup(ctx, cfg, text, markup)
		if serr != nil {
			n.dropButtons(codes)
			n.logf("Telegram'a gönderilemedi: %v", serr)
			return
		}
		msg = id
	}
	if len(codes) > 0 && msg != 0 {
		n.bindButtons(codes, msg)
	}
}
