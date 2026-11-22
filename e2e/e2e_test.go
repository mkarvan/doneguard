// Package e2e builds the doneguard binary and drives it the way Claude Code
// and Codex do: realistic hook JSON on stdin, decision JSON on stdout.
// Everything is offline and deterministic (no real agents, no network).
package e2e

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
)

var binPath string

func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" {
		fmt.Println("e2e: skipped on windows")
		os.Exit(0)
	}
	tmp, err := os.MkdirTemp("", "doneguard-e2e-bin")
	if err != nil {
		panic(err)
	}
	binPath = filepath.Join(tmp, "doneguard")
	build := exec.Command(goBin(), "build", "-o", binPath, "./cmd/doneguard")
	build.Dir = ".."
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Printf("building doneguard failed: %v\n%s", err, out)
		os.RemoveAll(tmp)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

func goBin() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

func testEnv(extra ...string) []string {
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@example.invalid",
		"GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@example.invalid",
		"GIT_CONFIG_NOSYSTEM=1", "GOTOOLCHAIN=local", "GOPROXY=off", "GOFLAGS=-mod=mod",
		"CGO_ENABLED=0",
	)
	return append(env, extra...)
}

func run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = testEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const buggyAdd = "package sample\n\n// Add returns a+b.\nfunc Add(a, b int) int { return a + b - 1 }\n"
const fixedAdd = "package sample\n\n// Add returns a+b.\nfunc Add(a, b int) int { return a + b }\n"
const addTest = `package sample

import "testing"

func TestAdd(t *testing.T) {
	if got := Add(2, 3); got != 5 {
		t.Fatalf("expected 5, got %d", got)
	}
}
`

// sampleRepo creates a tiny Go repo with a failing test, a protected .env
// and a committed .doneguard.yml.
func sampleRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q")
	write(t, filepath.Join(dir, "go.mod"), "module sample\n\ngo 1.21\n")
	write(t, filepath.Join(dir, "add.go"), buggyAdd)
	write(t, filepath.Join(dir, "add_test.go"), addTest)
	write(t, filepath.Join(dir, ".env"), "API_KEY=do-not-touch\n")
	write(t, filepath.Join(dir, ".doneguard.yml"), fmt.Sprintf(`# DoneGuard config
verify:
  - command: %q
    timeout: 120s
protected:
  - ".env"
  - ".env.*"
  - "**/*secret*"
  - "**/*.pem"
max_retries: 3
`, goBin()+" test ./..."))
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-q", "-m", "sample")
	return dir
}

type hookResult struct {
	stdout, stderr string
	code           int
	decision       map[string]any
}

func runHook(t *testing.T, dir string, payload map[string]any, args ...string) hookResult {
	t.Helper()
	data, _ := json.Marshal(payload)
	cmd := exec.Command(binPath, append([]string{"hook"}, args...)...)
	cmd.Dir = t.TempDir() // the hook must use payload cwd, not process cwd
	cmd.Env = testEnv()
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := hookResult{stdout: stdout.String(), stderr: stderr.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if s := strings.TrimSpace(res.stdout); s != "" {
		if err := json.Unmarshal([]byte(s), &res.decision); err != nil {
			t.Fatalf("hook stdout is not a single JSON object: %q", res.stdout)
		}
	}
	return res
}

func claudeStop(dir, transcript string, active bool) map[string]any {
	return map[string]any{
		"session_id":             "8d2c5c1e-0f7a-4c55-9f3e-0b0c7b1d2e3f",
		"transcript_path":        transcript,
		"cwd":                    dir,
		"permission_mode":        "acceptEdits",
		"hook_event_name":        "Stop",
		"stop_hook_active":       active,
		"last_assistant_message": "I've fixed the Add function and all tests pass. The task is complete!",
	}
}

func writeTranscript(t *testing.T, dir string, outputTokens int) string {
	t.Helper()
	p := filepath.Join(dir, "transcripts", "session.jsonl")
	line := fmt.Sprintf(`{"type":"assistant","message":{"id":"msg_%d","role":"assistant","usage":{"input_tokens":1200,"cache_read_input_tokens":800,"output_tokens":%d}}}`, outputTokens, outputTokens)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if os.IsNotExist(err) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		f, err = os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	}
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, line)
	f.Close()
	return p
}

func TestClaudeStopBlocksFailingTestThenAllowsAfterFix(t *testing.T) {
	dir := sampleRepo(t)
	tdir := t.TempDir()
	tp := writeTranscript(t, tdir, 300)

	// 1. Agent claims done with a failing test → blocked.
	r := runHook(t, dir, claudeStop(dir, tp, false))
	if r.code != 0 {
		t.Fatalf("exit code %d, stderr: %s", r.code, r.stderr)
	}
	if r.decision["decision"] != "block" {
		t.Fatalf("expected block, got %q", r.stdout)
	}
	reason, _ := r.decision["reason"].(string)
	for _, want := range []string{"DoneGuard blocked stop:", "test ./...` failed (exit 1)", "TestAdd", "expected 5, got 4", "Fix the failing tests"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason missing %q:\n%s", want, reason)
		}
	}
	if _, ok := r.decision["continue"]; ok {
		t.Error("must not send continue:false when blocking")
	}

	// 2. Agent fixes the bug (stop_hook_active now true) → allowed.
	write(t, filepath.Join(dir, "add.go"), fixedAdd)
	tp = writeTranscript(t, tdir, 500)
	r = runHook(t, dir, claudeStop(dir, tp, true))
	if r.code != 0 || strings.TrimSpace(r.stdout) != "" {
		t.Fatalf("expected silent allow, got code=%d stdout=%q stderr=%q", r.code, r.stdout, r.stderr)
	}

	// 3. The report reflects one false-done and the tokens spent on it.
	cmd := exec.Command(binPath, "report", "--json")
	cmd.Dir = filepath.Join(dir)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if rep["stop_attempts"].(float64) != 2 || rep["false_done"].(float64) != 1 || rep["allowed_verified"].(float64) != 1 {
		t.Errorf("report = %s", out)
	}
	if rep["tokens_on_false_done"].(float64) != 2300 || rep["tokens_total"].(float64) != 4800 {
		t.Errorf("token accounting = %v / %v", rep["tokens_on_false_done"], rep["tokens_total"])
	}

	// The session log is valid JSONL with the documented fields.
	data, err := os.ReadFile(filepath.Join(dir, ".doneguard", "sessions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var first map[string]any
	json.Unmarshal([]byte(lines[0]), &first)
	for _, k := range []string{"timestamp", "session_id", "hook_event_name", "source", "decision", "reason", "verify_ok", "failing_command", "protected_violations", "retry_count", "max_retries", "unverified", "input_tokens", "output_tokens", "total_tokens", "last_assistant_message"} {
		if _, ok := first[k]; !ok {
			t.Errorf("log entry missing %q: %s", k, lines[0])
		}
	}
	// .doneguard/ never shows up as a change in git.
	if st := run(t, dir, "git", "status", "--porcelain"); strings.Contains(st, ".doneguard/") {
		t.Errorf("git status shows .doneguard/: %s", st)
	}
}

func TestProtectedFileBlockedEvenWhenTestsPass(t *testing.T) {
	dir := sampleRepo(t)
	write(t, filepath.Join(dir, "add.go"), fixedAdd)
	write(t, filepath.Join(dir, ".env"), "API_KEY=agent-was-here\n")
	r := runHook(t, dir, claudeStop(dir, "", false))
	if r.decision["decision"] != "block" {
		t.Fatalf("expected block, got %q", r.stdout)
	}
	reason := r.decision["reason"].(string)
	if !strings.Contains(reason, "protected file touched: .env") || !strings.Contains(reason, "Revert the change") {
		t.Errorf("reason = %q", reason)
	}
	if strings.Contains(reason, "failed (exit") {
		t.Errorf("tests pass; reason should not mention test failure: %q", reason)
	}
	// An untracked new secret file is caught too.
	run(t, dir, "git", "checkout", "--", ".env")
	write(t, filepath.Join(dir, "config", "db_secret.json"), "{}")
	r = runHook(t, dir, claudeStop(dir, "", false))
	if r.decision["decision"] != "block" || !strings.Contains(r.decision["reason"].(string), "config/db_secret.json (untracked)") {
		t.Fatalf("untracked secret: %q", r.stdout)
	}
	os.RemoveAll(filepath.Join(dir, "config"))
	if r = runHook(t, dir, claudeStop(dir, "", false)); strings.TrimSpace(r.stdout) != "" {
		t.Fatalf("clean tree should allow: %q", r.stdout)
	}
}

func TestSubagentStopAndCodexStop(t *testing.T) {
	dir := sampleRepo(t)
	sub := map[string]any{
		"session_id": "8d2c5c1e", "transcript_path": "", "cwd": dir, "hook_event_name": "SubagentStop",
		"stop_hook_active": false, "agent_id": "a-1", "agent_type": "Explore",
		"agent_transcript_path": "", "last_assistant_message": "Subtask done.",
	}
	if r := runHook(t, dir, sub, "--source", "claude"); r.decision["decision"] != "block" {
		t.Fatalf("SubagentStop: %q", r.stdout)
	}
	codex := map[string]any{
		"session_id": "019a1f0d-ef69-7ef1-a0b1-1d799ac31cb1", "turn_id": "019a1f0d-f462", "cwd": dir,
		"hook_event_name": "Stop", "model": "gpt-6", "stop_hook_active": false,
		"transcript_path":        filepath.Join(t.TempDir(), "rollout-2026-11-22T00-00-00-019a1f0d.jsonl"),
		"last_assistant_message": "Done.",
	}
	r := runHook(t, dir, codex, "--source", "codex")
	if r.code != 0 || r.decision["decision"] != "block" || !strings.Contains(r.decision["reason"].(string), "expected 5, got 4") {
		t.Fatalf("Codex Stop: code=%d %q", r.code, r.stdout)
	}
	// Exit-2 fallback convention.
	r = runHook(t, dir, codex, "--source", "codex", "--exit2")
	if r.code != 2 || strings.TrimSpace(r.stdout) != "" || !strings.Contains(r.stderr, "expected 5, got 4") {
		t.Fatalf("exit2: code=%d stdout=%q", r.code, r.stdout)
	}
}

func TestRetryCapThroughBinary(t *testing.T) {
	dir := sampleRepo(t)
	write(t, filepath.Join(dir, ".doneguard.yml"), "verify:\n  - command: \"echo FAIL: still broken; exit 1\"\nmax_retries: 2\n")
	run(t, dir, "git", "commit", "-q", "-am", "cfg")
	for i := 0; i < 2; i++ {
		if r := runHook(t, dir, claudeStop(dir, "", i > 0)); r.decision["decision"] != "block" {
			t.Fatalf("attempt %d should block: %q", i+1, r.stdout)
		}
	}
	r := runHook(t, dir, claudeStop(dir, "", true))
	if r.decision["decision"] != nil || !strings.Contains(fmt.Sprint(r.decision["systemMessage"]), "UNVERIFIED") {
		t.Fatalf("cap should allow with UNVERIFIED message: %q", r.stdout)
	}
	out := run(t, dir, binPath, "report")
	// False-done counts the two blocked attempts; the cap-exhausted
	// allow is UNVERIFIED, not a third false-done.
	if !strings.Contains(out, "UNVERIFIED 1") || !strings.Contains(out, "False-done:           2 of 3") {
		t.Errorf("report:\n%s", out)
	}
}

func TestNoConfigNeverBricksUnrelatedProjects(t *testing.T) {
	dir := t.TempDir()
	r := runHook(t, dir, claudeStop(dir, "", false))
	if r.code != 0 || r.stdout != "" {
		t.Fatalf("code=%d stdout=%q", r.code, r.stdout)
	}
	cmd := exec.Command(binPath, "hook")
	cmd.Stdin = strings.NewReader("this is not json")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil || len(out) != 0 {
		t.Fatalf("garbage stdin: err=%v out=%q", err, out)
	}
}

func TestInitInstallAndRegisteredCommandWorks(t *testing.T) {
	dir := sampleRepo(t)
	os.Remove(filepath.Join(dir, ".doneguard.yml"))
	run(t, dir, "git", "commit", "-q", "-am", "drop cfg")
	write(t, filepath.Join(dir, ".claude", "settings.json"), `{"model":"opus","hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo other"}]}]}}`)

	out := run(t, dir, binPath, "init")
	if !strings.Contains(out, "go test ./...") {
		t.Fatalf("init output: %s", out)
	}
	if out := run(t, dir, binPath, "install"); !strings.Contains(out, ".claude/settings.json") || !strings.Contains(out, ".codex/hooks.json") {
		t.Fatalf("install output: %s", out)
	}
	if out := run(t, dir, binPath, "install"); strings.Count(out, "unchanged") != 2 {
		t.Fatalf("second install not idempotent: %s", out)
	}

	var settings struct {
		Model string `json:"model"`
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	data, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Model != "opus" || len(settings.Hooks["Stop"]) != 2 || settings.Hooks["Stop"][0].Hooks[0].Command != "echo other" {
		t.Fatalf("settings clobbered: %s", data)
	}
	registered := settings.Hooks["Stop"][1].Hooks[0].Command
	if !strings.HasPrefix(registered, binPath) {
		t.Fatalf("registered command %q should use the absolute binary path", registered)
	}

	// Run the exact registered command the way the agent would (sh -c).
	payload, _ := json.Marshal(claudeStop(dir, "", false))
	cmd := exec.Command("/bin/sh", "-c", registered)
	cmd.Dir = dir
	cmd.Env = testEnv()
	cmd.Stdin = bytes.NewReader(payload)
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal(stdout, &d); err != nil || d["decision"] != "block" {
		t.Fatalf("registered hook output = %q (%v)", stdout, err)
	}

	if out := run(t, dir, binPath, "uninstall"); !strings.Contains(out, "removed   "+filepath.Join(dir, ".claude", "settings.json")) || !strings.Contains(out, "removed   "+filepath.Join(dir, ".codex", "hooks.json")) {
		t.Fatalf("uninstall: %s", out)
	}
	data, _ = os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if strings.Contains(string(data), "doneguard") || !strings.Contains(string(data), "echo other") {
		t.Fatalf("after uninstall: %s", data)
	}
}

func TestRunCodexWrapperWithFakeCodex(t *testing.T) {
	dir := sampleRepo(t)
	bin := t.TempDir()
	fake := filepath.Join(bin, "codex")
	fixed := filepath.Join(bin, "fixed_add.go.txt")
	write(t, fixed, fixedAdd)
	// The fake agent "fixes" the bug only when resumed with DoneGuard's prompt.
	// It reports its thread the way `codex exec --json` does and records its
	// cwd, so the test can check the resume targets the pinned session.
	script := fmt.Sprintf(`#!/bin/sh
echo '{"type":"thread.started","thread_id":"019a1f0d-ef69-7ef1-a0b1-1d799ac31cb1"}'
pwd >> %q
case "$*" in
  *" resume 019a1f0d-ef69-7ef1-a0b1-1d799ac31cb1 "*"expected 5, got 4"*) cp %q add.go; echo "fixed it";;
  *) echo "all done (not really)";;
esac
`, filepath.Join(bin, "cwds"), fixed)
	write(t, fake, script)
	os.Chmod(fake, 0o755)
	cmd := exec.Command(binPath, "run", "codex", "--", "exec", "--json", "fix Add")
	cmd.Dir = dir
	cmd.Env = testEnv("DONEGUARD_CODEX_BIN="+fake, "CODEX_HOME="+t.TempDir())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("wrapper failed: %v\nstdout:%s\nstderr:%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "all done (not really)") || !strings.Contains(stdout.String(), "fixed it") {
		t.Errorf("child stdout not passed through: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "verified ✓") {
		t.Errorf("stderr: %s", stderr.String())
	}
	cwds, _ := os.ReadFile(filepath.Join(bin, "cwds"))
	if strings.Count(string(cwds), "\n") != 2 {
		t.Errorf("codex runs: %q", cwds)
	}
}

func TestConcurrentHookProcessesRespectCap(t *testing.T) {
	dir := sampleRepo(t)
	write(t, filepath.Join(dir, ".doneguard.yml"), "verify:\n  - command: \"echo FAIL; exit 1\"\nmax_retries: 3\n")
	run(t, dir, "git", "commit", "-q", "-am", "cfg")
	const n = 10
	results := make(chan hookResult, n)
	for i := 0; i < n; i++ {
		go func() { results <- runHook(t, dir, claudeStop(dir, "", true)) }()
	}
	blocks, unverified := 0, 0
	for i := 0; i < n; i++ {
		r := <-results
		switch {
		case r.decision["decision"] == "block":
			blocks++
		case strings.Contains(fmt.Sprint(r.decision["systemMessage"]), "UNVERIFIED"):
			unverified++
		default:
			t.Errorf("unexpected hook result: code=%d stdout=%q stderr=%q", r.code, r.stdout, r.stderr)
		}
	}
	if blocks != 3 || unverified != n-3 {
		t.Fatalf("racing hook processes: blocks=%d unverified=%d; want exactly 3 blocks", blocks, unverified)
	}
}
