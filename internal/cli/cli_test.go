package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunExitCodes(t *testing.T) {
	cases := []struct {
		args []string
		code int
	}{
		{nil, 2},
		{[]string{"version"}, 0},
		{[]string{"help"}, 0},
		{[]string{"install", "-h"}, 0},
		{[]string{"nope"}, 2},
		{[]string{"web"}, 1},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		if got := Run(c.args, &out, &errOut); got != c.code {
			t.Errorf("Run(%v) = %d, want %d (stderr: %s)", c.args, got, c.code, errOut.String())
		}
	}
}

func TestVersionOutput(t *testing.T) {
	var out bytes.Buffer
	Run([]string{"version"}, &out, &out)
	if !strings.HasPrefix(out.String(), "footops ") {
		t.Errorf("version output = %q", out.String())
	}
}
