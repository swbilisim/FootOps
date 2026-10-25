//go:build unix

package install

import (
	"os/exec"
	"syscall"
)

// setOwnProcessGroup detaches cmd from the terminal's foreground process group, so
// terminal signals (Ctrl+C, hang-up) are delivered to the installer only.
func setOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
