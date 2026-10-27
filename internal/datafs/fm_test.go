//go:build linux || darwin

package datafs

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/swbilisim/FootOps/internal/archive"
)

// fmEnv is a site's data directory and trash, as the helper sees them.
type fmEnv struct {
	data, trash   *os.Root
	dataDir, tDir string
}

func newFM(t *testing.T) *fmEnv {
	t.Helper()
	base := t.TempDir()
	e := &fmEnv{dataDir: filepath.Join(base, "data"), tDir: filepath.Join(base, "trash")}
	for _, d := range []string{e.dataDir, e.tDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	if e.data, err = os.OpenRoot(e.dataDir); err != nil {
		t.Fatal(err)
	}
	if e.trash, err = os.OpenRoot(e.tDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.data.Close(); e.trash.Close() })
	return e
}

// write creates files ("name/" is a folder) under the data directory.
func (e *fmEnv) write(t *testing.T, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(e.dataDir, name)
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *fmEnv) files(t *testing.T) map[string]string {
	t.Helper()
	files, _ := tree(t, e.dataDir)
	return files
}

var stamp = Stamp{Actor: "furkan", Now: time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)}

func names(l Listing) []string {
	var out []string
	for _, e := range l.Entries {
		out = append(out, e.Name)
	}
	return out
}

func TestListSortsAndPages(t *testing.T) {
	e := newFM(t)
	e.write(t, map[string]string{"b.txt": "bb", "a.txt": "a", "Çanta.jpg": "çççç", "zeytin.png": "z", "ılık.txt": "ı",
		"İzmir.txt": "i", "cami.txt": "c", "klasör/": "", "Ana/": "", "klasör/iç.txt": "x", ".gizli": "h"})
	if err := os.Symlink("klasör", filepath.Join(e.dataDir, "bağ")); err != nil {
		t.Fatal(err)
	}
	l, err := List(e.data, "", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Ana", "klasör", "a.txt", "b.txt", "bağ", "cami.txt", "Çanta.jpg", "ılık.txt", "İzmir.txt", "zeytin.png"}
	if got := names(l); !reflect.DeepEqual(got, want) {
		t.Errorf("order = %q", got)
	}
	if l.Total != 10 || l.Dirs != 2 || l.Files != 7 || l.Bytes != 2+1+8+1+2+1+1 || l.Hidden != 1 {
		t.Errorf("listing = %+v", l)
	}
	for _, en := range l.Entries {
		if en.Name == "bağ" && en.Type != "link" || en.Name == "klasör" && en.Type != "dir" || en.Name == "b.txt" && (en.Type != "file" || en.Size != 2) {
			t.Errorf("entry %+v", en)
		}
	}
	page, _ := List(e.data, "", ListOptions{Offset: 8, Limit: 5})
	if got := names(page); !reflect.DeepEqual(got, want[8:]) || page.Offset != 8 || page.Total != 10 {
		t.Errorf("page = %q %+v", got, page)
	}
	bySize, _ := List(e.data, "", ListOptions{Sort: "size"})
	if got := names(bySize); got[0] != "Ana" || got[2] != "Çanta.jpg" {
		t.Errorf("by size = %q", got)
	}
	dirs, _ := List(e.data, "", ListOptions{DirsOnly: true})
	if got := names(dirs); !reflect.DeepEqual(got, []string{"Ana", "klasör"}) {
		t.Errorf("dirs only = %q", got)
	}
	sub, err := List(e.data, "klasör", ListOptions{})
	if err != nil || !reflect.DeepEqual(names(sub), []string{"iç.txt"}) || sub.Dir != "klasör" {
		t.Errorf("sub = %+v %v", sub, err)
	}
}

func TestListOddNames(t *testing.T) {
	e := newFM(t)
	e.write(t, map[string]string{"tab\there.txt": "x", "ok.txt": "y"})
	l, err := List(e.data, "", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Entries) != 2 || !l.Entries[0].Odd && !l.Entries[1].Odd {
		t.Fatalf("entries = %+v", l.Entries)
	}
	for _, en := range l.Entries {
		if en.Odd && en.Name != "tab?here.txt" {
			t.Errorf("odd name shown as %q", en.Name)
		}
	}
}

// Links are never followed, wherever they are in a path, even when they point inside.
func TestLinksNotFollowed(t *testing.T) {
	e := newFM(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644)
	e.write(t, map[string]string{"klasör/a.txt": "a"})
	os.Symlink("klasör", filepath.Join(e.dataDir, "iç"))
	os.Symlink(outside, filepath.Join(e.dataDir, "dış"))
	os.Symlink("klasör/a.txt", filepath.Join(e.dataDir, "kısayol.txt"))
	for _, dir := range []string{"iç", "dış", "../", "klasör/../klasör", ".git", "klasör/a.txt"} {
		if _, err := List(e.data, dir, ListOptions{}); err == nil {
			t.Errorf("List(%q) worked", dir)
		}
	}
	for _, p := range []string{"iç/a.txt", "dış/secret", "kısayol.txt", "klasör", ""} {
		if f, err := OpenFile(e.data, p); err == nil {
			f.Close()
			t.Errorf("OpenFile(%q) worked", p)
		}
	}
	if err := Mkdir(e.data, "dış", "yeni"); err == nil {
		t.Error("Mkdir through a link worked")
	}
	if err := Move(e.data, "klasör", []string{"a.txt"}, "dış"); err == nil {
		t.Error("Move into a link worked")
	}
	if _, err := os.Stat(filepath.Join(outside, "a.txt")); err == nil {
		t.Error("file escaped")
	}
	f, err := OpenFile(e.data, "klasör/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "a" {
		t.Errorf("read %q", b)
	}
}

func TestNames(t *testing.T) {
	for _, n := range []string{"", ".", "..", "a/b", ".htaccess", "a\x00b", "tab\t", `a\b`, "C:x", strings.Repeat("x", 256)} {
		if CheckName(n) == nil {
			t.Errorf("CheckName(%q) passed", n)
		}
	}
	for _, n := range []string{"foto.jpg", "Çiçek Şenliği (2).png", "a b", "x.tar.gz"} {
		if err := CheckName(n); err != nil {
			t.Errorf("CheckName(%q): %v", n, err)
		}
	}
	for in, want := range map[string]string{"": "", "/": "", ".": "", "/a/b": "a/b", "a/b/": "a/b", "./a": "a"} {
		if got, err := CleanPath(in); err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"../a", "a/../b", "a/.git", "a//b"} {
		if _, err := CleanPath(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("CleanPath(%q): %v", in, err)
		}
	}
	for i, want := range []string{"foto.jpg", "foto (1).jpg", "foto (2).jpg"} {
		if got := numbered("foto.jpg", i); got != want {
			t.Errorf("numbered = %q", got)
		}
	}
	if got := numbered("arşiv.TAR.GZ", 1); got != "arşiv (1).TAR.GZ" {
		t.Errorf("numbered = %q", got)
	}
	if got := numbered("README", 3); got != "README (3)" {
		t.Errorf("numbered = %q", got)
	}
}

func TestMkdirRenameMove(t *testing.T) {
	e := newFM(t)
	e.write(t, map[string]string{"a.txt": "a", "b.txt": "b", "fotolar/c.jpg": "c", "hedef/b.txt": "eski"})
	if err := Mkdir(e.data, "", "yeni klasör"); err != nil {
		t.Fatal(err)
	}
	if err := Mkdir(e.data, "", "yeni klasör"); !errors.Is(err, ErrExists) {
		t.Errorf("mkdir twice: %v", err)
	}
	if err := Mkdir(e.data, "yok", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("mkdir in missing: %v", err)
	}
	if err := Rename(e.data, "", "a.txt", "b.txt"); !errors.Is(err, ErrExists) {
		t.Errorf("rename onto a taken name: %v", err)
	}
	if err := Rename(e.data, "", "yok.txt", "z.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename missing: %v", err)
	}
	if err := Rename(e.data, "", "a.txt", ".a"); !errors.Is(err, ErrInvalid) {
		t.Errorf("rename to hidden: %v", err)
	}
	if err := Rename(e.data, "", "a.txt", "ana.txt"); err != nil {
		t.Fatal(err)
	}
	// One taken name: nothing moves.
	if err := Move(e.data, "", []string{"ana.txt", "b.txt"}, "hedef"); !errors.Is(err, ErrExists) {
		t.Errorf("move with a taken name: %v", err)
	}
	if err := Move(e.data, "", []string{"fotolar"}, "fotolar"); !errors.Is(err, ErrInvalid) {
		t.Errorf("move into itself: %v", err)
	}
	if err := Move(e.data, "", []string{"ana.txt"}, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("move into the same folder: %v", err)
	}
	if err := Move(e.data, "", []string{"ana.txt", "ana.txt"}, "hedef"); !errors.Is(err, ErrInvalid) {
		t.Errorf("duplicate names: %v", err)
	}
	if err := Move(e.data, "", []string{"hedef"}, "hedef/x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("move below itself: %v", err)
	}
	if err := Move(e.data, "", []string{"ana.txt", "fotolar"}, "yeni klasör"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"b.txt": "b", "hedef": "/", "hedef/b.txt": "eski", "yeni klasör": "/",
		"yeni klasör/ana.txt": "a", "yeni klasör/fotolar": "/", "yeni klasör/fotolar/c.jpg": "c"}
	if got := e.files(t); !reflect.DeepEqual(got, want) {
		t.Errorf("tree = %v", got)
	}
}

func TestTrashAndRestore(t *testing.T) {
	e := newFM(t)
	e.write(t, map[string]string{"fotolar/a.jpg": "aaa", "fotolar/alt/b.jpg": "bb", "not.txt": "n", "kalan.txt": "k"})
	ent, err := Trash(e.data, e.trash, "", []string{"fotolar", "not.txt"}, stamp)
	if err != nil {
		t.Fatal(err)
	}
	if !TrashIDPattern.MatchString(ent.ID) || !strings.HasPrefix(ent.ID, "20261028-093000-") || ent.Files != 3 || ent.Dirs != 2 ||
		ent.Bytes != 6 || ent.Actor != "furkan" || ent.Dir != "" {
		t.Errorf("entry = %+v", ent)
	}
	if got := e.files(t); !reflect.DeepEqual(got, map[string]string{"kalan.txt": "k"}) {
		t.Errorf("after trash = %v", got)
	}
	list, err := TrashList(e.trash)
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0].Names, []string{"fotolar", "not.txt"}) || list[0].Bytes != 6 {
		t.Fatalf("trash = %+v %v", list, err)
	}
	// The name is taken meanwhile: the restored one gets a number.
	e.write(t, map[string]string{"not.txt": "yeni"})
	r, err := Untrash(e.data, e.trash, ent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Items) != 2 || r.Items[1] != (Renamed{"not.txt", "not (1).txt"}) || r.Items[0] != (Renamed{"fotolar", "fotolar"}) {
		t.Errorf("restored = %+v", r)
	}
	want := map[string]string{"kalan.txt": "k", "not.txt": "yeni", "not (1).txt": "n", "fotolar": "/", "fotolar/a.jpg": "aaa",
		"fotolar/alt": "/", "fotolar/alt/b.jpg": "bb"}
	if got := e.files(t); !reflect.DeepEqual(got, want) {
		t.Errorf("after restore = %v", got)
	}
	if list, _ := TrashList(e.trash); len(list) != 0 {
		t.Errorf("trash not empty: %+v", list)
	}
	if ents, _ := os.ReadDir(e.tDir); len(ents) != 0 {
		t.Errorf("trash folder not empty: %v", ents)
	}
	if _, err := Untrash(e.data, e.trash, ent.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("restore twice: %v", err)
	}
}

// A restore recreates the folder the items came from if it was deleted since.
func TestRestoreIntoDeletedFolder(t *testing.T) {
	e := newFM(t)
	e.write(t, map[string]string{"a/b/c.txt": "c"})
	ent, err := Trash(e.data, e.trash, "a/b", []string{"c.txt"}, stamp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(e.dataDir, "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := Untrash(e.data, e.trash, ent.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.files(t); got["a/b/c.txt"] != "c" {
		t.Errorf("tree = %v", got)
	}
}

func TestTrashMissingMovesNothing(t *testing.T) {
	e := newFM(t)
	e.write(t, map[string]string{"a.txt": "a"})
	if _, err := Trash(e.data, e.trash, "", []string{"a.txt", "yok.txt"}, stamp); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	if got := e.files(t); got["a.txt"] != "a" {
		t.Errorf("tree = %v", got)
	}
	if ents, _ := os.ReadDir(e.tDir); len(ents) != 0 {
		t.Errorf("trash = %v", ents)
	}
}

func upload(t *testing.T, body string) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "upload")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestPlace(t *testing.T) {
	e := newFM(t)
	e.write(t, map[string]string{"fotolar/kapak.jpg": "eski", "fotolar/alt/": ""})
	p, err := Place(e.data, e.trash, upload(t, "yeni"), "fotolar", "yeni.jpg", PlaceOptions{Overwrite: false}, stamp)
	if err != nil || p.Name != "yeni.jpg" || p.Size != 4 || p.Linked != (runtime.GOOS == "linux") {
		t.Fatalf("place = %+v %v", p, err)
	}
	p, err = Place(e.data, e.trash, upload(t, "ikinci"), "fotolar", "kapak.jpg", PlaceOptions{Overwrite: false}, stamp)
	if err != nil || p.Name != "kapak (1).jpg" || p.Trashed != "" {
		t.Fatalf("taken name = %+v %v", p, err)
	}
	p, err = Place(e.data, e.trash, upload(t, "değişti"), "fotolar", "kapak.jpg", PlaceOptions{Overwrite: true}, stamp)
	if err != nil || p.Name != "kapak.jpg" || p.Trashed == "" {
		t.Fatalf("overwrite = %+v %v", p, err)
	}
	if _, err := Place(e.data, e.trash, upload(t, "x"), "fotolar", "alt", PlaceOptions{Overwrite: true}, stamp); !errors.Is(err, ErrExists) {
		t.Errorf("overwrite a folder: %v", err)
	}
	if _, err := Place(e.data, e.trash, upload(t, "x"), "fotolar", ".htaccess", PlaceOptions{Overwrite: false}, stamp); !errors.Is(err, ErrInvalid) {
		t.Errorf("hidden name: %v", err)
	}
	want := map[string]string{"fotolar": "/", "fotolar/alt": "/", "fotolar/yeni.jpg": "yeni", "fotolar/kapak (1).jpg": "ikinci",
		"fotolar/kapak.jpg": "değişti"}
	if got := e.files(t); !reflect.DeepEqual(got, want) {
		t.Errorf("tree = %v", got)
	}
	list, _ := TrashList(e.trash)
	if len(list) != 1 || list[0].ID != p.Trashed || list[0].Reason != "overwrite" || !reflect.DeepEqual(list[0].Names, []string{"kapak.jpg"}) ||
		list[0].Dir != "fotolar" {
		t.Fatalf("trash = %+v", list)
	}
	// Restoring the replaced file gives it a number: the new one keeps its name.
	r, err := Untrash(e.data, e.trash, p.Trashed)
	if err != nil || r.Items[0].As != "kapak (2).jpg" {
		t.Errorf("restore = %+v %v", r, err)
	}
	if b, _ := os.ReadFile(filepath.Join(e.dataDir, "fotolar/kapak (2).jpg")); string(b) != "eski" {
		t.Errorf("restored = %q", b)
	}
	// No temporary file is left behind, in data/ or in the trash.
	for _, dir := range []string{filepath.Join(e.dataDir, "fotolar"), e.tDir} {
		ents, _ := os.ReadDir(dir)
		for _, en := range ents {
			if strings.HasPrefix(en.Name(), ".") {
				t.Errorf("left %s", en.Name())
			}
		}
	}
}

// Where a link is not possible, NoCopy reports it instead of copying (the agent then
// copies in a limited scope).
func TestPlaceNoCopy(t *testing.T) {
	e := newFM(t)
	_, err := Place(e.data, e.trash, upload(t, "x"), "", "a.txt", PlaceOptions{NoCopy: true}, stamp)
	if runtime.GOOS == "linux" && err != nil || runtime.GOOS != "linux" && !errors.Is(err, ErrNeedsCopy) {
		t.Errorf("err = %v", err)
	}
	if runtime.GOOS != "linux" {
		if ents, _ := os.ReadDir(e.tDir); len(ents) != 0 {
			t.Errorf("trash = %v", ents)
		}
		if ErrorCode(err) != "copy" {
			t.Errorf("code = %q", ErrorCode(err))
		}
	}
}

// Overwriting a file whose name disappears meanwhile is a plain placement; the trash
// entry made for it goes away again.
func TestPlaceOverwriteVanished(t *testing.T) {
	e := newFM(t)
	e.write(t, map[string]string{"a.txt": "eski"})
	d, _ := openDir(e.data, "")
	defer d.close()
	var old unix.Stat_t
	if err := unix.Fstatat(d.fd, "a.txt", &old, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(e.dataDir, "a.txt"))
	src := upload(t, "yeni")
	put := func(at int, n string) error { return copyOpen(src, 4, at, n) }
	p, err := placeOver(e.trash, d, "a.txt", &old, put, Placed{Size: 4}, stamp)
	if err != nil || p.Name != "a.txt" || p.Trashed != "" {
		t.Fatalf("place = %+v %v", p, err)
	}
	if got := e.files(t); got["a.txt"] != "yeni" {
		t.Errorf("tree = %v", got)
	}
	if ents, _ := os.ReadDir(e.tDir); len(ents) != 0 {
		t.Errorf("trash = %v", ents)
	}
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w, err := zw.Create(k)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(files[k]))
	}
	zw.Close()
	return buf.Bytes()
}

func TestUnpack(t *testing.T) {
	e := newFM(t)
	z := zipBytes(t, map[string]string{"fotolar/a.jpg": "a", "fotolar/yeni/b.jpg": "b", "not.txt": "n", ".DS_Store": "x",
		"__MACOSX/fotolar/._a.jpg": "x"})
	e.write(t, map[string]string{"gelen/arsiv.zip": string(z), "gelen/fotolar/c.jpg": "c"})
	u, err := Unpack(e.data, e.trash, "gelen/arsiv.zip", true, Options{}, stamp)
	if err != nil {
		t.Fatal(err)
	}
	if u.Files != 3 || u.Dir != "gelen" || u.Trashed == "" {
		t.Errorf("unpacked = %+v", u)
	}
	want := map[string]string{"gelen": "/", "gelen/fotolar": "/", "gelen/fotolar/a.jpg": "a", "gelen/fotolar/c.jpg": "c",
		"gelen/fotolar/yeni": "/", "gelen/fotolar/yeni/b.jpg": "b", "gelen/not.txt": "n"}
	if got := e.files(t); !reflect.DeepEqual(got, want) {
		t.Errorf("tree = %v", got)
	}
	// The work folder is gone; the archive is in the trash.
	ents, _ := os.ReadDir(e.tDir)
	if len(ents) != 1 || ents[0].Name() != u.Trashed {
		t.Errorf("trash folder = %v", ents)
	}
}

// A taken name stops the unpack before anything moves.
func TestUnpackConflict(t *testing.T) {
	e := newFM(t)
	z := zipBytes(t, map[string]string{"fotolar/a.jpg": "yeni", "fotolar/b.jpg": "b", "z.txt": "z"})
	e.write(t, map[string]string{"arsiv.zip": string(z), "fotolar/a.jpg": "eski"})
	before := e.files(t)
	_, err := Unpack(e.data, e.trash, "arsiv.zip", true, Options{}, stamp)
	if !errors.Is(err, ErrExists) || !strings.Contains(err.Error(), "fotolar/a.jpg") {
		t.Fatalf("err = %v", err)
	}
	if got := e.files(t); !reflect.DeepEqual(got, before) {
		t.Errorf("tree changed: %v", got)
	}
	if ents, _ := os.ReadDir(e.tDir); len(ents) != 0 {
		t.Errorf("trash = %v", ents)
	}
}

func TestUnpackTarGzAndRefusals(t *testing.T) {
	e := newFM(t)
	e.write(t, map[string]string{"a.tar.gz": string(tgz(t, entry{name: "x/y.txt", body: "y"})), "bozuk.zip": "PK\x03\x04bozuk",
		"metin.txt": "merhaba", "evil.tar.gz": string(tgz(t, entry{name: "../kaçak.txt", body: "x"}))})
	if _, err := Unpack(e.data, e.trash, "a.tar.gz", false, Options{}, stamp); err != nil {
		t.Fatal(err)
	}
	if got := e.files(t); got["x/y.txt"] != "y" || got["a.tar.gz"] == "" {
		t.Errorf("tree = %v", got)
	}
	for _, p := range []string{"bozuk.zip", "metin.txt", "evil.tar.gz", "yok.zip", "x"} {
		if _, err := Unpack(e.data, e.trash, p, false, Options{}, stamp); err == nil {
			t.Errorf("Unpack(%s) worked", p)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(e.dataDir), "kaçak.txt")); err == nil {
		t.Error("escaped")
	}
	if ents, _ := os.ReadDir(e.tDir); len(ents) != 0 {
		t.Errorf("work folders left: %v", ents)
	}
	// Room is asked before each file.
	e.write(t, map[string]string{"b.tar.gz": string(tgz(t, entry{name: "q.txt", body: "q"}))})
	full := errors.New("disk dolu")
	if _, err := Unpack(e.data, e.trash, "b.tar.gz", false, Options{Room: func(int64) error { return full }}, stamp); !errors.Is(err, full) {
		t.Errorf("room: %v", err)
	}
}

func TestPackZip(t *testing.T) {
	e := newFM(t)
	e.write(t, map[string]string{"fotolar/a.jpg": "jpeg verisi", "fotolar/not.txt": strings.Repeat("metin ", 100),
		"fotolar/.gizli": "h", "fotolar/alt/b.png": "png", "diğer.txt": "d"})
	os.Symlink("../diğer.txt", filepath.Join(e.dataDir, "fotolar/bağ"))
	var buf bytes.Buffer
	st, err := PackZip(e.data, "", []string{"fotolar"}, &buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 3 || st.Dirs != 2 || st.Skipped != 2 {
		t.Errorf("stats = %+v", st)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]uint16{}
	for _, f := range zr.File {
		got[f.Name] = f.Method
		if !strings.HasSuffix(f.Name, "/") {
			rc, _ := f.Open()
			b, err := io.ReadAll(rc)
			rc.Close()
			if err != nil || len(b) == 0 {
				t.Errorf("%s: %v", f.Name, err)
			}
		}
	}
	want := map[string]uint16{"fotolar/": zip.Store, "fotolar/a.jpg": zip.Store, "fotolar/not.txt": zip.Deflate,
		"fotolar/alt/": zip.Store, "fotolar/alt/b.png": zip.Store}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("zip = %v", got)
	}
	// No names: the folder's contents.
	buf.Reset()
	if _, err := PackZip(e.data, "fotolar/alt", nil, &buf, nil); err != nil {
		t.Fatal(err)
	}
	zr, _ = zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if len(zr.File) != 1 || zr.File[0].Name != "b.png" {
		t.Errorf("folder zip = %v", zr.File)
	}
	if _, err := PackZip(e.data, "", []string{"bağ"}, io.Discard, nil); err == nil {
		t.Error("zipped a missing name")
	}
}

func TestFilePassing(t *testing.T) {
	mine, theirs, err := SocketPair()
	if err != nil {
		t.Fatal(err)
	}
	defer mine.Close()
	e := newFM(t)
	e.write(t, map[string]string{"a/b.txt": "içerik"})
	r := Serve(Request{Op: "open", Path: "a/b.txt"}, Handles{Data: e.data, Extra: theirs})
	theirs.Close()
	if r.Error != "" || r.File == nil || r.File.Size != len64("içerik") || r.File.Name != "b.txt" {
		t.Fatalf("reply = %+v", r)
	}
	f, err := ReceiveFile(mine, "b.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	if string(b) != "içerik" {
		t.Errorf("got %q", b)
	}
	// Nothing sent: end of file, not a hang.
	m2, t2, _ := SocketPair()
	defer m2.Close()
	t2.Close()
	if _, err := ReceiveFile(m2, "x"); err == nil {
		t.Error("received from nothing")
	}
}

func len64(s string) int64 { return int64(len(s)) }

func TestServeCodes(t *testing.T) {
	e := newFM(t)
	h := Handles{Data: e.data, Trash: e.trash}
	for _, c := range []struct {
		req  Request
		code string
	}{
		{Request{Op: "nope"}, "invalid"},
		{Request{Op: "list", Dir: "yok"}, "notfound"},
		{Request{Op: "mkdir", Name: "a"}, ""},
		{Request{Op: "mkdir", Name: "a"}, "exists"},
		{Request{Op: "untrash", ID: "x"}, "notfound"},
		{Request{Op: "place", Name: "a"}, ""},
		{Request{Op: "rename", Name: "a", To: ".b"}, "invalid"},
	} {
		if r := Serve(c.req, h); r.Code != c.code {
			t.Errorf("%+v: code %q (%s)", c.req, r.Code, r.Error)
		}
	}
}

func TestUnpackStopsBombs(t *testing.T) {
	e := newFM(t)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("sifir.bin")
	io.CopyN(w, zeroReader{}, 70<<20) // 70 MB of zeros: about 70 KB compressed
	zw.Close()
	e.write(t, map[string]string{"bomba.zip": buf.String()})
	if _, err := Unpack(e.data, e.trash, "bomba.zip", false, Options{}, stamp); err == nil || !strings.Contains(err.Error(), "sıkıştırma") {
		t.Errorf("err = %v", err)
	}
	if got := e.files(t); len(got) != 1 {
		t.Errorf("tree = %v", got)
	}
	// A central directory over the member limit is refused before it is read.
	z := zipBytes(t, map[string]string{"a": "a", "b": "b", "c": "c"})
	if _, err := ExtractZip(bytes.NewReader(z), int64(len(z)), e.data, Options{Limits: archive.Limits{MaxTotal: 1 << 20, MaxFile: 1 << 20, MaxFiles: 2}}); err == nil ||
		!strings.Contains(err.Error(), "çok fazla üye") {
		t.Errorf("member limit: %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// What this user may not read is left out of a zip download and listed in it.
func TestPackZipUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	e := newFM(t)
	e.write(t, map[string]string{"a/ok.txt": "ok", "a/gizli.txt": "x", "a/kilitli/b.txt": "b"})
	os.Chmod(filepath.Join(e.dataDir, "a/gizli.txt"), 0)
	os.Chmod(filepath.Join(e.dataDir, "a/kilitli"), 0)
	t.Cleanup(func() { os.Chmod(filepath.Join(e.dataDir, "a/kilitli"), 0o755) })
	var buf bytes.Buffer
	st, err := PackZip(e.data, "", []string{"a"}, &buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	var names []string
	var note string
	for _, f := range zr.File {
		names = append(names, f.Name)
		if f.Name == UnreadableNote {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			note = string(b)
		}
	}
	if !reflect.DeepEqual(names, []string{"a/", "a/kilitli/", "a/ok.txt", UnreadableNote}) && !reflect.DeepEqual(names, []string{"a/", "a/ok.txt", "a/kilitli/", UnreadableNote}) {
		t.Errorf("zip = %v", names)
	}
	if !strings.Contains(note, "a/gizli.txt") || !strings.Contains(note, "a/kilitli/") || st.Files != 1 {
		t.Errorf("note = %q, stats %+v", note, st)
	}
}

func TestWalkerBudget(t *testing.T) {
	e := newFM(t)
	e.write(t, map[string]string{"a/1": "", "a/2": "", "a/3": "", "a/b/4": ""})
	d, _ := openDir(e.data, "")
	defer d.close()
	n := 0
	// a, a/1, a/2, a/3, a/b, a/b/4: six entries, each costing one.
	w := walker{budget: 6, visit: func(int, string, string, *unix.Stat_t) (bool, error) { n++; return true, nil }}
	if err := w.walk(d.fd, ""); err != nil || n != 6 {
		t.Errorf("6 entries, budget 6: %v, %d visits", err, n)
	}
	w.budget, n = 5, 0
	if err := w.walk(d.fd, ""); !errors.Is(err, errTooMany) {
		t.Errorf("budget 5: %v", err)
	}
}

func TestTrashListCaps(t *testing.T) {
	e := newFM(t)
	files := map[string]string{}
	var names []string
	for i := 0; i < 25; i++ {
		n := fmt.Sprintf("f%02d.txt", i)
		files["x/"+n] = "x"
		names = append(names, n)
	}
	e.write(t, files)
	if _, err := Trash(e.data, e.trash, "x", names, stamp); err != nil {
		t.Fatal(err)
	}
	list, err := TrashList(e.trash)
	if err != nil || len(list) != 1 || len(list[0].Names) != maxTrashNames || list[0].More != 5 || list[0].Files != 25 {
		t.Fatalf("list = %+v %v", list, err)
	}
	// A restore still puts back all of them.
	r, err := Untrash(e.data, e.trash, list[0].ID)
	if err != nil || len(r.Items) != 25 {
		t.Errorf("restore: %d %v", len(r.Items), err)
	}
}

func TestDirs(t *testing.T) {
	e := newFM(t)
	e.write(t, map[string]string{"b/": "", "a/c/": "", "a/çay/": "", ".git/x/": "", "dosya.txt": "x"})
	os.Symlink("a", filepath.Join(e.dataDir, "bağ"))
	got, err := Dirs(e.data)
	if err != nil || !reflect.DeepEqual(got, []string{"a", "a/c", "a/çay", "b"}) {
		t.Errorf("dirs = %q %v", got, err)
	}
}

func TestPlaceCopyNeedsRoom(t *testing.T) {
	e := newFM(t)
	full := errors.New("disk dolu")
	_, err := Place(e.data, e.trash, upload(t, "x"), "", "a.txt", PlaceOptions{Room: func(int64) error { return full }}, stamp)
	if runtime.GOOS == "linux" && err != nil || runtime.GOOS != "linux" && !errors.Is(err, full) {
		t.Errorf("err = %v", err) // a link needs no room, a copy does
	}
}

func TestSortKey(t *testing.T) {
	in := []string{"zebra", "Ünal", "uzun", "şeker", "sabun", "İnci", "ılgaz", "hamam", "Çay", "cam", "Gül", "ğ", "Öykü", "ot"}
	sort.Slice(in, func(a, b int) bool { return SortKey(in[a]) < SortKey(in[b]) })
	want := []string{"cam", "Çay", "Gül", "ğ", "hamam", "ılgaz", "İnci", "ot", "Öykü", "sabun", "şeker", "uzun", "Ünal", "zebra"}
	if !reflect.DeepEqual(in, want) {
		t.Errorf("order = %q", in)
	}
}
