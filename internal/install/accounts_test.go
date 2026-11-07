package install

import (
	"strings"
	"testing"
)

func TestFreeAdminID(t *testing.T) {
	h, _ := newTestHost(t, nil)
	if id, err := freeAdminID(h); err != nil || id != 2000 {
		t.Fatalf("empty host: %d %v", id, err)
	}
	put(t, h, "/etc/passwd", "root:x:0:0:root:/root:/bin/bash\nubuntu:x:1000:1000::/home/ubuntu:/bin/bash\nbaska:x:2000:2000::/home/baska:/bin/bash\n")
	put(t, h, "/etc/group", "root:x:0:\nsudo:x:27:ubuntu\nbaska:x:2000:\ngrup:x:2001:\n")
	if id, err := freeAdminID(h); err != nil || id != 2002 {
		t.Fatalf("taken uid and gid: %d %v", id, err)
	}
}

func TestAccountNotes(t *testing.T) {
	h, _ := newTestHost(t, nil)
	put(t, h, "/etc/passwd", strings.Join([]string{
		"root:x:0:0:root:/root:/bin/bash",
		"ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash",
		"furkan:x:1001:1001::/home/furkan:/bin/bash",
		"admin:x:999:999::/nonexistent:/usr/sbin/nologin", // a service account of that name: not a login
	}, "\n"))
	put(t, h, "/etc/group", "sudo:x:27:ubuntu,furkan\nfurkan:x:1001:\n")
	put(t, h, "/etc/sudoers", "root ALL=(ALL:ALL) ALL\n# %wheel ALL=(ALL) NOPASSWD: ALL\n#includedir /etc/sudoers.d\n")
	put(t, h, "/etc/sudoers.d/90-cloud-init-users", "# Created by cloud-init\nubuntu ALL=(ALL) NOPASSWD:ALL\n")
	put(t, h, "/etc/sudoers.d/90-footops-furkan", "furkan ALL=(ALL) NOPASSWD:ALL\n")
	put(t, h, "/etc/sudoers.d/README.txt", "x ALL=(ALL) NOPASSWD:ALL\n") // sudo skips names with a dot

	notes := strings.Join(accountNotes(h, "furkan", SudoNoPassword), "\n")
	for _, want := range []string{"Yönetici hesabı furkan uid 1001 / gid 1001", "sudo usermod -u 2000 furkan && sudo groupmod -g 2000 furkan", "sudo find /home/furkan -xdev -gid 1001 -exec chgrp -h 2000",
		"Bulut imajının hesabı ubuntu duruyor (uid 1000, sudo grubunda)", "sudo userdel -r ubuntu",
		"/etc/sudoers.d/90-cloud-init-users → ubuntu ALL=(ALL) NOPASSWD:ALL"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	for _, not := range []string{"README", "admin", "90-footops-furkan", "%wheel"} {
		if strings.Contains(notes, not) {
			t.Errorf("notes mention %q:\n%s", not, notes)
		}
	}
	// With a sudo password the admin's own NOPASSWD rule is a leftover too.
	if n := strings.Join(accountNotes(h, "furkan", SudoPassword), "\n"); !strings.Contains(n, "90-footops-furkan") {
		t.Errorf("own rule with a password mode:\n%s", n)
	}
	// A clean server: nothing to say.
	h2, _ := newTestHost(t, nil)
	put(t, h2, "/etc/passwd", "root:x:0:0:root:/root:/bin/bash\nfurkan:x:2000:2000::/home/furkan:/bin/bash\n")
	put(t, h2, "/etc/group", "sudo:x:27:furkan\nfurkan:x:2000:\n")
	if n := accountNotes(h2, "furkan", SudoPassword); len(n) != 0 {
		t.Errorf("clean server: %v", n)
	}
}
