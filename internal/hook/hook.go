package hook

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mkarvan/doneguard/internal/config"
	"github.com/mkarvan/doneguard/internal/safepath"
	"github.com/mkarvan/doneguard/internal/sessionlog"
	"github.com/mkarvan/doneguard/internal/state"
	"github.com/mkarvan/doneguard/internal/transcript"
)

// Options configures Handle. Zero values use the real process environment.
type Options struct {
	Source  string // "claude" | "codex" | "" (auto-detect)
	Getwd   func() (string, error)
	Getenv  func(string) string
	Now     func() time.Time
	Stderr  io.Writer
	Verbose bool
}

func (o *Options) defaults() {
	if o.Getwd == nil {
		o.Getwd = os.Getwd
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
}

// Decision is what the hook tells the agent.
type Decision struct {
	Block         bool
	Reason        string
	SystemMessage string // shown to the user on UNVERIFIED allows
	Source        string
	// Silent means DoneGuard is not active here (no config, disabled, not a
	// stop event): print nothing and allow.
	Silent bool
	Entry  *sessionlog.Entry
}

const maxLoggedMessage = 500

// reservationSlack is added to the verify timeouts to bound how long an
// invocation can hold a block slot (git checks, transcript parsing).
const reservationSlack = 5 * time.Minute

// Handle processes one hook invocation.
//
// Retry accounting is interprocess-safe: a block slot is reserved under a
// per-key file lock before the checks run, and released (reset) under the
// same lock when the checks pass. If the reservation cannot be persisted
// (lock timeout, unwritable or symlinked state dir) DoneGuard does not emit
// a block it could not charge: a failing stop is allowed as UNVERIFIED
// instead, so retries can never exceed the cap.
func Handle(stdin []byte, opts Options) Decision {
	opts.defaults()
	start := opts.Now()
	logf := func(format string, args ...any) {
		if opts.Verbose {
			fmt.Fprintf(opts.Stderr, "doneguard: "+format+"\n", args...)
		}
	}
	if v := strings.ToLower(opts.Getenv("DONEGUARD_DISABLE")); v == "1" || v == "true" || v == "yes" {
		logf("disabled via DONEGUARD_DISABLE")
		return Decision{Silent: true}
	}
	if opts.Getenv("DONEGUARD_WRAPPER") == "1" {
		// `doneguard run codex` owns verification and retries for its child.
		logf("running under the doneguard wrapper; native hook stays silent")
		return Decision{Silent: true}
	}
	p, err := ParsePayload(stdin)
	if err != nil {
		// Never brick the agent because of unexpected input.
		fmt.Fprintf(opts.Stderr, "doneguard: ignoring hook input: %v\n", err)
		return Decision{Silent: true}
	}
	if p.Event != EventStop && p.Event != EventSubagentStop {
		if p.RawEvent == "" {
			fmt.Fprintln(opts.Stderr, "doneguard: ignoring hook input without hook_event_name")
		} else {
			logf("ignoring event %q", p.RawEvent)
		}
		return Decision{Silent: true}
	}
	if p.SessionID == "" {
		// Without a session id retries cannot be attributed to a chain.
		fmt.Fprintln(opts.Stderr, "doneguard: ignoring stop hook input without session_id")
		return Decision{Silent: true}
	}
	cwd := p.Cwd
	if cwd == "" {
		if cwd, err = opts.Getwd(); err != nil {
			return Decision{Silent: true}
		}
	}
	pol := ResolvePolicy(cwd)
	if !pol.Active() {
		logf("no .doneguard.yml found from %s; allowing stop", cwd)
		return Decision{Silent: true}
	}
	source := resolveSource(opts, p)

	// Load the policy. A config with integrity violations (or an
	// undetermined one) is never loaded from the working tree; limits come
	// from the committed copy when there is one.
	var cfg *config.Config
	var cfgErr error
	cfgName := config.FileNames[0]
	trusted := len(pol.Violations) == 0 && pol.GitErr == nil
	if trusted {
		cfg, cfgErr = pol.Load()
		cfgName = filepath.Base(pol.Path)
	} else if pol.Committed != nil {
		if c, err := config.Parse(pol.Committed, pol.Dir); err == nil {
			cfg = c
		}
	}
	maxRetries := config.DefaultMaxRetries
	outputLimit := config.DefaultOutputLimit
	if cfg != nil {
		maxRetries = cfg.EffectiveMaxRetries()
		outputLimit = cfg.OutputLimit
	}

	entry := &sessionlog.Entry{
		Timestamp:            start.UTC().Format(time.RFC3339),
		SessionID:            p.SessionID,
		AgentID:              p.AgentID,
		HookEventName:        p.Event,
		Source:               source,
		MaxRetries:           maxRetries,
		StopHookActive:       p.StopHookActive,
		LastAssistantMessage: truncate(p.LastAssistantMessage, maxLoggedMessage),
	}

	// Phase 1: reserve a block slot under the per-key lock. The reservation
	// is tied to this invocation (resID) until it settles, so a concurrent
	// passing or fresh stop cannot hand the same slot to another hook.
	key := StateKey(p)
	fresh := p.StopHookActive != nil && !*p.StopHookActive
	dataDir, ddErr := PrepareDataDir(pol.Dir)
	charged := false
	baseHead := ""
	persistNote := ""
	resID := state.NewReservationID()
	ttl := reservationSlack + config.DefaultTimeout
	if cfg != nil {
		ttl = reservationSlack + cfg.TotalTimeout()
	}
	if ddErr != nil {
		persistNote = "retry state unavailable: " + ddErr.Error()
	} else {
		// A fresh stop (new user turn) starts a new retry chain and may
		// replace an unreadable state file. While stop_hook_active is true
		// we rely on our own counter instead of allowing immediately, so the
		// agent gets max_retries tries.
		err := withState(dataDir, key, fresh, func(st *state.State) (bool, error) {
			st.Prune(opts.Now())
			if fresh {
				st.Reset("")
			}
			baseHead = st.BaseHead
			charged = st.Reserve(resID, maxRetries, opts.Now().Add(ttl))
			entry.RetryCount = st.Retries
			return true, nil
		})
		if err != nil {
			charged = false
			persistNote = "could not persist retry state: " + err.Error()
		}
	}

	failing := false
	reason := ""
	head := ""
	baseline := ""
	switch {
	case pol.GitErr != nil:
		failing = true
		res := &CheckResult{GitErr: pol.GitErr.Error()}
		reason = BuildReason(&config.Config{OutputLimit: outputLimit}, res)
		entry.BlockKind = res.Kind()
		entry.Notes = append(entry.Notes, "policy check indeterminate: "+pol.GitErr.Error(), "verify not run")
	case len(pol.Violations) > 0:
		failing = true
		res := &CheckResult{Violations: pol.Violations}
		reason = BuildReason(&config.Config{OutputLimit: outputLimit}, res)
		entry.BlockKind = res.Kind()
		entry.ProtectedViolations = res.ViolationStrings()
		entry.Notes = append(entry.Notes, "verify not run: DoneGuard config failed its integrity check")
	case cfgErr != nil:
		failing = true
		reason = ConfigErrorReason(cfgName, cfgErr)
		entry.BlockKind = "config"
	default:
		res := Check(cfg, baseHead)
		head = res.Head
		baseline = res.Baseline
		entry.VerifyOK = res.VerifyOK()
		entry.VerifySkipped = res.VerifySkipped
		entry.ProtectedViolations = res.ViolationStrings()
		entry.Notes = append(entry.Notes, res.Notes...)
		if res.GitErr != "" {
			entry.Notes = append(entry.Notes, "protected-path check indeterminate: "+res.GitErr)
		}
		if res.Failed != nil {
			entry.FailingCommand = res.Failed.Command
			code := res.Failed.ExitCode
			entry.FailingExitCode = &code
		}
		if !res.OK() {
			failing = true
			reason = BuildReason(cfg, res)
			entry.BlockKind = res.Kind()
		}
	}
	if persistNote != "" {
		entry.Notes = append(entry.Notes, persistNote)
	}

	// Phase 2: settle the reservation.
	d := Decision{Source: source, Entry: entry}
	settle := func(repair bool, fn func(st *state.State) bool) error {
		if ddErr != nil {
			return ddErr
		}
		err := withState(dataDir, key, repair, func(st *state.State) (bool, error) {
			st.Prune(opts.Now())
			return fn(st), nil
		})
		if err != nil {
			fmt.Fprintf(opts.Stderr, "doneguard: could not save state: %v\n", err)
		}
		return err
	}
	keepBaseline := func(st *state.State) {
		// Without a stored baseline, remember the HEAD from before this
		// stop's verify so commits made during it stay visible next time.
		if st.BaseHead == "" && baseline != "" {
			st.BaseHead = baseline
		}
	}
	if failing && charged {
		// Only block while this invocation still holds its reservation.
		held := false
		err := settle(false, func(st *state.State) bool {
			held = st.Holds(resID)
			st.Release(resID)
			keepBaseline(st)
			return true
		})
		if err != nil || !held {
			charged = false
			if err != nil {
				persistNote = "could not settle retry state: " + err.Error()
			} else {
				persistNote = "retry reservation expired before verification finished"
			}
			entry.Notes = append(entry.Notes, persistNote)
		}
	}
	switch {
	case !failing:
		entry.Decision = "allow"
		entry.RetryCount = 0
		_ = settle(true, func(st *state.State) bool {
			st.Reset(resID)
			if head != "" {
				st.BaseHead = head
			}
			return true
		})
	case charged:
		entry.Decision = "block"
		entry.Reason = reason
		d.Block = true
		d.Reason = reason
	default:
		entry.Decision = "allow"
		entry.Unverified = true
		entry.Reason = reason
		if entry.BlockKind == "config" && cfgErr != nil {
			entry.Notes = append(entry.Notes, "config error: "+cfgErr.Error())
		}
		entry.BlockKind = ""
		if persistNote != "" {
			d.SystemMessage = "DoneGuard: UNVERIFIED — " + persistNote + "; allowing stop without blocking. Failure: " + firstLine(reason)
		} else {
			d.SystemMessage = unverifiedMessage(entry.RetryCount, reason)
		}
		// Keep the baseline from the last verified stop: an UNVERIFIED allow
		// must not accept committed violations as the next baseline. When the
		// platform tells us about new turns (stop_hook_active), keep the
		// counter at the cap so racing hooks cannot reopen the chain; the next
		// fresh turn resets it. In-flight reservations of other hooks always
		// stay charged.
		_ = settle(false, func(st *state.State) bool {
			st.Release(resID)
			if p.StopHookActive == nil {
				st.Reset(resID)
			}
			keepBaseline(st)
			return true
		})
	}

	if ddErr == nil {
		writeTelemetry(entry, p, dataDir, start, opts)
	}
	if d.SystemMessage != "" {
		fmt.Fprintln(opts.Stderr, d.SystemMessage)
	}
	return d
}

// withState runs fn on key's state under the interprocess lock and saves it
// when fn reports a change.
//
// An unreadable state file is an error (the chain's count is unknown) unless
// repair is set, in which case a corrupt file is replaced by a fresh state.
func withState(dataDir, key string, repair bool, fn func(st *state.State) (bool, error)) error {
	release, err := state.Lock(dataDir, key)
	if err != nil {
		return err
	}
	defer release()
	st, err := state.Load(dataDir, key)
	if err != nil {
		if !repair || !errors.Is(err, state.ErrCorrupt) {
			return err
		}
		st = state.State{Key: key}
	}
	changed, err := fn(&st)
	if err != nil || !changed {
		return err
	}
	return state.Save(dataDir, st)
}

// maxTranscriptBytes bounds transcript parsing so telemetry cannot stall
// the verdict; larger transcripts are skipped.
const maxTranscriptBytes = 256 << 20

func writeTelemetry(entry *sessionlog.Entry, p *Payload, dataDir string, start time.Time, opts Options) {
	tp := p.TranscriptPath
	if p.Event == EventSubagentStop && p.AgentTranscriptPath != "" {
		tp = p.AgentTranscriptPath
	}
	tp = expandHome(tp, opts.Getenv)
	if st, err := os.Stat(tp); tp != "" && err == nil && st.Mode().IsRegular() && st.Size() <= maxTranscriptBytes {
		if u, err := transcript.ParseFile(tp); err == nil && u != nil {
			entry.InputTokens, entry.OutputTokens, entry.TotalTokens = &u.InputTokens, &u.OutputTokens, &u.TotalTokens
		}
	}
	entry.DurationMS = opts.Now().Sub(start).Milliseconds()
	logPath, err := safepath.Resolve(dataDir, filepath.Join(dataDir, "sessions.jsonl"))
	if err == nil {
		err = sessionlog.Append(logPath, entry)
	}
	if err != nil {
		fmt.Fprintf(opts.Stderr, "doneguard: could not write session log: %v\n", err)
	}
}

// StateKey identifies a retry chain: the session, plus the subagent for
// SubagentStop so parallel subagents do not share a counter.
func StateKey(p *Payload) string {
	sid := p.SessionID
	if sid == "" {
		sid = "unknown-session"
	}
	if p.Event == EventSubagentStop {
		agent := p.AgentID
		if agent == "" {
			agent = p.AgentType
		}
		if agent == "" {
			agent = "subagent"
		}
		return sid + "|subagent|" + agent
	}
	return sid
}

func resolveSource(opts Options, p *Payload) string {
	if opts.Source != "" {
		return opts.Source
	}
	if p.Source != "" {
		return p.Source
	}
	if opts.Getenv("CLAUDE_PROJECT_DIR") != "" || opts.Getenv("CLAUDECODE") != "" {
		return "claude"
	}
	if opts.Getenv("CODEX_THREAD_ID") != "" || opts.Getenv("CODEX_SANDBOX") != "" || opts.Getenv("CODEX_MANAGED_BY_NPM") != "" {
		return "codex"
	}
	return "claude"
}

func firstLine(reason string) string {
	first := reason
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	return strings.TrimPrefix(first, "DoneGuard blocked stop: ")
}

func unverifiedMessage(retries int, reason string) string {
	first := firstLine(reason)
	if retries == 0 {
		return "DoneGuard: UNVERIFIED (audit mode, max_retries: 0) — " + first
	}
	return fmt.Sprintf("DoneGuard: UNVERIFIED — still failing after %d blocked stop attempt(s); allowing stop. Last failure: %s", retries, first)
}

// PrepareDataDir creates <project>/.doneguard (self-gitignored) and returns
// its real path. It refuses symlinks that lead outside the project, so a
// checkout cannot redirect DoneGuard's writes elsewhere.
func PrepareDataDir(projectDir string) (string, error) {
	dd, err := safepath.Resolve(projectDir, filepath.Join(projectDir, config.DataDirName))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dd, 0o700); err != nil {
		return "", err
	}
	stateDir, err := safepath.Resolve(dd, filepath.Join(dd, "state"))
	if err != nil {
		return "", err
	}
	if stateDir != filepath.Join(dd, "state") {
		return "", fmt.Errorf("%s must not be a symlink", filepath.Join(dd, "state"))
	}
	gi := filepath.Join(dd, ".gitignore")
	if _, err := os.Lstat(gi); os.IsNotExist(err) {
		if f, err := os.OpenFile(gi, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err == nil {
			_, _ = f.WriteString("# DoneGuard runtime data (session logs, retry state)\n*\n")
			f.Close()
		}
	}
	return dd, nil
}

func expandHome(p string, getenv func(string) string) string {
	if strings.HasPrefix(p, "~/") {
		if home := getenv("HOME"); home != "" {
			return filepath.Join(home, p[2:])
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
