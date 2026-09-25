//go:build !unix

package prove

import "os/exec"

func setProcessGroup(*exec.Cmd) {}

// processAlive cannot probe other processes here; assume a lock is live.
func processAlive(int) bool { return true }
