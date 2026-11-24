//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// signalProcess signals the child pid, not its process group.
// The child stays in statuspan's group so a terminal keeps it in the
// foreground. A new group would stop an interactive command with
// SIGTTIN or SIGTTOU, and signaling the group would also hit statuspan.
func signalProcess(cmd *exec.Cmd, sig os.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(sig)
}

func waitStatusCode(ee *exec.ExitError) int {
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		if code := ee.ExitCode(); code >= 0 {
			return code
		}
		return 1
	}
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}
