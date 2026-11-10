// Command relsign signs the integration test's releases (u3) with a key kept in the
// test's work folder — a key of its own, which only the test's builds trust
// (-ldflags -X …/release.testKey). `relsign pub <seed file>` makes the key if missing
// and prints its public half; `relsign sign <seed file> <dir> <version> [notes]`
// writes footops-release.json and its signature for the footops_linux_* in dir.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/swbilisim/FootOps/internal/release"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "relsign:", err)
		os.Exit(1)
	}
}

func key(seedFile string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(seedFile)
	if os.IsNotExist(err) {
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		if err := os.WriteFile(seedFile, []byte(base64.StdEncoding.EncodeToString(seed)), 0o600); err != nil {
			return nil, err
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s bozuk", seedFile)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func run(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("kullanım: relsign pub <seed> | sign <seed> <dir> <version> [notes]")
	}
	priv, err := key(args[1])
	if err != nil {
		return err
	}
	switch {
	case args[0] == "pub" && len(args) == 2:
		fmt.Println(base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)))
		return nil
	case args[0] == "sign" && (len(args) == 4 || len(args) == 5):
		dir, ver := args[2], args[3]
		m := release.Manifest{Format: release.Format, Version: ver, Commit: "it", Date: "2026-11-10T00:00:00Z", Files: map[string]release.File{}}
		if len(args) == 5 {
			m.Notes = args[4]
		}
		names, _ := filepath.Glob(filepath.Join(dir, "footops_linux_*"))
		for _, p := range names {
			f, err := release.FileOf(p)
			if err != nil {
				return err
			}
			m.Files[filepath.Base(p)] = f
		}
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		b = append(b, '\n')
		if err := os.WriteFile(filepath.Join(dir, release.ManifestName), b, 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, release.SigName), release.Sign(priv, b), 0o644)
	}
	return fmt.Errorf("bilinmeyen: %v", args)
}
