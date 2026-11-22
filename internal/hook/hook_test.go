package hook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mkarvan/doneguard/internal/sessionlog"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
		"GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// project creates a git repo whose verify command passes iff PASS exists.
func project(t *testing.T, extraConfig string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fixtures use sh")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	writeFile(t, filepath.Join(dir, ".doneguard.yml"), `verify:
  - command: "test -f PASS || { echo 'add_test.go:8: expected 5, got 4'; echo FAIL; exit 1; }"
    timeout: 10s
protected:
  - ".env"
  - "**/*.pem"
`+extraConfig)
	writeFile(t, filepath.Join(dir, ".env"), "TOKEN=original\n")
	writeFile(t, filepath.Join(dir, "main.txt"), "hello\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func payload(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func claudeStop(dir string, active bool) map[string]any {
	return map[string]any{
		"session_id":             "sess-1",
		"transcript_path":        filepath.Join(dir, "no-transcript.jsonl"),
		"cwd":                    dir,
		"permission_mode":        "default",
		"hook_event_name":        "Stop",
		"stop_hook_active":       active,
		"last_assistant_message": "All done! Tests pass.",
	}
}

func handle(t *testing.T, in []byte) (Decision, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	d := Handle(in, Options{Getenv: func(string) string { return "" }, Stderr: &stderr})
	code := d.Write(&stdout, &stderr, false)
	return d, stdout.String(), code
}

func decode(t *testing.T, out string) map[string]string {
	t.Helper()
	if strings.TrimSpace(out) == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("stdout is not JSON: %q (%v)", out, err)
	}
	return m
}

func readLog(t *testing.T, dir string) []sessionlog.Entry {
	t.Helper()
	entries, _, err := sessionlog.ReadFile(filepath.Join(dir, ".doneguard", "sessions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestClaudeStopBlocksThenAllows(t *testing.T) {
	dir := project(t, "")
	d, out, code := handle(t, payload(t, claudeStop(dir, false)))
	if code != 0 || !d.Block {
		t.Fatalf("expected block, code=%d d=%+v", code, d)
	}
	m := decode(t, out)
	if m["decision"] != "block" || !strings.Contains(m["reason"], "add_test.go:8: expected 5, got 4") {
		t.Fatalf("stdout = %q", out)
	}
	if !strings.HasPrefix(m["reason"], "DoneGuard blocked stop: `test -f PASS") || !strings.Contains(m["reason"], "failed (exit 1)") {
		t.Errorf("reason = %q", m["reason"])
	}
	if _, ok := m["continue"]; ok {
		t.Error("must not send continue")
	}

	writeFile(t, filepath.Join(dir, "PASS"), "")
	d, out, _ = handle(t, payload(t, claudeStop(dir, true)))
	if d.Block || out != "" {
		t.Fatalf("expected silent allow, got %q", out)
	}
	entries := readLog(t, dir)
	if len(entries) != 2 {
		t.Fatalf("entries = %d", len(entries))
	}
	e0, e1 := entries[0], entries[1]
	if e0.Decision != "block" || e0.Source != "claude" || e0.BlockKind != "verify" || e0.VerifyOK == nil || *e0.VerifyOK || e0.RetryCount != 1 || e0.FailingCommand == "" {
		t.Errorf("entry0 = %+v", e0)
	}
	if e0.FailingExitCode == nil || *e0.FailingExitCode != 1 || e0.LastAssistantMessage != "All done! Tests pass." {
		t.Errorf("entry0 = %+v", e0)
	}
	if e1.Decision != "allow" || e1.Unverified || e1.VerifyOK == nil || !*e1.VerifyOK {
		t.Errorf("entry1 = %+v", e1)
	}
	// The data dir must be self-ignoring so it never shows up as a change.
	if _, err := os.Stat(filepath.Join(dir, ".doneguard", ".gitignore")); err != nil {
		t.Error(err)
	}
}

func TestSubagentStopBlocksWithSeparateCounter(t *testing.T) {
	dir := project(t, "max_retries: 1\n")
	sub := map[string]any{
		"session_id": "sess-1", "cwd": dir, "hook_event_name": "SubagentStop",
		"stop_hook_active": false, "agent_id": "agent-a", "agent_type": "general-purpose",
		"agent_transcript_path": filepath.Join(dir, "agent.jsonl"),
	}
	d, out, _ := handle(t, payload(t, sub))
	if !d.Block || decode(t, out)["decision"] != "block" {
		t.Fatalf("subagent stop should block: %q", out)
	}
	// The main session still has its own budget.
	d, _, _ = handle(t, payload(t, claudeStop(dir, false)))
	if !d.Block {
		t.Fatal("main Stop should block independently of subagent counter")
	}
	sub["stop_hook_active"] = true
	d, _, _ = handle(t, payload(t, sub))
	if d.Block || !d.Entry.Unverified {
		t.Fatalf("subagent should hit cap 1: %+v", d)
	}
	if d.Entry.HookEventName != "SubagentStop" || d.Entry.AgentID != "agent-a" {
		t.Errorf("entry = %+v", d.Entry)
	}
}

func TestCodexStopPayload(t *testing.T) {
	dir := project(t, "")
	in := payload(t, map[string]any{
		"session_id": "019a-codex", "turn_id": "turn-1", "cwd": dir,
		"hook_event_name": "Stop", "stop_hook_active": false, "model": "gpt-6",
		"transcript_path":        filepath.Join(dir, ".codex", "sessions", "rollout-x.jsonl"),
		"last_assistant_message": "Implemented the feature.",
	})
	d, out, code := handle(t, in)
	m := decode(t, out)
	if code != 0 || !d.Block || m["decision"] != "block" || !strings.Contains(m["reason"], "expected 5, got 4") {
		t.Fatalf("codex block: code=%d out=%q", code, out)
	}
	if d.Entry.Source != "codex" {
		t.Errorf("source = %q", d.Entry.Source)
	}
	writeFile(t, filepath.Join(dir, "PASS"), "")
	if d, out, _ := handle(t, in); d.Block || out != "" {
		t.Fatalf("codex allow: %q", out)
	}
}

func TestCodexCamelCaseFields(t *testing.T) {
	dir := project(t, "")
	in := payload(t, map[string]any{"sessionId": "c1", "cwd": dir, "hookEventName": "stop", "stopHookActive": false})
	var stderr bytes.Buffer
	d := Handle(in, Options{Source: "codex", Getenv: func(string) string { return "" }, Stderr: &stderr})
	if !d.Block || d.Entry.SessionID != "c1" || d.Entry.Source != "codex" {
		t.Fatalf("d = %+v", d)
	}
}

func TestRetryCapAllowsUnverified(t *testing.T) {
	dir := project(t, "max_retries: 2\n")
	for i, active := range []bool{false, true} {
		d, _, _ := handle(t, payload(t, claudeStop(dir, active)))
		if !d.Block || d.Entry.RetryCount != i+1 {
			t.Fatalf("attempt %d: expected block, got %+v", i+1, d.Entry)
		}
	}
	d, out, code := handle(t, payload(t, claudeStop(dir, true)))
	if d.Block || code != 0 {
		t.Fatalf("cap should allow: %+v", d)
	}
	if !d.Entry.Unverified || d.Entry.Decision != "allow" || d.Entry.RetryCount != 2 || d.Entry.Reason == "" {
		t.Errorf("entry = %+v", d.Entry)
	}
	m := decode(t, out)
	if !strings.Contains(m["systemMessage"], "UNVERIFIED") {
		t.Errorf("expected UNVERIFIED systemMessage, got %q", out)
	}
	if _, ok := m["decision"]; ok {
		t.Error("allow must not carry a decision")
	}
	// A new user turn (stop_hook_active=false) starts a fresh chain.
	d, _, _ = handle(t, payload(t, claudeStop(dir, false)))
	if !d.Block || d.Entry.RetryCount != 1 {
		t.Fatalf("fresh turn should block again: %+v", d.Entry)
	}
}

func TestStopHookActiveDoesNotShortCircuit(t *testing.T) {
	dir := project(t, "")
	// First invocation already has stop_hook_active=true (e.g. another Stop
	// hook blocked earlier). DoneGuard must still block, not allow blindly.
	d, out, _ := handle(t, payload(t, claudeStop(dir, true)))
	if !d.Block || decode(t, out)["decision"] != "block" {
		t.Fatalf("stop_hook_active=true must not allow immediately: %q", out)
	}
	d, _, _ = handle(t, payload(t, claudeStop(dir, true)))
	if !d.Block || d.Entry.RetryCount != 2 {
		t.Fatalf("second continuation should still block: %+v", d.Entry)
	}
}

func TestNeverExceedsSevenConsecutiveBlocks(t *testing.T) {
	dir := project(t, "max_retries: 50\n")
	blocks := 0
	for i := 0; i < 12; i++ {
		d, _, _ := handle(t, payload(t, claudeStop(dir, i > 0)))
		if !d.Block {
			if !d.Entry.Unverified {
				t.Fatalf("allow without unverified at %d", i)
			}
			break
		}
		blocks++
	}
	if blocks != 7 {
		t.Fatalf("blocks in a row = %d, want 7 (under the platform cap of 8)", blocks)
	}
}

func TestAuditModeMaxRetriesZero(t *testing.T) {
	dir := project(t, "max_retries: 0\n")
	d, _, _ := handle(t, payload(t, claudeStop(dir, false)))
	if d.Block || !d.Entry.Unverified || !d.Entry.CheckFailed() {
		t.Fatalf("audit mode should allow+log unverified: %+v", d.Entry)
	}
}

func TestProtectedFileBlocksEvenWhenTestsPass(t *testing.T) {
	dir := project(t, "")
	writeFile(t, filepath.Join(dir, "PASS"), "")
	writeFile(t, filepath.Join(dir, ".env"), "TOKEN=changed\n")
	writeFile(t, filepath.Join(dir, "certs", "new.pem"), "key") // untracked
	d, out, _ := handle(t, payload(t, claudeStop(dir, false)))
	m := decode(t, out)
	if !d.Block || m["decision"] != "block" {
		t.Fatalf("expected block: %q", out)
	}
	r := m["reason"]
	if !strings.Contains(r, "protected files touched: .env (modified), certs/new.pem (untracked)") || !strings.Contains(r, "Revert the change") {
		t.Errorf("reason = %q", r)
	}
	if strings.Contains(r, "Also:") {
		t.Errorf("tests pass; reason should only mention protected files: %q", r)
	}
	if d.Entry.BlockKind != "protected" || len(d.Entry.ProtectedViolations) != 2 || d.Entry.VerifyOK == nil || !*d.Entry.VerifyOK {
		t.Errorf("entry = %+v", d.Entry)
	}
}

func TestProtectedAndFailingTestsBothReported(t *testing.T) {
	dir := project(t, "")
	if err := os.Remove(filepath.Join(dir, ".env")); err != nil {
		t.Fatal(err)
	}
	_, out, _ := handle(t, payload(t, claudeStop(dir, false)))
	r := decode(t, out)["reason"]
	if !strings.Contains(r, ".env (deleted)") || !strings.Contains(r, "Also: `test -f PASS") {
		t.Errorf("reason = %q", r)
	}
}

func TestCommittedProtectedChangeDetectedAfterBase(t *testing.T) {
	dir := project(t, "")
	writeFile(t, filepath.Join(dir, "PASS"), "")
	gitRun(t, dir, "add", "PASS")
	gitRun(t, dir, "commit", "-q", "-m", "pass")
	if d, _, _ := handle(t, payload(t, claudeStop(dir, false))); d.Block {
		t.Fatal("clean tree should allow (records base HEAD)")
	}
	writeFile(t, filepath.Join(dir, ".env"), "TOKEN=leaked\n")
	gitRun(t, dir, "commit", "-q", "-am", "agent commits the secret")
	d, out, _ := handle(t, payload(t, claudeStop(dir, false)))
	if !d.Block || !strings.Contains(decode(t, out)["reason"], ".env (committed)") {
		t.Fatalf("committed protected change should block: %q", out)
	}
}

func TestTrackedConfigIsImplicitlyProtected(t *testing.T) {
	dir := project(t, "")
	writeFile(t, filepath.Join(dir, ".doneguard.yml"), "verify: []\n")
	d, out, _ := handle(t, payload(t, claudeStop(dir, false)))
	if !d.Block || !strings.Contains(decode(t, out)["reason"], ".doneguard.yml (modified)") {
		t.Fatalf("editing committed config should block: %q", out)
	}
}

func TestEmptyVerifyAllowsWhenProtectedClean(t *testing.T) {
	dir := project(t, "")
	writeFile(t, filepath.Join(dir, ".doneguard.yml"), "verify: []\nprotected: ['.env']\n")
	gitRun(t, dir, "commit", "-q", "-am", "no verify")
	d, out, _ := handle(t, payload(t, claudeStop(dir, false)))
	if d.Block || out != "" {
		t.Fatalf("expected allow: %q", out)
	}
	if !d.Entry.VerifySkipped || d.Entry.VerifyOK != nil {
		t.Errorf("entry = %+v", d.Entry)
	}
	if len(d.Entry.Notes) == 0 || !strings.Contains(d.Entry.Notes[0], "verify skipped") {
		t.Errorf("notes = %v", d.Entry.Notes)
	}
	writeFile(t, filepath.Join(dir, ".env"), "x\n")
	if d, _, _ := handle(t, payload(t, claudeStop(dir, false))); !d.Block {
		t.Fatal("protected check must still run with empty verify")
	}
}

func TestNoConfigAllowsSilently(t *testing.T) {
	dir := t.TempDir()
	d, out, code := handle(t, payload(t, claudeStop(dir, false)))
	if !d.Silent || d.Block || out != "" || code != 0 {
		t.Fatalf("d=%+v out=%q", d, out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".doneguard")); err == nil {
		t.Error("must not create .doneguard without a config")
	}
}

func TestInvalidConfigBlocksWithParseError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".doneguard.yml"), "verify:\n  - command: ok\nmax_retries: many\n")
	d, out, _ := handle(t, payload(t, claudeStop(dir, false)))
	r := decode(t, out)["reason"]
	if !d.Block || !strings.Contains(r, ".doneguard.yml is invalid") || !strings.Contains(r, "line 3") {
		t.Fatalf("out = %q", out)
	}
	if d.Entry.BlockKind != "config" || d.Entry.CheckFailed() {
		t.Errorf("config errors are not false-dones: %+v", d.Entry)
	}
}

func TestNoGitRepoStillRunsVerify(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	writeFile(t, filepath.Join(dir, ".doneguard.yml"), "verify:\n  - exit 4\n")
	d, _, _ := handle(t, payload(t, claudeStop(dir, false)))
	if !d.Block || !strings.Contains(d.Reason, "failed (exit 4)") {
		t.Fatalf("d = %+v", d)
	}
	found := false
	for _, n := range d.Entry.Notes {
		if strings.Contains(n, "not a git repository") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v", d.Entry.Notes)
	}
}

func TestDisabledMalformedAndOtherEvents(t *testing.T) {
	dir := project(t, "")
	var stderr bytes.Buffer
	d := Handle(payload(t, claudeStop(dir, false)), Options{Getenv: func(k string) string {
		if k == "DONEGUARD_DISABLE" {
			return "1"
		}
		return ""
	}, Stderr: &stderr})
	if !d.Silent {
		t.Error("DONEGUARD_DISABLE should allow silently")
	}
	if d, _, _ := handle(t, []byte("not json")); !d.Silent || d.Block {
		t.Error("malformed input must not block")
	}
	if d, _, _ := handle(t, payload(t, map[string]any{"cwd": dir, "hook_event_name": "PreToolUse"})); !d.Silent {
		t.Error("non-stop events are ignored")
	}
}

func TestTokensLoggedFromTranscript(t *testing.T) {
	dir := project(t, "")
	tp := filepath.Join(dir, "transcript.jsonl")
	writeFile(t, filepath.Join(dir, ".git", "info", "exclude"), "transcript.jsonl\n")
	writeFile(t, tp, `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":100,"cache_read_input_tokens":900,"output_tokens":50}}}`+"\n")
	p := claudeStop(dir, false)
	p["transcript_path"] = tp
	d, _, _ := handle(t, payload(t, p))
	e := d.Entry
	if e.InputTokens == nil || *e.InputTokens != 1000 || *e.OutputTokens != 50 || *e.TotalTokens != 1050 {
		t.Fatalf("tokens = %v %v %v", e.InputTokens, e.OutputTokens, e.TotalTokens)
	}
}

func TestExit2Fallback(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Decision{Block: true, Reason: "nope"}.Write(&stdout, &stderr, true)
	if code != 2 || stdout.Len() != 0 || strings.TrimSpace(stderr.String()) != "nope" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := (Decision{}).Write(&stdout, &stderr, false); code != 0 || stdout.Len() != 0 {
		t.Fatal("allow must be silent exit 0")
	}
	// HTML-ish characters stay readable.
	stdout.Reset()
	(Decision{Block: true, Reason: "revert <file> && retry"}).Write(&stdout, &stderr, false)
	if !strings.Contains(stdout.String(), "<file> &&") {
		t.Errorf("escaped: %s", stdout.String())
	}
}

func TestNormalizeEventAndStateKey(t *testing.T) {
	for in, want := range map[string]string{"Stop": EventStop, "stop": EventStop, "": "", "SubagentStop": EventSubagentStop, "subagent_stop": EventSubagentStop, "PostToolUse": "PostToolUse"} {
		if got := NormalizeEvent(in); got != want {
			t.Errorf("NormalizeEvent(%q) = %q", in, got)
		}
	}
	p := &Payload{SessionID: "s", Event: EventSubagentStop, AgentID: "a"}
	if StateKey(p) != "s|subagent|a" {
		t.Errorf("key = %q", StateKey(p))
	}
	if StateKey(&Payload{Event: EventStop}) != "unknown-session" {
		t.Error("empty session key")
	}
}

func TestOutputLimitRespected(t *testing.T) {
	dir := project(t, "output_limit: 500\n")
	writeFile(t, filepath.Join(dir, ".doneguard.yml"), fmt.Sprintf("verify:\n  - command: %q\noutput_limit: 500\n",
		"i=0; while [ $i -lt 3000 ]; do echo line $i; i=$((i+1)); done; echo 'FAIL: the real reason'; exit 1"))
	gitRun(t, dir, "commit", "-q", "-am", "cfg")
	d, _, _ := handle(t, payload(t, claudeStop(dir, false)))
	if !d.Block || len(d.Reason) > 900 || !strings.Contains(d.Reason, "FAIL: the real reason") {
		t.Fatalf("reason (%d bytes) = %q", len(d.Reason), d.Reason)
	}
}
