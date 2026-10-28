package sites

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// EnvEntry is one variable read from a .env text.
type EnvEntry struct {
	Key   string
	Value string
	Line  int  // where the variable starts (1-based)
	Quote byte // the quote the value was written in: 0, '\'' or '"'
	// Comment is set when an unquoted value lost a trailing " # ..." comment.
	Comment bool
	// Escaped is set when a double-quoted value had backslash escapes turned into
	// characters (\n, \t, \r, \", \\).
	Escaped bool
}

// maxDotenv bounds a pasted or loaded .env text; the form itself is limited to 1 MB.
const maxDotenv = 256 << 10

// ParseDotenv reads a .env file:
//
//	# comment                 export KEY=value          KEY = value
//	KEY='literal $ \n'        KEY="escapes \n \t \" \\ and
//	                          more lines"               KEY=value # comment
//
// Ordinary files give what docker compose and Node's dotenv give, but no tool agrees on
// every corner and this parser picks the safe side where they differ: nothing is
// interpolated ("$" is always literal; compose would expand $VAR); an unquoted value
// loses a comment only where "#" follows whitespace ("p@ss#1" keeps its "#"; dotenv would
// cut it), and is trimmed; in double quotes \n \t \r \" \\ become characters (dotenv
// turns only \n and \r), other escapes stay as written; single quotes keep everything.
// A key that appears twice is an error, because the tools disagree on which one wins.
// The review page says where a value was cut at a comment or had escapes turned.
func ParseDotenv(text string) ([]EnvEntry, error) {
	if len(text) > maxDotenv {
		return nil, fmt.Errorf("en fazla %d KB", maxDotenv>>10)
	}
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("metin UTF-8 değil")
	}
	text = strings.TrimPrefix(text, "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	var out []EnvEntry
	seen := map[string]int{}
	for i := 0; i < len(lines); i++ {
		line := strings.TrimLeft(lines[i], " \t")
		if line == "" || line[0] == '#' {
			continue
		}
		start := i + 1
		if rest, ok := strings.CutPrefix(line, "export"); ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			line = strings.TrimLeft(rest, " \t")
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimRight(k, " \t")
		if !ok || k == "" {
			return nil, fmt.Errorf("satır %d: KEY=DEĞER biçiminde olmalı", start)
		}
		if !reEnvKey.MatchString(k) {
			return nil, fmt.Errorf("satır %d: %q geçerli bir değişken adı değil (harf, rakam, alt çizgi; rakamla başlamaz)", start, k)
		}
		if prev, dup := seen[k]; dup {
			return nil, fmt.Errorf("satır %d: %s satır %d'de de var", start, k, prev)
		}
		e := EnvEntry{Key: k, Line: start}
		lead := len(v) - len(strings.TrimLeft(v, " \t"))
		v = v[lead:]
		switch {
		case v != "" && (v[0] == '\'' || v[0] == '"'):
			e.Quote = v[0]
			val, rest, next, err := quoted(v, lines, i, &e.Escaped)
			if err != nil {
				return nil, fmt.Errorf("satır %d: %s: %w", start, k, err)
			}
			rest = strings.TrimLeft(rest, " \t")
			if rest != "" && rest[0] != '#' {
				return nil, fmt.Errorf("satır %d: %s: kapanan tırnaktan sonra beklenmeyen metin", next+1, k)
			}
			e.Value, i = val, next
		default:
			// A comment needs whitespace before "#": also between "=" and the value.
			cut := -1
			if lead > 0 && strings.HasPrefix(v, "#") {
				cut = 0
			} else if j := commentStart(v); j >= 0 {
				cut = j
			}
			if cut >= 0 {
				v, e.Comment = v[:cut], true
			}
			e.Value = strings.TrimRight(v, " \t")
		}
		seen[k] = start
		out = append(out, e)
	}
	return out, nil
}

// commentStart finds " #" or "\t#" in an unquoted value.
func commentStart(v string) int {
	for j := 1; j < len(v); j++ {
		if v[j] == '#' && (v[j-1] == ' ' || v[j-1] == '\t') {
			return j - 1
		}
	}
	return -1
}

// quoted reads a quoted value that starts at v[0] on line i and may run over the
// following lines. It returns the value, the text after the closing quote and the line
// the value ended on.
func quoted(v string, lines []string, i int, escaped *bool) (string, string, int, error) {
	q := v[0]
	var b strings.Builder
	s := v[1:]
	for {
		for j := 0; j < len(s); j++ {
			c := s[j]
			switch {
			case c == q:
				return b.String(), s[j+1:], i, nil
			case c == '\\' && q == '"' && j+1 < len(s):
				j++
				switch s[j] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case 'r':
					b.WriteByte('\r')
				case '"', '\\':
					b.WriteByte(s[j])
				default: // unknown escapes stay as written, like dotenv
					b.WriteByte('\\')
					b.WriteByte(s[j])
					continue
				}
				*escaped = true
			default:
				b.WriteByte(c)
			}
		}
		if i+1 >= len(lines) {
			return "", "", i, fmt.Errorf("tırnak kapanmamış (%c)", q)
		}
		i++
		b.WriteByte('\n')
		s = lines[i]
	}
}

// EnvMap turns entries into the map a site takes.
func EnvMap(entries []EnvEntry) map[string]string {
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		m[e.Key] = e.Value
	}
	return m
}

// DBVars are the names apps commonly read a MongoDB connection string from.
var DBVars = []string{"MONGODB_URI", "DB_URI", "MONGO_URL", "MONGO_URI", "DATABASE_URL"}

// EnvRow describes one variable for a review page: never its value, only its length
// and what may be wrong with it.
type EnvRow struct {
	Key   string
	Chars int
	Lines int
	Notes []string
	Warn  bool // a note that probably breaks the site
}

var (
	reLocal       = regexp.MustCompile(`(?i)(^|[^a-z0-9.-])(localhost|127\.0\.0\.1)([^a-z0-9.-]|$)`)
	buildPrefixes = []string{"NEXT_PUBLIC_", "VITE_", "REACT_APP_", "NUXT_PUBLIC_", "PUBLIC_", "EXPO_PUBLIC_"}
)

// DescribeEnv reviews the variables a site is about to get.
func DescribeEnv(entries []EnvEntry, s Spec) []EnvRow {
	rows := make([]EnvRow, 0, len(entries))
	for _, e := range entries {
		r := EnvRow{Key: e.Key, Chars: utf8.RuneCountInString(e.Value), Lines: strings.Count(e.Value, "\n") + 1}
		note := func(warn bool, format string, args ...any) {
			r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
			r.Warn = r.Warn || warn
		}
		v := strings.TrimSpace(e.Value)
		switch {
		case e.Value == "":
			note(false, "boş")
		case v != e.Value && e.Quote != 0:
			note(false, "başında ya da sonunda boşluk var")
		}
		if e.Comment {
			note(false, "satır sonundaki “ #…” yorum sayıldı, alınmadı; değerin parçasıysa tırnak içine yaz")
		}
		if e.Escaped {
			note(false, "çift tırnaktaki \\n, \\t gibi kaçışlar karaktere çevrildi; olduğu gibi kalsın istiyorsan tek tırnak kullan")
		}
		if r.Lines > 1 {
			note(false, "%d satır", r.Lines)
		}
		for _, p := range buildPrefixes {
			if strings.HasPrefix(e.Key, p) {
				note(false, "derlemede koda gömülür: tarayıcıdaki kod imaj derlenirken aldığı değeri kullanır, burada vermek onu değiştirmez")
				break
			}
		}
		switch e.Key {
		case "PORT":
			if v != strconv.Itoa(s.Port) {
				note(true, "uygulama bu portu dinleyebilir, formdaki uygulama portu %d", s.Port)
			}
		case "HOSTNAME":
			if v == "localhost" || v == "127.0.0.1" || v == "::1" {
				note(true, "uygulama yalnız container'ın içinden erişilebilir dinler, Caddy ulaşamaz; 0.0.0.0 yaz ya da hiç verme")
			}
		case "NODE_ENV":
			if v != "production" {
				note(true, "production değil: geliştirme modu yavaş çalışır ve hataları ayrıntılı gösterir")
			}
		}
		if e.Key != "HOSTNAME" && reLocal.MatchString(e.Value) {
			note(true, "localhost içeriyor: container'da bu, uygulamanın kendisi demek (bilgisayarındaki servis değil)")
		}
		if s.Database {
			for _, d := range DBVars {
				if e.Key == d && d != s.DBVar {
					note(true, "panel veritabanı adresini %s değişkeninde verecek; uygulama %s okuyorsa bu değer (muhtemelen yereldeki veritabanı) kullanılır. Gerekirse veritabanı değişkenini %s yap", s.DBVar, d, d)
				}
			}
		} else if strings.HasPrefix(v, "mongodb://") || strings.HasPrefix(v, "mongodb+srv://") {
			note(false, "dışarıdaki bir veritabanının adresi (panel veritabanı oluşturmayacak)")
		}
		rows = append(rows, r)
	}
	return rows
}
