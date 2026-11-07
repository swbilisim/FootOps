package install

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Accounts and container users (2026-11-03): processes of a container that runs as uid
// 1000 or 1001 (Node and Next.js images) are, on the server, the account with that uid.
// When that is the admin account, whoever has its SSH key reads the sites' secrets in
// /proc/<pid>/environ and stops their processes without the sudo password. So the admin
// account gets a uid from adminMinID up, and the end of a run points out an older
// install's admin account below it, cloud images' leftover accounts and password-less
// sudo rules.

const (
	adminMinID = 2000
	adminMaxID = 2999
)

// cloudUsers are the default accounts of cloud images: unused once the admin account is
// set up, and often allowed sudo without a password (cloud-init).
var cloudUsers = []string{"ubuntu", "debian", "admin", "ec2-user", "cloud-user", "centos", "rocky", "almalinux", "fedora", "opc"}

// passwdEntry is a line of /etc/passwd.
type passwdEntry struct {
	name        string
	uid, gid    int
	home, shell string
}

func readPasswd(h Host) ([]passwdEntry, error) {
	data, err := h.ReadFile("/etc/passwd")
	if err != nil {
		return nil, err
	}
	var out []passwdEntry
	for _, line := range strings.Split(data, "\n") {
		f := strings.Split(line, ":")
		if len(f) < 7 {
			continue
		}
		uid, err1 := strconv.Atoi(f[2])
		gid, err2 := strconv.Atoi(f[3])
		if err1 == nil && err2 == nil {
			out = append(out, passwdEntry{name: f[0], uid: uid, gid: gid, home: f[5], shell: f[6]})
		}
	}
	return out, nil
}

// groupIDs maps the groups of /etc/group to their ids and members.
// groupName is the name of the group with gid (its number if there is none).
func groupName(h Host, gid int) string {
	data, _ := h.ReadFile("/etc/group")
	for _, line := range strings.Split(data, "\n") {
		if f := strings.Split(line, ":"); len(f) >= 3 && f[2] == strconv.Itoa(gid) {
			return f[0]
		}
	}
	return strconv.Itoa(gid)
}

func readGroups(h Host) (ids map[int]bool, members map[string][]string, err error) {
	data, err := h.ReadFile("/etc/group")
	if err != nil {
		return nil, nil, err
	}
	ids, members = map[int]bool{}, map[string][]string{}
	for _, line := range strings.Split(data, "\n") {
		f := strings.Split(line, ":")
		if len(f) < 4 {
			continue
		}
		if id, err := strconv.Atoi(f[2]); err == nil {
			ids[id] = true
		}
		if f[3] != "" {
			members[f[0]] = strings.Split(f[3], ",")
		}
	}
	return ids, members, nil
}

// freeAdminID is the first id from adminMinID that no user and no group has, so the
// admin account and its group get the same one.
func freeAdminID(h Host) (int, error) {
	users, err := readPasswd(h)
	if err != nil {
		return 0, err
	}
	groups, _, err := readGroups(h)
	if err != nil {
		return 0, err
	}
	taken := map[int]bool{}
	for _, u := range users {
		taken[u.uid] = true
	}
	for id := adminMinID; id <= adminMaxID; id++ {
		if !taken[id] && !groups[id] {
			return id, nil
		}
	}
	return 0, fmt.Errorf("%d-%d arasında boş kullanıcı numarası yok", adminMinID, adminMaxID)
}

// accountNotes lists what the operator should put right about the accounts (nothing is
// changed: an account in use cannot be renumbered from its own session).
func accountNotes(h Host, admin string, mode SudoMode) []string {
	users, err := readPasswd(h)
	if err != nil {
		return nil
	}
	_, members, _ := readGroups(h)
	sudoers := map[string]bool{}
	for _, m := range members["sudo"] {
		sudoers[m] = true
	}
	var notes []string
	for _, u := range users {
		if u.name == admin && (u.uid < adminMinID || u.gid < adminMinID) {
			id, err := freeAdminID(h)
			if err != nil {
				id = adminMinID
			}
			group := groupName(h, u.gid)
			notes = append(notes, fmt.Sprintf("Yönetici hesabı %s uid %d / gid %d ile: container'ların kullandığı aralıkta (Node imajları 1000, Next.js 1001). "+
				"Bu uid'le çalışan container'ların süreçleri sunucuda bu hesabın sayılır: SSH anahtarı çalınırsa sudo şifresi olmadan sitelerin sırları okunur. "+
				"Düzeltme (bu hesabın oturumu kapalıyken, başka bir sudo hesabıyla): sudo usermod -u %d %s && sudo groupmod -g %d %s && "+
				"sudo find %s -xdev -gid %d -exec chgrp -h %d {} + (yalnız ev klasörü: başka yerde aynı numara container dosyalarınındır)",
				u.name, u.uid, u.gid, id, u.name, id, group, u.home, u.gid, id))
		}
	}
	for _, name := range cloudUsers {
		for _, u := range users {
			if u.name != name || name == admin || strings.HasSuffix(u.shell, "nologin") || strings.HasSuffix(u.shell, "/false") {
				continue
			}
			extra := ""
			if sudoers[name] {
				extra = ", sudo grubunda"
			}
			notes = append(notes, fmt.Sprintf("Bulut imajının hesabı %s duruyor (uid %d%s). Kullanılmıyorsa sil: sudo userdel -r %s", name, u.uid, extra, name))
		}
	}
	for _, r := range nopasswdRules(h) {
		if mode == SudoNoPassword && r.file == "/etc/sudoers.d/90-footops-"+admin {
			continue // chosen at setup
		}
		notes = append(notes, fmt.Sprintf("Şifresiz sudo kuralı: %s → %s. Hesap kullanılmıyorsa dosyayı (ya da satırı) kaldır, sonra sudo visudo -c", r.file, r.line))
	}
	return notes
}

type sudoRule struct{ file, line string }

// nopasswdRules finds NOPASSWD rules in /etc/sudoers and the files sudo reads from
// /etc/sudoers.d (it skips names with a dot or ending in ~).
func nopasswdRules(h Host) []sudoRule {
	files := []string{"/etc/sudoers"}
	if ents, err := os.ReadDir(h.Path("/etc/sudoers.d")); err == nil {
		for _, e := range ents {
			if n := e.Name(); !e.IsDir() && !strings.Contains(n, ".") && !strings.HasSuffix(n, "~") {
				files = append(files, "/etc/sudoers.d/"+n)
			}
		}
	}
	sort.Strings(files[1:])
	var out []sudoRule
	for _, f := range files {
		data, err := h.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(data, "\n") {
			l := strings.TrimSpace(line)
			if l == "" || strings.HasPrefix(l, "#") || !strings.Contains(l, "NOPASSWD") {
				continue
			}
			out = append(out, sudoRule{file: f, line: l})
		}
	}
	return out
}
