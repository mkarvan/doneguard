// Package sessionlog appends and reads .doneguard/sessions.jsonl.
package sessionlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mkarvan/doneguard/internal/filelock"
)

// LockTimeout bounds how long Append waits for a concurrent writer; on
// timeout the entry is skipped rather than delaying the hook verdict.
var LockTimeout = 2 * time.Second

// Entry is one hook invocation (or wrapper attempt).
type Entry struct {
	Timestamp           string   `json:"timestamp"`
	SessionID           string   `json:"session_id"`
	AgentID             string   `json:"agent_id,omitempty"`
	HookEventName       string   `json:"hook_event_name"`
	Source              string   `json:"source"` // claude | codex | wrapper
	Decision            string   `json:"decision"`
	Reason              string   `json:"reason,omitempty"`
	BlockKind           string   `json:"block_kind,omitempty"` // verify | protected | config
	VerifyOK            *bool    `json:"verify_ok"`
	VerifySkipped       bool     `json:"verify_skipped,omitempty"`
	FailingCommand      string   `json:"failing_command,omitempty"`
	FailingExitCode     *int     `json:"failing_exit_code,omitempty"`
	ProtectedViolations []string `json:"protected_violations"`
	RetryCount          int      `json:"retry_count"`
	MaxRetries          int      `json:"max_retries"`
	Unverified          bool     `json:"unverified"`
	StopHookActive      *bool    `json:"stop_hook_active,omitempty"`
	// Token usage is cumulative for the transcript at the time of the stop.
	InputTokens          *int64   `json:"input_tokens"`
	OutputTokens         *int64   `json:"output_tokens"`
	TotalTokens          *int64   `json:"total_tokens"`
	LastAssistantMessage string   `json:"last_assistant_message,omitempty"`
	DurationMS           int64    `json:"duration_ms"`
	Notes                []string `json:"notes,omitempty"`
}

// CheckFailed reports whether verification failed for this attempt (the
// agent tried to stop but the work was not actually done).
func (e *Entry) CheckFailed() bool {
	return (e.VerifyOK != nil && !*e.VerifyOK) || len(e.ProtectedViolations) > 0
}

// FalseDone reports whether this attempt counts as a false-done: the
// agent tried to stop, verification failed (failing verify command or
// protected path touched) and DoneGuard blocked the stop. Allows at the
// retry cap are UNVERIFIED, not additional false-dones.
func (e *Entry) FalseDone() bool {
	return e.Decision == "block" && e.CheckFailed()
}

// Append writes one JSON line. It uses O_APPEND plus a bounded interprocess
// lock so concurrent hook invocations (parallel subagents) never interleave
// lines. The log is private (0600) and must be a regular file.
func Append(path string, e *Entry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if st, err := os.Lstat(path); err == nil && !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file; not logging", path)
	}
	if e.ProtectedViolations == nil {
		e.ProtectedViolations = []string{}
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	release, err := filelock.Acquire(path+".lock", LockTimeout)
	if err != nil {
		return fmt.Errorf("locking %s: %w", path, err)
	}
	defer release()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// Read parses all entries, skipping malformed lines (counted in bad).
func Read(r io.Reader) (entries []Entry, bad int, err error) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, rerr := br.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			var e Entry
			if json.Unmarshal(line, &e) == nil && e.Decision != "" {
				entries = append(entries, e)
			} else {
				bad++
			}
		}
		if rerr == io.EOF {
			return entries, bad, nil
		}
		if rerr != nil {
			return entries, bad, rerr
		}
	}
}

// ReadFile reads a sessions.jsonl file.
func ReadFile(path string) ([]Entry, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	return Read(f)
}
