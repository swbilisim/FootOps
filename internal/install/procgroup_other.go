//go:build !unix

package install

import "os/exec"

func setOwnProcessGroup(*exec.Cmd) {}
