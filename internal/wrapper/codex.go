// Package wrapper implements `doneguard run codex -- <args>`: a fallback for
// environments where Codex hooks cannot be trusted or are unavailable. It
// runs Codex, verifies, and resumes the session with the failure reason until
// verification passes or the retry cap is hit.
package wrapper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mkarvan/doneguard/internal/config"
	"github.com/mkarvan/doneguard/internal/gitutil"
	"github.com/mkarvan/doneguard/internal/hook"
	"github.com/mkarvan/doneguard/internal/safepath"
	"github.com/mkarvan/doneguard/internal/sessionlog"
	"github.com/mkarvan/doneguard/internal/transcript"
)

// ExitUnverified is returned when the retry cap is hit with checks failing.
const ExitUnverified = 3

// Options configures Run.
type Options struct {
	CodexBin string // default: $DONEGUARD_CODEX_BIN or "codex"
	Args     []string
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	Getwd    func() (string, error)
	Getenv   func(string) string
}

// Run executes the wrapper loop and returns the process exit code.
//
// The wrapper only resumes a session whose identity it knows for certain:
// a validated id from Codex's structured `thread.started` event (--json),
// the "session id:" line of the exec header, or an explicit
// `resume <id>` in the original arguments. It never falls back to
// `resume --last`, which could pick up a different, concurrent session. When
// identity or the execution context cannot be preserved (interactive mode,
// --ephemeral, unrecognised options), it verifies once and exits UNVERIFIED
// on failure instead of resuming.
func Run(opts Options) int {
	if opts.Getwd == nil {
		opts.Getwd = os.Getwd
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.CodexBin == "" {
		opts.CodexBin = opts.Getenv("DONEGUARD_CODEX_BIN")
	}
	if opts.CodexBin == "" {
		opts.CodexBin = "codex"
	}
	say := func(format string, args ...any) {
		fmt.Fprintf(opts.Stderr, "doneguard: "+format+"\n", args...)
	}
	cwd, err := opts.Getwd()
	if err != nil {
		say("%v", err)
		return 1
	}
	plan := parseArgs(opts.Args)
	if plan.passthrough {
		code, _, _, err := runChild(opts, opts.Args, cwd, opts.Stdin, false)
		if err != nil {
			say("could not start %s: %v", opts.CodexBin, err)
		}
		return code
	}
	workDir := cwd
	if plan.cd != "" {
		workDir = plan.cd
		if !filepath.IsAbs(workDir) {
			workDir = filepath.Join(cwd, workDir)
		}
		workDir = filepath.Clean(workDir)
	}
	plan.absCd = workDir

	pol := hook.ResolvePolicy(workDir)
	if !pol.Active() {
		say("no .doneguard.yml found from %s; running codex without verification (run `doneguard init`)", workDir)
		code, _, _, err := runChild(opts, opts.Args, cwd, opts.Stdin, false)
		if err != nil {
			say("could not start %s: %v", opts.CodexBin, err)
		}
		return code
	}
	if pol.GitErr != nil {
		say("cannot determine the DoneGuard policy (git error: %v); not starting codex", pol.GitErr)
		return 2
	}
	if len(pol.Violations) > 0 {
		var names []string
		for _, v := range pol.Violations {
			names = append(names, v.String())
		}
		say("the DoneGuard config differs from the committed one (%s); commit or restore it before running the wrapper", strings.Join(names, ", "))
		return 2
	}
	cfg, err := pol.Load()
	if err != nil {
		say("%s", hook.ConfigErrorReason(filepath.Base(pol.Path), err))
		return 2
	}
	maxRetries := cfg.EffectiveMaxRetries()
	baseHead := ""
	if root, err := gitutil.Root(cfg.Dir); err == nil {
		baseHead = gitutil.Head(root)
	}
	canResume, whyNot := true, ""
	switch {
	case plan.unsupported != "":
		canResume, whyNot = false, plan.unsupported
	case plan.ephemeral:
		canResume, whyNot = false, "--ephemeral sessions cannot be resumed"
	case !plan.isExec && plan.explicitID == "":
		canResume, whyNot = false, "interactive sessions do not report a session id DoneGuard can pin"
	}
	if !canResume {
		say("note: %s; DoneGuard will verify once but cannot retry", whyNot)
	}

	start := time.Now()
	sessionID := plan.explicitID
	args := opts.Args
	stdin := opts.Stdin
	retries := 0
	for {
		code, captured, interrupted, startErr := runChild(opts, args, cwd, stdin, plan.isExec)
		if startErr != nil {
			say("could not start %s: %v", opts.CodexBin, startErr)
			return 127
		}
		if interrupted {
			say("codex was interrupted; skipping verification")
			return code
		}
		if code != 0 {
			say("codex exited with status %d (the turn did not complete); skipping verification", code)
			return code
		}
		switch {
		case captured.conflict:
			canResume, whyNot = false, "codex output reported conflicting session ids"
		case captured.id != "" && sessionID != "" && captured.id != sessionID:
			canResume, whyNot = false, fmt.Sprintf("codex reported session %s, expected %s", captured.id, sessionID)
		case captured.id != "" && sessionID == "":
			sessionID = captured.id
		}
		if canResume && sessionID == "" {
			canResume, whyNot = false, "could not capture the codex session id"
		}
		say("verifying (%s)…", describeChecks(cfg))
		res := hook.Check(cfg, baseHead)
		entry := &sessionlog.Entry{
			Timestamp:           time.Now().UTC().Format(time.RFC3339),
			SessionID:           sessionID,
			HookEventName:       "WrapperStop",
			Source:              "wrapper",
			MaxRetries:          maxRetries,
			VerifyOK:            res.VerifyOK(),
			VerifySkipped:       res.VerifySkipped,
			ProtectedViolations: res.ViolationStrings(),
			Notes:               res.Notes,
		}
		if entry.SessionID == "" {
			entry.SessionID = "wrapper-" + start.UTC().Format("20060102T150405Z")
		}
		if res.GitErr != "" {
			entry.Notes = append(entry.Notes, "protected-path check indeterminate: "+res.GitErr)
		}
		if res.Failed != nil {
			entry.FailingCommand = res.Failed.Command
			c := res.Failed.ExitCode
			entry.FailingExitCode = &c
		}
		if u := codexUsage(sessionID, opts.Getenv); u != nil {
			entry.InputTokens, entry.OutputTokens, entry.TotalTokens = &u.InputTokens, &u.OutputTokens, &u.TotalTokens
		}
		logEntry := func() {
			entry.DurationMS = time.Since(start).Milliseconds()
			if err := appendLog(cfg, entry); err != nil {
				say("could not write session log: %v", err)
			}
		}
		if res.OK() {
			entry.Decision = "allow"
			logEntry()
			if res.VerifySkipped {
				say("protected paths clean; no verify commands configured (verify skipped)")
			} else {
				say("verified ✓")
			}
			return code
		}
		reason := hook.BuildReason(cfg, res)
		entry.Reason = reason
		entry.BlockKind = res.Kind()
		if retries >= maxRetries || !canResume {
			entry.Decision = "allow"
			entry.Unverified = true
			entry.BlockKind = ""
			entry.RetryCount = retries
			if !canResume && retries < maxRetries {
				entry.Notes = append(entry.Notes, "not resumed: "+whyNot)
			}
			logEntry()
			if !canResume && retries < maxRetries {
				fmt.Fprintf(opts.Stderr, "\n%s\n\ndoneguard: UNVERIFIED — verification failed and the session cannot be resumed safely (%s).\n", reason, whyNot)
			} else {
				fmt.Fprintf(opts.Stderr, "\n%s\n\ndoneguard: UNVERIFIED — verification still failing after %d retr%s.\n", reason, retries, plural(retries, "y", "ies"))
			}
			return ExitUnverified
		}
		retries++
		entry.Decision = "block"
		entry.RetryCount = retries
		logEntry()
		fmt.Fprintf(opts.Stderr, "\n%s\n\n", reason)
		say("resuming codex session %s with the failure (retry %d/%d)", sessionID, retries, maxRetries)
		args = resumeArgs(plan, sessionID, ResumePrompt(reason))
		stdin = nil
	}
}

func appendLog(cfg *config.Config, entry *sessionlog.Entry) error {
	dd, err := hook.PrepareDataDir(cfg.Dir)
	if err != nil {
		return err
	}
	p, err := safepath.Resolve(dd, filepath.Join(dd, "sessions.jsonl"))
	if err != nil {
		return err
	}
	return sessionlog.Append(p, entry)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func describeChecks(cfg *config.Config) string {
	if len(cfg.Verify) == 0 {
		return "protected paths only"
	}
	var cmds []string
	for _, v := range cfg.Verify {
		cmds = append(cmds, v.Command)
	}
	return strings.Join(cmds, "; ")
}

// ResumePrompt is the prompt sent when resuming after a failed verification.
func ResumePrompt(reason string) string {
	return "DoneGuard verification failed after your last turn, so the task is not done yet.\n\n" +
		reason + "\n\nKeep working until verification passes, then finish."
}

type argPlan struct {
	isExec bool
	// rootFlags are options given before the subcommand/prompt; they are
	// repeated at the same position (`codex [exec] <rootFlags> resume ...`)
	// so profile, sandbox, approval and extra directories are preserved.
	rootFlags []string
	// preFlags are root options given before the `exec` subcommand
	// (`codex -c k=v exec ...`); they are repeated before `exec`.
	preFlags []string
	// resumeFlags are options that followed an explicit `resume`.
	resumeFlags []string
	cd          string
	absCd       string
	ephemeral   bool
	explicitID  string
	// unsupported explains why resume arguments cannot be rebuilt safely.
	unsupported string
	// passthrough marks non-task invocations (--version, login, ...).
	passthrough bool
}

// Codex CLI 0.170 options. Value flags consume the next argument unless
// given as --flag=value.
var (
	valueFlags = map[string]bool{"-c": true, "--config": true, "--enable": true, "--disable": true, "-m": true, "--model": true,
		"-p": true, "--profile": true, "-s": true, "--sandbox": true, "-a": true, "--ask-for-approval": true,
		"--add-dir": true, "--local-provider": true, "-o": true, "--output-last-message": true, "--output-schema": true,
		"--color": true, "-i": true, "--image": true, "-C": true, "--cd": true, "--remote": true,
		"--remote-auth-token-env": true, "--thread-source": true, "--cyber-access-program": true}
	boolFlags = map[string]bool{"--dangerously-bypass-approvals-and-sandbox": true, "--dangerously-bypass-hook-trust": true,
		"--strict-config": true, "--skip-git-repo-check": true, "--ignore-user-config": true, "--ignore-rules": true,
		"--json": true, "--full-auto": true, "--oss": true, "--search": true, "--no-alt-screen": true,
		"--approve-for-me": true, "--ephemeral": true, "--last": true, "--all": true}
	// Not repeated on resume: tied to the original prompt or replaced.
	droppedFlags = map[string]bool{"-i": true, "--image": true, "-C": true, "--cd": true, "--ephemeral": true}
	// Top-level codex subcommands that are not agent turns.
	nonTaskCommands = map[string]bool{"login": true, "logout": true, "mcp": true, "mcp-server": true, "app-server": true,
		"completion": true, "sandbox": true, "debug": true, "apply": true, "a": true, "help": true, "features": true,
		"cloud": true, "proto": true, "generate-ts": true}
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func parseArgs(args []string) argPlan {
	var p argPlan
	rest := args
	inResume := false
	sawLast := false
	positionals := 0
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positionals++
			switch {
			case positionals == 1 && !p.isExec && (a == "exec" || a == "e"):
				// Root options may precede the subcommand.
				p.isExec = true
				p.preFlags, p.rootFlags = p.rootFlags, nil
				positionals = 0
			case positionals == 1 && a == "resume":
				inResume = true
			case positionals == 1 && !p.isExec && nonTaskCommands[a]:
				p.passthrough = true
				return p
			case inResume && positionals == 2 && uuidRe.MatchString(a):
				p.explicitID = a
			}
			continue
		}
		switch a {
		case "--version", "-V", "--help", "-h":
			p.passthrough = true
			return p
		}
		name, val, hasVal := a, "", false
		if strings.HasPrefix(a, "--") {
			if j := strings.IndexByte(a, '='); j > 0 {
				name, val, hasVal = a[:j], a[j+1:], true
			}
		} else if len(a) > 2 && valueFlags[a[:2]] {
			// Attached short value: -C/path, -mgpt-5, -C=path.
			name, val, hasVal = a[:2], strings.TrimPrefix(a[2:], "="), true
		}
		var tokens []string
		switch {
		case valueFlags[name]:
			if !hasVal {
				if i+1 >= len(rest) {
					p.unsupported = "option " + name + " has no value"
					return p
				}
				val = rest[i+1]
				i++
			}
			tokens = []string{name, val}
		case boolFlags[name] && !hasVal:
			tokens = []string{name}
		default:
			if p.unsupported == "" {
				p.unsupported = "unrecognised codex option " + a
			}
			continue
		}
		switch name {
		case "-C", "--cd":
			p.cd = val
		case "--ephemeral":
			p.ephemeral = true
		}
		if droppedFlags[name] {
			continue
		}
		if inResume {
			if name == "--last" {
				sawLast = true
			}
			if name == "--last" || name == "--all" {
				continue
			}
			p.resumeFlags = append(p.resumeFlags, tokens...)
		} else {
			p.rootFlags = append(p.rootFlags, tokens...)
		}
	}
	if sawLast {
		// `resume --last <text>` selects the most recent session and treats
		// the positional as the prompt, even when it looks like a session id.
		p.explicitID = ""
		if p.unsupported == "" {
			p.unsupported = "the original command resumes the most recent session (--last), not a pinned id"
		}
	}
	if inResume && p.explicitID == "" && p.unsupported == "" {
		p.unsupported = "the original command resumes a session without an explicit id"
	}
	return p
}

func resumeArgs(p argPlan, sessionID, prompt string) []string {
	out := append([]string{}, p.preFlags...)
	if p.isExec {
		out = append(out, "exec")
	}
	out = append(out, p.rootFlags...)
	if p.absCd != "" {
		out = append(out, "-C", p.absCd)
	}
	out = append(out, "resume")
	out = append(out, p.resumeFlags...)
	return append(out, sessionID, prompt)
}

// captureResult is the session identity observed in one codex run.
type captureResult struct {
	id       string
	conflict bool
}

// jsonIDScanner reads the first line of `codex exec --json` stdout, which
// must be the structured thread.started event.
type jsonIDScanner struct {
	mu   sync.Mutex
	line []byte
	done bool
	id   string
}

const maxFirstLine = 64 << 10

func (s *jsonIDScanner) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return len(p), nil
	}
	for _, c := range p {
		if c == '\n' {
			if len(bytes.TrimSpace(s.line)) == 0 {
				s.line = s.line[:0]
				continue
			}
			s.finish()
			return len(p), nil
		}
		s.line = append(s.line, c)
		if len(s.line) > maxFirstLine {
			s.done = true
			return len(p), nil
		}
	}
	return len(p), nil
}

func (s *jsonIDScanner) finish() {
	s.done = true
	var ev struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
	}
	if json.Unmarshal(bytes.TrimSpace(s.line), &ev) == nil && ev.Type == "thread.started" && uuidRe.MatchString(ev.ThreadID) {
		s.id = ev.ThreadID
	}
	s.line = nil
}

func (s *jsonIDScanner) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// headerIDScanner finds the "session id: <uuid>" line that codex exec
// prints in its stderr header. Only whole lines within the first few KiB
// count, so session-like text later in the agent's output is ignored.
type headerIDScanner struct {
	mu    sync.Mutex
	seen  int
	line  []byte
	id    string
	extra bool // a second, different id appeared in the header
}

const maxHeaderBytes = 8 << 10

var headerRe = regexp.MustCompile(`^session id: ([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)

func (s *headerIDScanner) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range p {
		if s.seen >= maxHeaderBytes {
			break
		}
		s.seen++
		if c != '\n' {
			s.line = append(s.line, c)
			continue
		}
		if m := headerRe.FindSubmatch(bytes.TrimRight(s.line, "\r")); m != nil {
			id := string(m[1])
			if s.id == "" {
				s.id = id
			} else if s.id != id {
				s.extra = true
			}
		}
		s.line = s.line[:0]
	}
	return len(p), nil
}

func (s *headerIDScanner) result() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id, s.extra
}

// runChild runs codex with stdio passed through. In exec mode output is also
// scanned for the session id; interactive mode keeps the real TTY.
func runChild(opts Options, args []string, dir string, stdin io.Reader, scan bool) (code int, capt captureResult, interrupted bool, startErr error) {
	cmd := exec.Command(opts.CodexBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DONEGUARD_WRAPPER=1")
	cmd.Stdin = stdin
	js, hs := &jsonIDScanner{}, &headerIDScanner{}
	if scan {
		cmd.Stdout = io.MultiWriter(opts.Stdout, js)
		cmd.Stderr = io.MultiWriter(opts.Stderr, hs)
	} else {
		cmd.Stdout, cmd.Stderr = opts.Stdout, opts.Stderr
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		return 127, capt, false, err
	}
	done := make(chan struct{})
	var gotSignal bool
	var mu sync.Mutex
	go func() {
		for {
			select {
			case s := <-sigs:
				mu.Lock()
				gotSignal = true
				mu.Unlock()
				// The terminal already delivers SIGINT to the child's process
				// group; forward anything else (e.g. SIGTERM from a supervisor).
				if s != os.Interrupt && cmd.Process != nil {
					_ = cmd.Process.Signal(s)
				}
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	code = 0
	if err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
			if code < 0 {
				code = 130
				interrupted = true
			}
		}
	}
	mu.Lock()
	if gotSignal || code == 130 {
		interrupted = true
	}
	mu.Unlock()
	if scan {
		jid := js.ID()
		hid, extra := hs.result()
		switch {
		case extra || (jid != "" && hid != "" && jid != hid):
			capt.conflict = true
		case jid != "":
			capt.id = jid
		default:
			capt.id = hid
		}
	}
	return code, capt, interrupted, nil
}

// codexUsage finds the rollout file for sessionID and parses token usage.
func codexUsage(sessionID string, getenv func(string) string) *transcript.Usage {
	if sessionID == "" {
		return nil
	}
	home := getenv("CODEX_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		home = filepath.Join(h, ".codex")
	}
	var found string
	_ = filepath.WalkDir(filepath.Join(home, "sessions"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.Contains(d.Name(), sessionID) && strings.HasSuffix(d.Name(), ".jsonl") {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	if found == "" {
		return nil
	}
	u, err := transcript.ParseFile(found)
	if err != nil {
		return nil
	}
	return u
}
