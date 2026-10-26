package install

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Prompter asks questions on a terminal.
type Prompter struct {
	in  *bufio.Reader
	out io.Writer
	// readSecret reads a line without echoing it. Nil means secrets are read like normal lines (tests).
	readSecret func() (string, error)
}

func NewPrompter(in io.Reader, out io.Writer, readSecret func() (string, error)) *Prompter {
	return &Prompter{in: bufio.NewReader(in), out: out, readSecret: readSecret}
}

// ErrInputClosed is returned when stdin ends before an answer is given.
var ErrInputClosed = errors.New("girdi kapandı")

// readRaw returns the next line without its line ending. A final line without a
// newline is returned normally; EOF before any input is ErrInputClosed.
func (p *Prompter) readRaw() (string, error) {
	line, err := p.in.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		if errors.Is(err, io.EOF) {
			return "", ErrInputClosed
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (p *Prompter) readLine() (string, error) {
	line, err := p.readRaw()
	return strings.TrimSpace(line), err
}

// Say prints a line.
func (p *Prompter) Say(format string, args ...any) {
	fmt.Fprintf(p.out, format+"\n", args...)
}

// Ask prompts until validate accepts the answer. An empty answer selects def.
func (p *Prompter) Ask(label, def string, validate func(string) error) (string, error) {
	for {
		if def != "" {
			fmt.Fprintf(p.out, "%s [%s]: ", label, def)
		} else {
			fmt.Fprintf(p.out, "%s: ", label)
		}
		ans, err := p.readLine()
		if err != nil {
			return "", err
		}
		if ans == "" {
			ans = def
		}
		if validate != nil {
			if err := validate(ans); err != nil {
				p.Say("  ✗ %v", err)
				continue
			}
		}
		return ans, nil
	}
}

// YesNo asks a yes/no question. Accepts Turkish and English answers.
func (p *Prompter) YesNo(label string, def bool) (bool, error) {
	hint := "E/h"
	if !def {
		hint = "e/H"
	}
	for {
		fmt.Fprintf(p.out, "%s [%s]: ", label, hint)
		ans, err := p.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(ans) {
		case "":
			return def, nil
		case "e", "evet", "y", "yes":
			return true, nil
		case "h", "hayır", "hayir", "n", "no":
			return false, nil
		}
		p.Say("  ✗ 'e' veya 'h' yaz")
	}
}

// Lines reads lines until an empty line. Returns nil if the first line is empty.
func (p *Prompter) Lines(label string) ([]string, error) {
	p.Say("%s (bitirmek için boş satır):", label)
	var lines []string
	for {
		l, err := p.readLine()
		if err != nil {
			return nil, err
		}
		if l == "" {
			return lines, nil
		}
		lines = append(lines, l)
	}
}

// NewPassword asks for a secret twice without echo and validates it.
func (p *Prompter) NewPassword(label string, validate func(string) error) (string, error) {
	for {
		fmt.Fprintf(p.out, "%s: ", label)
		a, err := p.secret()
		if err != nil {
			return "", err
		}
		if err := validate(a); err != nil {
			p.Say("  ✗ %v", err)
			continue
		}
		fmt.Fprint(p.out, "Tekrar: ")
		b, err := p.secret()
		if err != nil {
			return "", err
		}
		if a != b {
			p.Say("  ✗ Şifreler aynı değil")
			continue
		}
		return a, nil
	}
}

// Secret asks for a single secret value without echo (e.g. an API token).
func (p *Prompter) Secret(label string, validate func(string) error) (string, error) {
	for {
		fmt.Fprintf(p.out, "%s: ", label)
		v, err := p.secret()
		if err != nil {
			return "", err
		}
		v = strings.TrimSpace(v) // tokens never contain spaces; pasting often adds them
		if err := validate(v); err != nil {
			p.Say("  ✗ %v", err)
			continue
		}
		return v, nil
	}
}

// secret reads a password. Surrounding spaces are kept: they are part of the password.
func (p *Prompter) secret() (string, error) {
	if p.readSecret == nil {
		return p.readRaw()
	}
	s, err := p.readSecret()
	fmt.Fprintln(p.out)
	return strings.TrimRight(s, "\r\n"), err
}
