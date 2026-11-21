//go:build unix

package cliagent

import (
	"errors"
	"os/exec"
	"syscall"
)

// setupProcGroup puts the CLI child in its own process group and cancels
// by SIGKILL-ing the whole group. exec.CommandContext's default kill hits
// only the direct child — but CLI agents (claude/gemini/…) spawn their own
// children, and those grandchildren survive /stop or the turn watchdog,
// leaking processes (and holding the inherited stdout pipe open).
func setupProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
}
