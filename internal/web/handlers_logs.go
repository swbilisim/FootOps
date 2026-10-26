package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/swbilisim/FootOps/internal/docker"
)

var (
	logTails   = []int{200, 500, 1000, 2000}
	reLogError = regexp.MustCompile(`(?i)\b(error|err|fatal|panic|exception|crit|critical|emerg|failed)\b`)
	reLogWarn  = regexp.MustCompile(`(?i)\bwarn(ing)?\b`)
)

const maxLogQuery = 200

// logLine is one rendered line: its level (a CSS class) and the text split around
// search matches, so the template can mark them without building HTML itself.
type logLine struct {
	Level string
	Parts []logPart
}

type logPart struct {
	Text string
	Hit  bool
}

func lineLevel(l string) string {
	switch {
	case reLogError.MatchString(l):
		return "bad"
	case reLogWarn.MatchString(l):
		return "warn"
	}
	return ""
}

// filterLogs keeps lines at or above level ("", "warn", "error") that contain q
// (case-insensitive) and splits them around the matches.
func filterLogs(lines []string, q, level string) []logLine {
	var re *regexp.Regexp
	if q != "" {
		re = regexp.MustCompile("(?i)" + regexp.QuoteMeta(q))
	}
	var out []logLine
	for _, l := range lines {
		lv := lineLevel(l)
		if (level == "error" && lv != "bad") || (level == "warn" && lv == "") {
			continue
		}
		ll := logLine{Level: lv}
		if re == nil {
			ll.Parts = []logPart{{Text: l}}
			out = append(out, ll)
			continue
		}
		hits := re.FindAllStringIndex(l, -1)
		if hits == nil {
			continue
		}
		at := 0
		for _, h := range hits {
			if h[0] > at {
				ll.Parts = append(ll.Parts, logPart{Text: l[at:h[0]]})
			}
			ll.Parts = append(ll.Parts, logPart{Text: l[h[0]:h[1]], Hit: true})
			at = h[1]
		}
		if at < len(l) {
			ll.Parts = append(ll.Parts, logPart{Text: l[at:]})
		}
		out = append(out, ll)
	}
	return out
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !docker.ValidName(name) {
		http.NotFound(w, r)
		return
	}
	qs := r.URL.Query()
	tail := logTails[0]
	if n, err := strconv.Atoi(qs.Get("tail")); err == nil {
		for _, t := range logTails {
			if n == t {
				tail = n
			}
		}
	}
	// Invalid UTF-8 would make the search pattern fail to compile.
	q := strings.TrimSpace(strings.ToValidUTF8(qs.Get("q"), ""))
	if r := []rune(q); len(r) > maxLogQuery {
		q = string(r[:maxLogQuery])
	}
	level := qs.Get("level")
	if level != "warn" && level != "error" {
		level = ""
	}
	live := qs.Get("live") != "0"

	// Links keep the current filter and only flip live following.
	link := func(live bool) string {
		v := url.Values{"tail": {strconv.Itoa(tail)}}
		if q != "" {
			v.Set("q", q)
		}
		if level != "" {
			v.Set("level", level)
		}
		if !live {
			v.Set("live", "0")
		}
		return "/containers/" + name + "/logs?" + v.Encode()
	}

	lines, err := s.agent.Logs(r.Context(), name, tail)
	shown := filterLogs(lines, q, level)
	data := map[string]any{"Title": name + " logları", "Name": name, "Tail": tail, "Tails": logTails, "Q": q, "Level": level,
		"Lines": shown, "Total": len(lines), "Filtered": q != "" || level != "", "Err": errString(err),
		"LiveOn": live, "PauseURL": link(false), "LiveURL": link(true)}
	if live {
		data["Live"] = 5
	}
	s.render(w, r, "logs", data)
}
