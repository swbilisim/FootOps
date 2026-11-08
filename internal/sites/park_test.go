package sites

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestValidExpiry(t *testing.T) {
	for _, ok := range []string{"", "2026-12-31", "2020-01-01", "2100-12-31", "2028-02-29"} {
		if err := ValidExpiry(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"2026-13-01", "31.12.2026", "2026-02-30", "2019-12-31", "2101-01-01", "2026-1-5", " 2026-12-31"} {
		if ValidExpiry(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	s := validSpec()
	s.Expires = " 2026-12-31 "
	s.Normalize()
	if s.Expires != "2026-12-31" || s.Validate(nil) != nil {
		t.Errorf("normalized end date %q refused", s.Expires)
	}
	s.Expires = "yarın"
	var ve *ValidationError
	if err := s.Validate(nil); !errors.As(err, &ve) || !strings.Contains(err.Error(), "bitiş tarihi") {
		t.Errorf("bad end date: %v", err)
	}
}

func TestDaysLeft(t *testing.T) {
	ist, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Skip(err)
	}
	now := time.Date(2026, 11, 9, 23, 30, 0, 0, ist) // late in the day: still the 9th
	for exp, want := range map[string]int{"2026-11-16": 7, "2026-11-12": 3, "2026-11-10": 1, "2026-11-09": 0, "2026-11-07": -2, "2027-11-09": 365} {
		if got, ok := DaysLeft(exp, now); !ok || got != want {
			t.Errorf("%s: %d %v, want %d", exp, got, ok, want)
		}
	}
	for _, none := range []string{"", "yarın"} {
		if _, ok := DaysLeft(none, now); ok {
			t.Errorf("%q has an end", none)
		}
	}
	// Calendar days, not 24-hour spans, across a clock change.
	if ber, err := time.LoadLocation("Europe/Berlin"); err == nil {
		if got, _ := DaysLeft("2026-03-30", time.Date(2026, 3, 28, 23, 30, 0, 0, ber)); got != 2 {
			t.Errorf("across the clock change: %d", got)
		}
	}
}

func TestExtend(t *testing.T) {
	now := time.Date(2026, 11, 9, 15, 0, 0, 0, time.Local)
	for _, c := range []struct {
		from   string
		months int
		want   string
	}{
		{"2026-12-31", 1, "2027-01-31"},  // not passed: from the end date
		{"2027-01-31", 1, "2027-02-28"},  // the month has no 31st: its last day
		{"2028-01-31", 1, "2028-02-29"},  // a leap year
		{"2028-02-29", 12, "2029-02-28"}, // a year on from a leap day
		{"2026-11-09", 1, "2026-12-09"},  // the day itself: the same either way
		{"2026-11-01", 1, "2026-12-09"},  // passed: from today
		{"2026-11-01", 12, "2027-11-09"},
		{"", 12, "2027-11-09"}, // none: from today
		{"2026-12-31", 12, "2027-12-31"},
	} {
		if got := Extend(c.from, c.months, now); got != c.want {
			t.Errorf("Extend(%q, %d) = %s, want %s", c.from, c.months, got, c.want)
		}
	}
}

func TestParkBlock(t *testing.T) {
	s := validSpec()
	s.Domains = []string{"enisparta.com", "www.enisparta.com", "ozel.com"}
	a, b := Cert{ID: "aaaaaaaaaaaaaaaa"}, Cert{ID: "bbbbbbbbbbbbbbbb"}
	certFor := map[string]Cert{"enisparta.com": a, "www.enisparta.com": a, "ozel.com": b}
	block := ParkBlock(s, certFor)
	for _, want := range []string{
		"# Managed by FootOps: site enisparta (musteri), parked\n",
		"https://enisparta.com, https://www.enisparta.com {\n\ttls /etc/caddy/certs/aaaaaaaaaaaaaaaa.crt /etc/caddy/certs/aaaaaaaaaaaaaaaa.key\n",
		"https://ozel.com {\n\ttls /etc/caddy/certs/bbbbbbbbbbbbbbbb.crt /etc/caddy/certs/bbbbbbbbbbbbbbbb.key\n",
		"\t\tRetry-After 3600\n", "\t\tCache-Control no-store\n", `Content-Type "text/html; charset=utf-8"`,
		"\trespond `<!doctype html>", "</html>\n` 503\n}\n",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("park block misses %q:\n%s", want, block)
		}
	}
	if strings.Count(block, "respond `") != 2 || strings.Contains(block, "reverse_proxy") {
		t.Errorf("one page per certificate, no app:\n%s", block)
	}
	// The page goes into the Caddyfile inside backticks: nothing in it may end the token,
	// or read as a placeholder ({…}) or the environment ({$…}).
	for _, c := range []string{"`", "{", "}", "$"} {
		if strings.Contains(ParkPage, c) {
			t.Errorf("park page holds %q", c)
		}
	}
	for _, want := range []string{"<title>Bakım çalışması</title>", "Bu site şu anda bakımda. Kısa süre içinde yeniden yayında olacak.", `lang="tr"`, "viewport"} {
		if !strings.Contains(ParkPage, want) {
			t.Errorf("park page misses %q", want)
		}
	}
	// Nothing on it names the server or the reason.
	for _, not := range []string{"FootOps", "Caddy", "ödeme", "fatura", "süresi", "park"} {
		if strings.Contains(strings.ToLower(ParkPage), strings.ToLower(not)) {
			t.Errorf("park page says %q", not)
		}
	}
	if SiteBlock(s, certFor) != CaddyBlock(s, certFor) {
		t.Error("a site in service got another block")
	}
	s.Parked = &ParkState{At: time.Now()}
	if SiteBlock(s, certFor) != block {
		t.Error("a parked site did not get its park block")
	}
}
