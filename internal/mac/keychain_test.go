package mac

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A stand-in for macOS's security(1) with made-up items: what it prints and how it
// fails, as the real one does.
const fakeSecurity = `#!/bin/sh
case "$1" in
find-generic-password)
	svc="" acct=""
	while [ $# -gt 0 ]; do
		[ "$1" = "-s" ] && svc=$2
		[ "$1" = "-a" ] && acct=$2
		shift
	done
	[ "$svc" = footops-yedek ] || { echo "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain." >&2; exit 44; }
	case "$acct" in
	r2-hesap) echo 0123456789abcdef0123456789abcdef ;;
	kilitli) echo "security: SecKeychainItemCopyContent: User interaction is not allowed." >&2; exit 36 ;;
	*) echo "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain." >&2; exit 44 ;;
	esac ;;
dump-keychain)
	cat <<'EOF'
keychain: "/Users/x/Library/Keychains/login.keychain-db"
version: 512
class: "genp"
attributes:
    0x00000007 <blob>="footops-yedek"
    "acct"<blob>="restic-h1"
    "svce"<blob>="footops-yedek"
keychain: "/Users/x/Library/Keychains/login.keychain-db"
class: "genp"
attributes:
    "acct"<blob>="restic-zz"
    "svce"<blob>="baska-hizmet"
keychain: "/Users/x/Library/Keychains/login.keychain-db"
class: "genp"
attributes:
    "acct"<blob>=0x00FF  "\000\377"
    "svce"<blob>="footops-yedek"
keychain: "/Users/x/Library/Keychains/login.keychain-db"
class: "genp"
attributes:
    "svce"<blob>="footops-yedek"
    "acct"<blob>="r2-hesap"
keychain: "/Users/x/Library/Keychains/login.keychain-db"
class: "inet"
attributes:
    "acct"<blob>="restic-a"
    "svce"<blob>="footops-yedek"
EOF
	;;
esac
`

func TestSecurityKeychain(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh yok")
	}
	tool := filepath.Join(t.TempDir(), "security")
	if err := os.WriteFile(tool, []byte(fakeSecurity), 0o755); err != nil {
		t.Fatal(err)
	}
	k := securityKeychain{Tool: tool, Service: "footops-yedek"}
	ctx := context.Background()
	if v, err := k.Get(ctx, "r2-hesap"); err != nil || v != "0123456789abcdef0123456789abcdef" {
		t.Errorf("get: %q %v", v, err)
	}
	if _, err := k.Get(ctx, "restic-yok"); !errors.Is(err, errNoItem) || !strings.Contains(err.Error(), "footops-yedek / restic-yok") {
		t.Errorf("missing: %v", err)
	}
	// Another failure (a locked Keychain) is not taken for a missing item.
	if _, err := k.Get(ctx, "kilitli"); err == nil || errors.Is(err, errNoItem) || !strings.Contains(err.Error(), "User interaction is not allowed") {
		t.Errorf("locked: %v", err)
	}
	// The service's generic passwords only — not another service's, not one in hex, not
	// an internet password.
	names, err := k.Names(ctx)
	if want := []string{"r2-hesap", "restic-h1"}; err != nil || !reflect.DeepEqual(names, want) {
		t.Errorf("names: %v %v", names, err)
	}
	if _, err := (securityKeychain{Tool: filepath.Join(t.TempDir(), "yok"), Service: "footops-yedek"}).Names(ctx); err == nil {
		t.Error("no tool, no error")
	}
}
