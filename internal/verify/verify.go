// Package verify runs verify commands and condenses their output.
package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/mkarvan/doneguard/internal/config"
)

// waitDelay is how long to wait for descendants holding the output pipes
// after the shell exits.
var waitDelay = 3 * time.Second

// maxCapture bounds how much command output is kept in memory (the tail).
const maxCapture = 1 << 20

// Result is the outcome of one verify command.
type Result struct {
	Command  string
	Dir      string
	ExitCode int
	Output   string
	TimedOut bool
	Timeout  time.Duration
	StartErr string
	// Incomplete is set when the shell exited but descendants kept its output
	// open past the wait delay: the check did not finish, so it is a failure.
	Incomplete bool
	Duration   time.Duration
}

// OK reports whether the command succeeded.
func (r Result) OK() bool {
	return r.ExitCode == 0 && !r.TimedOut && r.StartErr == "" && !r.Incomplete
}

// Summary is a one-line description of a failure, e.g. "`go test ./...` failed (exit 1)".
func (r Result) Summary() string {
	switch {
	case r.StartErr != "":
		return fmt.Sprintf("`%s` could not be run: %s", oneLine(r.Command), r.StartErr)
	case r.TimedOut:
		return fmt.Sprintf("`%s` timed out after %s", oneLine(r.Command), r.Timeout)
	case r.Incomplete:
		return fmt.Sprintf("`%s` did not finish cleanly: background processes kept its output open after it exited (exit %d)", oneLine(r.Command), r.ExitCode)
	default:
		return fmt.Sprintf("`%s` failed (exit %d)", oneLine(r.Command), r.ExitCode)
	}
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

// Run executes command through the shell in dir with a timeout. Output
// (stdout and stderr combined) is captured, never forwarded: the hook's own
// stdout is reserved for the JSON decision.
func Run(command, dir string, timeout time.Duration, extraEnv []string) Result {
	if timeout <= 0 {
		timeout = config.DefaultTimeout
	}
	res := Result{Command: command, Dir: dir, Timeout: timeout}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	name, args := shellArgs(command)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "DONEGUARD=1", "CI_DONEGUARD=1"), extraEnv...)
	setProcessGroup(cmd)
	cmd.WaitDelay = waitDelay
	buf := &tailBuffer{max: maxCapture}
	cmd.Stdout = buf
	cmd.Stderr = buf
	start := time.Now()
	err := cmd.Run()
	res.Duration = time.Since(start)
	res.Output = buf.String()
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			if res.ExitCode < 0 {
				res.ExitCode = -1
				if res.StartErr == "" {
					res.StartErr = exitErr.String()
				}
			}
		} else if errors.Is(err, exec.ErrWaitDelay) {
			// The shell exited but a descendant kept the pipes open: the
			// check is incomplete, never a success.
			res.Incomplete = true
			if cmd.ProcessState != nil {
				res.ExitCode = cmd.ProcessState.ExitCode()
			}
			killGroup(cmd)
		} else {
			res.ExitCode = -1
			res.StartErr = err.Error()
		}
	}
	return res
}

// RunAll runs the commands in order and stops at the first failure.
// It returns all results so far; the last one is the failure if any.
func RunAll(cmds []config.Command, dir string) (results []Result, failed *Result) {
	for _, c := range cmds {
		r := Run(c.Command, dir, c.Timeout, nil)
		results = append(results, r)
		if !r.OK() {
			f := r
			return results, &f
		}
	}
	return results, nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu      sync.Mutex
	buf     []byte
	max     int
	dropped int64
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.dropped += int64(over)
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dropped > 0 {
		return fmt.Sprintf("[... %d bytes of earlier output dropped ...]\n", t.dropped) + string(t.buf)
	}
	return string(t.buf)
}
