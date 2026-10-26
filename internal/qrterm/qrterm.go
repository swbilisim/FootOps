// Package qrterm renders QR codes for a terminal.
package qrterm

import (
	"strings"

	"rsc.io/qr"
)

const quiet = 2 // quiet-zone modules around the code

// Render returns text as a QR code drawn with half-block characters, two modules per
// character row. Colours are set explicitly (black on white) so the code scans on
// dark-themed terminals too.
func Render(text string) (string, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	n := code.Size
	black := func(x, y int) bool {
		if x < 0 || y < 0 || x >= n || y >= n {
			return false
		}
		return code.Black(x, y)
	}
	var b strings.Builder
	for y := -quiet; y < n+quiet; y += 2 {
		b.WriteString("\x1b[30;47m") // black foreground, white background
		for x := -quiet; x < n+quiet; x++ {
			top, bottom := black(x, y), black(x, y+1)
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\x1b[0m\n")
	}
	return b.String(), nil
}
