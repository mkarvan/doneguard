//go:build !unix

package verify

import "os/exec"

func shellArgs(script string) (string, []string) {
	return "cmd", []string{"/C", script}
}

func setProcessGroup(cmd *exec.Cmd) {}

// killGroup is unsupported here: descendants are not tracked on this platform.
func killGroup(cmd *exec.Cmd) {}
