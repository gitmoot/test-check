//go:build !unix

package prove

import "os/exec"

func setProcessGroup(*exec.Cmd) {}
