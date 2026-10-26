package qrterm

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRender(t *testing.T) {
	out, err := Render("otpauth://totp/FootOps:furkan?secret=JBSWY3DPEHPK3PXP&issuer=FootOps")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) < 10 {
		t.Fatalf("too few lines: %d", len(lines))
	}
	width := -1
	for _, l := range lines {
		if !strings.HasPrefix(l, "\x1b[30;47m") || !strings.HasSuffix(l, "\x1b[0m") {
			t.Fatalf("line without explicit colours: %q", l)
		}
		body := strings.TrimSuffix(strings.TrimPrefix(l, "\x1b[30;47m"), "\x1b[0m")
		w := utf8.RuneCountInString(body)
		if width == -1 {
			width = w
		} else if w != width {
			t.Fatalf("ragged QR: %d vs %d", w, width)
		}
	}
}
