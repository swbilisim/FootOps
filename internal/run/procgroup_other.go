//go:build !unix

package run

import "os/exec"

func setOwnProcessGroup(*exec.Cmd) {}
