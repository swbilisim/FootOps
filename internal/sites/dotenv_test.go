package sites

import (
	"strings"
	"testing"
)

func TestParseDotenv(t *testing.T) {
	text := "\ufeff# yorum\r\n" +
		"export NODE_ENV=production\r\n" +
		"SPACED = value with spaces   \n" +
		"HASH=p@ss#1\n" +
		"COMMENTED=abc # yorum\n" +
		"ONLY_COMMENT= # yorum\n" +
		"EMPTY=\n" +
		"SINGLE='lit $HOME \\n # not a comment'\n" +
		"DOUBLE=\"a\\nb\\t\\\"c\\\" \\\\ \\x\" # sonra yorum\n" +
		"MULTI=\"-----BEGIN KEY-----\n" +
		"abc\n" +
		"-----END KEY-----\"\n" +
		"  INDENTED=1\n" +
		"EQ=a=b=c\n" +
		"DOLLAR=$5 and ${X}\n"
	got, err := ParseDotenv(text)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"NODE_ENV":     "production",
		"SPACED":       "value with spaces",
		"HASH":         "p@ss#1",
		"COMMENTED":    "abc",
		"ONLY_COMMENT": "",
		"EMPTY":        "",
		"SINGLE":       `lit $HOME \n # not a comment`,
		"DOUBLE":       "a\nb\t\"c\" \\ \\x",
		"MULTI":        "-----BEGIN KEY-----\nabc\n-----END KEY-----",
		"INDENTED":     "1",
		"EQ":           "a=b=c",
		"DOLLAR":       "$5 and ${X}",
	}
	m := EnvMap(got)
	if len(m) != len(want) {
		t.Fatalf("got %d vars: %v", len(m), m)
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	lines := map[string]int{}
	for _, e := range got {
		lines[e.Key] = e.Line
	}
	if lines["NODE_ENV"] != 2 || lines["MULTI"] != 10 || lines["INDENTED"] != 13 {
		t.Errorf("lines = %v", lines)
	}
	for _, e := range got {
		if (e.Key == "COMMENTED" || e.Key == "ONLY_COMMENT") != e.Comment {
			t.Errorf("%s: comment flag %v", e.Key, e.Comment)
		}
		if (e.Key == "DOUBLE") != e.Escaped {
			t.Errorf("%s: escaped flag %v", e.Key, e.Escaped)
		}
	}
	// Only escapes that changed something count: an unknown one stays as written.
	if es, _ := ParseDotenv(`A="C:\x"` + "\n" + `B="a\tb"`); es[0].Escaped || !es[1].Escaped {
		t.Errorf("escaped flags: %+v", es)
	}
	rows := DescribeEnv([]EnvEntry{{Key: "B", Value: "a\tb", Quote: '"', Escaped: true}}, validSpec())
	if len(rows[0].Notes) != 1 || !strings.Contains(rows[0].Notes[0], "kaçış") {
		t.Errorf("escape note: %v", rows[0].Notes)
	}
}

func TestParseDotenvErrors(t *testing.T) {
	for name, text := range map[string]string{
		"no equals":      "A=1\njust text",
		"duplicate":      "A=1\n\nA=2",
		"bad key":        "1A=x",
		"dashed key":     "MY-KEY=x",
		"unclosed":       "A=\"abc\nB=2",
		"unclosed '":     "A='abc",
		"trailing text":  "A=\"abc\"def",
		"empty key":      "=x",
		"not utf-8":      "A=\xff",
		"export only":    "export",
		"too big":        "A=" + strings.Repeat("x", maxDotenv),
		"quote in quote": "A='it\\'s'",
	} {
		if _, err := ParseDotenv(text); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err := ParseDotenv("A=1\nB=2\nA=3")
	if err == nil || !strings.Contains(err.Error(), "satır 3") || !strings.Contains(err.Error(), "satır 1") {
		t.Errorf("duplicate error = %v", err)
	}
}

// Whatever the input, the parser neither panics nor returns a key a site would refuse.
func FuzzParseDotenv(f *testing.F) {
	for _, s := range []string{"A=1", "export B = 'x'", "C=\"a\\\nb\"", "D=x # y", "E=\"", "#\n\nF=\"\"\"", "G='\n'"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := ParseDotenv(s)
		if err != nil {
			return
		}
		seen := map[string]bool{}
		for _, e := range got {
			if !reEnvKey.MatchString(e.Key) || seen[e.Key] {
				t.Fatalf("key %q", e.Key)
			}
			seen[e.Key] = true
		}
	})
}

func TestDescribeEnv(t *testing.T) {
	s := validSpec() // port 3000, database in MONGODB_URI
	entries, err := ParseDotenv(`NODE_ENV=development
PORT=3001
HOSTNAME=localhost
NEXTAUTH_URL=http://localhost:3000
NEXT_PUBLIC_API=https://api.example.com
MONGO_URL=mongodb://localhost:27017/app
SECRET=abc # yorum
KEY="  padded"
PEM="a
b"
EMPTY=
FINE=ok
`)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]EnvRow{}
	for _, r := range DescribeEnv(entries, s) {
		rows[r.Key] = r
	}
	warn := map[string]bool{"NODE_ENV": true, "PORT": true, "HOSTNAME": true, "NEXTAUTH_URL": true, "MONGO_URL": true}
	for k, r := range rows {
		if r.Warn != warn[k] {
			t.Errorf("%s: warn %v, notes %v", k, r.Warn, r.Notes)
		}
	}
	for k, n := range map[string]int{"NEXT_PUBLIC_API": 1, "SECRET": 1, "KEY": 1, "PEM": 1, "EMPTY": 1, "FINE": 0} {
		if len(rows[k].Notes) != n {
			t.Errorf("%s: notes %v", k, rows[k].Notes)
		}
	}
	if rows["PEM"].Lines != 2 || rows["SECRET"].Chars != 3 {
		t.Errorf("PEM %+v SECRET %+v", rows["PEM"], rows["SECRET"])
	}
	// Without a database, a MongoDB address is someone else's database: a note, no warning.
	s.Database, s.DBVar = false, ""
	for _, r := range DescribeEnv([]EnvEntry{{Key: "MONGO_URL", Value: "mongodb+srv://u:p@x.mongodb.net/db"}}, s) {
		if r.Warn || len(r.Notes) != 1 {
			t.Errorf("external db: %+v", r)
		}
	}
	// Notes never carry the value.
	for _, r := range DescribeEnv([]EnvEntry{{Key: "NODE_ENV", Value: "s3cr3t-dev"}, {Key: "X", Value: "http://localhost/s3cr3t"}}, validSpec()) {
		for _, n := range r.Notes {
			if strings.Contains(n, "s3cr3t") {
				t.Errorf("note leaks the value: %q", n)
			}
		}
	}
}
