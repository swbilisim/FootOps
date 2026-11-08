package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Mac helper (mac/footops-yedek) makes the recovery line the installer reads: run it
// with a stand-in for macOS's security(1) holding made-up values, parse what it prints.
func TestMacHelperRecoveryLine(t *testing.T) {
	for _, tool := range []string{"bash", "python3", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " yok")
		}
	}
	shim := t.TempDir()
	security := `#!/bin/bash
case "$1" in
find-generic-password)
	acct=""
	while [ $# -gt 0 ]; do [ "$1" = "-a" ] && acct=$2; shift; done
	case "$acct" in
	r2-hesap) echo 0123456789abcdef0123456789abcdef ;;
	r2-mac-erisim) echo FAKEKEYID99 ;;
	r2-mac-gizli) echo Fake/Secret+Key= ;;
	restic-t1) echo FakeRepoPassword0123456789abcdefghijklmn ;;
	kova-t1) echo footops-t1/kasa ;;
	*) exit 44 ;;
	esac ;;
dump-keychain)
	printf 'keychain: "x"\n    "acct"<blob>="restic-t1"\n    "svce"<blob>="footops-yedek"\n'
	printf 'keychain: "y"\n    "acct"<blob>="restic-zz"\n    "svce"<blob>="baska"\n' ;;
esac
`
	if err := os.WriteFile(filepath.Join(shim, "security"), []byte(security), 0o755); err != nil {
		t.Fatal(err)
	}
	helper, _ := filepath.Abs("../../mac/footops-yedek")
	run := func(args ...string) (string, string, error) {
		cmd := exec.Command("bash", append([]string{helper}, args...)...)
		cmd.Env = append(os.Environ(), "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		return out.String(), errb.String(), err
	}
	out, stderr, err := run("kurtarma", "T1")
	if err != nil {
		t.Fatalf("%v: %s", err, stderr)
	}
	r, err := ParseRecoveryInfo(out)
	if err != nil {
		t.Fatalf("line %q: %v", out, err)
	}
	want := RecoverAnswers{Repository: "s3:https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com/footops-t1/kasa",
		KeyID: "FAKEKEYID99", SecretKey: "Fake/Secret+Key=", Password: "FakeRepoPassword0123456789abcdefghijklmn"}
	if r != want {
		t.Errorf("got %+v", r)
	}
	if strings.Contains(stderr, "FakeRepoPassword") || strings.Contains(stderr, "Fake/Secret") {
		t.Error("a secret on stderr")
	}
	out, _, err = run("sunucular")
	if err != nil || !strings.Contains(out, "t1       footops-t1/kasa") || strings.Contains(out, "zz") {
		t.Errorf("servers: %v\n%s", err, out)
	}
	if _, stderr, err := run("kurtarma", "yok"); err == nil || !strings.Contains(stderr, "restic-yok yok") {
		t.Errorf("unknown server: %v %s", err, stderr)
	}
}
