//go:build unix

package verify

import (
	"os/exec"
	"syscall"
)

func shellArgs(script string) (string, []string) {
	return "/bin/sh", []string{"-c", script}
}

// setProcessGroup puts the command in its own process group so a timeout
// kills the whole tree (test runners love to fork).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// killGroup kills any descendants left in the command's process group.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
