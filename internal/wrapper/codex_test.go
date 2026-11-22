package wrapper

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mkarvan/doneguard/internal/sessionlog"
)

const fakeSession = "019a1f0d-ef69-7ef1-a0b1-1d799ac31cb1"
const otherSession = "019a1f0d-0000-7ef1-a0b1-1d799ac31cb2"

// call is one recorded fake-codex invocation.
type call struct {
	argv []string
	cwd  string
}

// fakeCodex writes a script that records each invocation's exact argv
// (NUL-separated, so argument boundaries are preserved) and cwd. It reports
// its session the way codex exec does: a thread.started JSON event on
// stdout with --json, otherwise a "session id:" header line on stderr.
// Behaviour is tuned with files next to the script:
//
//	fix_on_resume — create PASS in the project when resumed with sessionFile's id
//	session       — id to report (default fakeSession)
//	exit_code     — exit status
func fakeCodex(t *testing.T, dir string, fixOnResume bool) (bin, callLog string) {
	t.Helper()
	callLog = filepath.Join(dir, "calls.log")
	if fixOnResume {
		os.WriteFile(filepath.Join(dir, "fix_on_resume"), nil, 0o644)
	}
	script := `#!/bin/sh
D="` + dir + `"
SID="` + fakeSession + `"
[ -f "$D/session" ] && SID=$(cat "$D/session")
{ printf 'CWD=%s\0' "$(pwd)"; for a in "$@"; do printf '%s\0' "$a"; done; printf '\001\n'; } >> "$D/calls.log"
json=0; resumed=0; prev=""
for a in "$@"; do
  [ "$a" = "--json" ] && json=1
  [ "$prev" = "resume" ] && [ "$a" = "` + fakeSession + `" ] && resumed=1
  prev="$a"
done
if [ $json = 1 ]; then
  echo '{"type":"thread.started","thread_id":"'"$SID"'"}'
else
  echo "OpenAI Codex (fake)" >&2
  echo "--------" >&2
  echo "session id: $SID" >&2
  echo "--------" >&2
fi
echo "agent: I am done! (mentions session id: ` + otherSession + ` in passing)"
if [ $resumed = 1 ] && [ -f "$D/fix_on_resume" ]; then touch "$D/proj/PASS"; fi
[ -f "$D/exit_code" ] && exit $(cat "$D/exit_code")
exit 0
`
	bin = filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, callLog
}

func project(t *testing.T, dir, cfg string) string {
	t.Helper()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, ".doneguard.yml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return proj
}

func calls(t *testing.T, path string) []call {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []call
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\x01\n"), "\x01\n") {
		fields := strings.Split(strings.TrimSuffix(line, "\x00"), "\x00")
		out = append(out, call{cwd: strings.TrimPrefix(fields[0], "CWD="), argv: fields[1:]})
	}
	return out
}

func skipIfNoSh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake codex is a shell script")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
}

func realDir(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const failingUntilPass = "verify:\n  - command: \"test -f PASS || { echo 'TestAdd: expected 5, got 4'; exit 1; }\"\nmax_retries: 3\n"

func TestWrapperResumesPinnedSessionUntilVerified(t *testing.T) {
	skipIfNoSh(t)
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codexhome"))
	proj := project(t, dir, failingUntilPass)
	bin, callLog := fakeCodex(t, dir, true)
	var stdout, stderr bytes.Buffer
	code := Run(Options{
		CodexBin: bin,
		Args:     []string{"exec", "--model", "gpt-6", "-s", "workspace-write", "-p", "work", "--add-dir", "/extra", "--json", "-i", "x.png", "fix the bug"},
		Stdout:   &stdout, Stderr: &stderr,
		Getwd: func() (string, error) { return proj, nil },
	})
	if code != 0 {
		t.Fatalf("code = %d\nstderr:\n%s", code, stderr.String())
	}
	cs := calls(t, callLog)
	if len(cs) != 2 {
		t.Fatalf("calls = %q", cs)
	}
	if got := strings.Join(cs[0].argv, "|"); got != "exec|--model|gpt-6|-s|workspace-write|-p|work|--add-dir|/extra|--json|-i|x.png|fix the bug" {
		t.Errorf("first call argv = %q", got)
	}
	want := []string{"exec", "--model", "gpt-6", "-s", "workspace-write", "-p", "work", "--add-dir", "/extra", "--json", "-C", proj, "resume", fakeSession}
	got := cs[1].argv
	if len(got) != len(want)+1 || strings.Join(got[:len(want)], "|") != strings.Join(want, "|") {
		t.Fatalf("resume argv = %q\nwant prefix %q", got, want)
	}
	prompt := got[len(got)-1]
	if !strings.HasPrefix(prompt, "DoneGuard verification failed") || !strings.Contains(prompt, "TestAdd: expected 5, got 4") {
		t.Errorf("resume prompt = %q", prompt)
	}
	for i, c := range cs {
		if realDir(t, c.cwd) != realDir(t, proj) {
			t.Errorf("call %d cwd = %s, want %s", i, c.cwd, proj)
		}
	}
	if !strings.Contains(stdout.String(), "agent: I am done!") || !strings.Contains(stdout.String(), "thread.started") {
		t.Error("child output must be passed through")
	}
	if !strings.Contains(stderr.String(), "verified ✓") {
		t.Errorf("stderr = %s", stderr.String())
	}
	entries, _, err := sessionlog.ReadFile(filepath.Join(proj, ".doneguard", "sessions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Decision != "block" || entries[1].Decision != "allow" || entries[0].Source != "wrapper" || entries[0].SessionID != fakeSession {
		t.Errorf("entries = %+v", entries)
	}
}

func TestWrapperPreservesCdAndUsesHeaderID(t *testing.T) {
	skipIfNoSh(t)
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	proj := project(t, dir, failingUntilPass)
	bin, callLog := fakeCodex(t, dir, true)
	var stdout, stderr bytes.Buffer
	// Started from the parent directory with -C pointing at the project.
	code := Run(Options{CodexBin: bin, Args: []string{"exec", "-C", "proj", "do it"}, Stdout: &stdout, Stderr: &stderr,
		Getwd: func() (string, error) { return dir, nil }})
	if code != 0 {
		t.Fatalf("code = %d\n%s", code, stderr.String())
	}
	cs := calls(t, callLog)
	if len(cs) != 2 {
		t.Fatalf("calls = %q", cs)
	}
	want := "exec|-C|" + proj + "|resume|" + fakeSession
	if got := strings.Join(cs[1].argv[:5], "|"); got != want {
		t.Errorf("resume argv = %q, want %q", got, want)
	}
	// The session-like text in the agent's stdout must not be picked up.
	if strings.Contains(strings.Join(cs[1].argv, " "), otherSession) {
		t.Error("resumed an id from agent output")
	}
}

func TestWrapperNeverFallsBackToLast(t *testing.T) {
	skipIfNoSh(t)
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	proj := project(t, dir, failingUntilPass)
	bin, callLog := fakeCodex(t, dir, true)
	os.WriteFile(filepath.Join(dir, "session"), []byte("not-a-uuid"), 0o644)
	var stderr bytes.Buffer
	code := Run(Options{CodexBin: bin, Args: []string{"exec", "--json", "do it"}, Stdout: &bytes.Buffer{}, Stderr: &stderr,
		Getwd: func() (string, error) { return proj, nil }})
	if code != ExitUnverified {
		t.Fatalf("code = %d, want UNVERIFIED\n%s", code, stderr.String())
	}
	cs := calls(t, callLog)
	if len(cs) != 1 {
		t.Fatalf("must not resume without a pinned id: %q", cs)
	}
	if !strings.Contains(stderr.String(), "could not capture the codex session id") {
		t.Errorf("stderr = %s", stderr.String())
	}
	for _, c := range cs {
		for _, a := range c.argv {
			if a == "--last" {
				t.Fatal("--last used")
			}
		}
	}
}

func TestWrapperStopsWhenResumedSessionChanges(t *testing.T) {
	skipIfNoSh(t)
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	proj := project(t, dir, "verify:\n  - exit 1\nmax_retries: 3\n")
	bin, callLog := fakeCodex(t, dir, false)
	// From the second resume on, codex reports a different session.
	var stderr bytes.Buffer
	Run(Options{CodexBin: bin, Args: []string{"exec", "do it"}, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Getwd: func() (string, error) { return proj, nil }})
	os.Remove(callLog)
	os.Remove(filepath.Join(proj, ".doneguard", "sessions.jsonl"))
	script, _ := os.ReadFile(bin)
	os.WriteFile(bin, bytes.Replace(script, []byte(`[ -f "$D/exit_code" ]`), []byte(`[ "$prev" != "do it" ] && echo `+otherSession+` > "$D/session"; [ -f "$D/exit_code" ]`), 1), 0o755)
	code := Run(Options{CodexBin: bin, Args: []string{"exec", "do it"}, Stdout: &bytes.Buffer{}, Stderr: &stderr,
		Getwd: func() (string, error) { return proj, nil }})
	if code != ExitUnverified || len(calls(t, callLog)) != 3 || !strings.Contains(stderr.String(), "expected "+fakeSession) {
		t.Fatalf("code=%d calls=%d stderr=%s", code, len(calls(t, callLog)), stderr.String())
	}
}

func TestWrapperEphemeralVerifiesOnce(t *testing.T) {
	skipIfNoSh(t)
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	proj := project(t, dir, failingUntilPass)
	bin, callLog := fakeCodex(t, dir, true)
	var stderr bytes.Buffer
	code := Run(Options{CodexBin: bin, Args: []string{"exec", "--ephemeral", "do it"}, Stdout: &bytes.Buffer{}, Stderr: &stderr,
		Getwd: func() (string, error) { return proj, nil }})
	if code != ExitUnverified || len(calls(t, callLog)) != 1 || !strings.Contains(stderr.String(), "--ephemeral sessions cannot be resumed") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestWrapperInteractiveDoesNotResume(t *testing.T) {
	skipIfNoSh(t)
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	proj := project(t, dir, failingUntilPass)
	bin, callLog := fakeCodex(t, dir, true)
	var stderr bytes.Buffer
	code := Run(Options{CodexBin: bin, Args: []string{"build it"}, Stdout: &bytes.Buffer{}, Stderr: &stderr,
		Getwd: func() (string, error) { return proj, nil }})
	if code != ExitUnverified || len(calls(t, callLog)) != 1 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	// An explicit `resume <id>` is a pinned identity and can be retried.
	os.Remove(callLog)
	os.Remove(filepath.Join(dir, "fix_on_resume"))
	project(t, dir, "verify:\n  - exit 1\nmax_retries: 1\n")
	code = Run(Options{CodexBin: bin, Args: []string{"-m", "gpt-6", "resume", fakeSession, "continue"}, Stdout: &bytes.Buffer{}, Stderr: &stderr,
		Getwd: func() (string, error) { return proj, nil }})
	cs := calls(t, callLog)
	if code != ExitUnverified || len(cs) != 2 || strings.Join(cs[1].argv[:6], "|") != "-m|gpt-6|-C|"+proj+"|resume|"+fakeSession {
		t.Fatalf("explicit resume: code=%d calls=%q", code, cs)
	}
}

func TestWrapperFailedCodexRunIsNotATurn(t *testing.T) {
	skipIfNoSh(t)
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	proj := project(t, dir, failingUntilPass)
	bin, callLog := fakeCodex(t, dir, true)
	os.WriteFile(filepath.Join(dir, "exit_code"), []byte("1"), 0o644)
	var stderr bytes.Buffer
	code := Run(Options{CodexBin: bin, Args: []string{"exec", "do it"}, Stdout: &bytes.Buffer{}, Stderr: &stderr,
		Getwd: func() (string, error) { return proj, nil }})
	if code != 1 || len(calls(t, callLog)) != 1 || !strings.Contains(stderr.String(), "turn did not complete") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(proj, ".doneguard", "sessions.jsonl")); err == nil {
		t.Error("a failed codex run must not be logged as a stop attempt")
	}
}

func TestWrapperUnknownOptionDoesNotResume(t *testing.T) {
	skipIfNoSh(t)
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	proj := project(t, dir, failingUntilPass)
	bin, callLog := fakeCodex(t, dir, true)
	var stderr bytes.Buffer
	code := Run(Options{CodexBin: bin, Args: []string{"exec", "--brand-new-flag", "v", "do it"}, Stdout: &bytes.Buffer{}, Stderr: &stderr,
		Getwd: func() (string, error) { return proj, nil }})
	if code != ExitUnverified || len(calls(t, callLog)) != 1 || !strings.Contains(stderr.String(), "unrecognised codex option --brand-new-flag") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestWrapperCapExitsUnverified(t *testing.T) {
	skipIfNoSh(t)
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	proj := project(t, dir, "verify:\n  - exit 1\nmax_retries: 2\n")
	bin, callLog := fakeCodex(t, dir, false)
	var stdout, stderr bytes.Buffer
	code := Run(Options{CodexBin: bin, Args: []string{"exec", "do it"}, Stdout: &stdout, Stderr: &stderr,
		Getwd: func() (string, error) { return proj, nil }})
	if code != ExitUnverified {
		t.Fatalf("code = %d", code)
	}
	cs := calls(t, callLog)
	if len(cs) != 3 {
		t.Errorf("codex runs = %d, want 1 + 2 retries", len(cs))
	}
	for _, c := range cs[1:] {
		if strings.Join(c.argv[:5], "|") != "exec|-C|"+proj+"|resume|"+fakeSession {
			t.Errorf("retry argv = %q", c.argv)
		}
	}
	if !strings.Contains(stderr.String(), "UNVERIFIED") {
		t.Errorf("stderr = %s", stderr.String())
	}
	entries, _, _ := sessionlog.ReadFile(filepath.Join(proj, ".doneguard", "sessions.jsonl"))
	if len(entries) != 3 || !entries[2].Unverified || entries[2].RetryCount != 2 {
		t.Errorf("entries = %+v", entries)
	}
}

func TestWrapperWithoutConfigPassesThrough(t *testing.T) {
	skipIfNoSh(t)
	dir := t.TempDir()
	bin, callLog := fakeCodex(t, dir, false)
	var stdout, stderr bytes.Buffer
	code := Run(Options{CodexBin: bin, Args: []string{"exec", "hi"}, Stdout: &stdout, Stderr: &stderr,
		Getwd: func() (string, error) { return dir, nil }})
	if code != 0 || len(calls(t, callLog)) != 1 || !strings.Contains(stderr.String(), "no .doneguard.yml") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestWrapperNonTaskCommandsPassThrough(t *testing.T) {
	skipIfNoSh(t)
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	proj := project(t, dir, "verify:\n  - exit 1\n")
	bin, callLog := fakeCodex(t, dir, false)
	for _, args := range [][]string{{"--version"}, {"login"}, {"exec", "--help"}} {
		os.Remove(callLog)
		var stderr bytes.Buffer
		code := Run(Options{CodexBin: bin, Args: args, Stdout: &bytes.Buffer{}, Stderr: &stderr,
			Getwd: func() (string, error) { return proj, nil }})
		if code != 0 || len(calls(t, callLog)) != 1 || strings.Contains(stderr.String(), "verifying") {
			t.Errorf("%v: code=%d stderr=%s", args, code, stderr.String())
		}
	}
}

func TestWrapperMissingBinary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	project(t, dir, "verify: []\n")
	var stderr bytes.Buffer
	code := Run(Options{CodexBin: filepath.Join(dir, "nope"), Args: []string{"exec", "x"}, Stdout: &bytes.Buffer{}, Stderr: &stderr,
		Getwd: func() (string, error) { return filepath.Join(dir, "proj"), nil }})
	if code != 127 || !strings.Contains(stderr.String(), "could not start") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestParseArgsAndResume(t *testing.T) {
	p := parseArgs([]string{"exec", "-C", "sub", "--config=model=\"o3\"", "--dangerously-bypass-hook-trust", "-p", "work", "--ephemeral", "task"})
	if !p.isExec || p.cd != "sub" || !p.ephemeral || p.unsupported != "" {
		t.Fatalf("plan = %+v", p)
	}
	p.absCd = "/abs/sub"
	got := strings.Join(resumeArgs(p, fakeSession, "PROMPT"), " ")
	want := `exec --config model="o3" --dangerously-bypass-hook-trust -p work -C /abs/sub resume ` + fakeSession + ` PROMPT`
	if got != want {
		t.Errorf("resume = %q, want %q", got, want)
	}
	p = parseArgs([]string{"-s", "workspace-write", "-a", "never", "--search", "-i", "a.png", "build it"})
	got = strings.Join(resumeArgs(p, "abc", "P"), " ")
	if p.isExec || got != "-s workspace-write -a never --search resume abc P" {
		t.Errorf("interactive resume = %q", got)
	}
	if p := parseArgs([]string{"exec", "resume", "--last", "go on"}); p.unsupported == "" {
		t.Error("resume --last has no pinned identity")
	}
	if p := parseArgs([]string{"exec", "resume", fakeSession, "go on"}); p.explicitID != fakeSession {
		t.Errorf("explicit id = %q", p.explicitID)
	}
}

func TestIDScanners(t *testing.T) {
	s := &jsonIDScanner{}
	s.Write([]byte(`{"type":"thread.started","thread_id":"`))
	s.Write([]byte(fakeSession + `"}` + "\n" + `{"type":"thread.started","thread_id":"` + otherSession + `"}` + "\n"))
	if s.ID() != fakeSession {
		t.Errorf("json id = %q", s.ID())
	}
	// Only the first event counts.
	s = &jsonIDScanner{}
	s.Write([]byte(`{"type":"item.completed","text":"thread_id ` + fakeSession + `"}` + "\n"))
	if s.ID() != "" {
		t.Errorf("non-structured id accepted: %q", s.ID())
	}
	h := &headerIDScanner{}
	h.Write([]byte("header\nsession id: " + fakeSession + "\n"))
	if id, extra := h.result(); id != fakeSession || extra {
		t.Errorf("header id = %q extra=%v", id, extra)
	}
	// Mid-line mentions and anything past the header window are ignored.
	h = &headerIDScanner{}
	h.Write([]byte("agent said session id: " + otherSession + "\n"))
	h.Write(bytes.Repeat([]byte("x"), 10000))
	h.Write([]byte("\nsession id: " + fakeSession + "\n"))
	if id, _ := h.result(); id != "" {
		t.Errorf("header id = %q", id)
	}
}

func TestWrapperResumeLastWithUUIDPromptDoesNotResume(t *testing.T) {
	skipIfNoSh(t)
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	proj := project(t, dir, failingUntilPass)
	bin, callLog := fakeCodex(t, dir, true)
	// Codex treats the UUID-shaped positional as the prompt for the most
	// recent session; it is not a session id.
	for _, args := range [][]string{
		{"resume", "--last", fakeSession},
		{"exec", "resume", "--last", fakeSession},
	} {
		os.Remove(callLog)
		var stderr bytes.Buffer
		code := Run(Options{CodexBin: bin, Args: args, Stdout: &bytes.Buffer{}, Stderr: &stderr,
			Getwd: func() (string, error) { return proj, nil }})
		if code != ExitUnverified || len(calls(t, callLog)) != 1 || !strings.Contains(stderr.String(), "--last") {
			t.Fatalf("%q: code=%d calls=%q stderr=%s", args, code, calls(t, callLog), stderr.String())
		}
	}
	if p := parseArgs([]string{"resume", "--last", fakeSession}); p.explicitID != "" || p.unsupported == "" {
		t.Errorf("plan = %+v", p)
	}
}

func TestParseArgsRootOptionsBeforeExecAndAttachedValues(t *testing.T) {
	p := parseArgs([]string{"-c", "model=o3", "exec", "-C/abs/sub", "-mgpt-6", "task"})
	if !p.isExec || p.cd != "/abs/sub" || p.unsupported != "" || p.passthrough {
		t.Fatalf("plan = %+v", p)
	}
	p.absCd = p.cd
	got := strings.Join(resumeArgs(p, fakeSession, "P"), " ")
	want := "-c model=o3 exec -m gpt-6 -C /abs/sub resume " + fakeSession + " P"
	if got != want {
		t.Errorf("resume = %q, want %q", got, want)
	}
	if p := parseArgs([]string{"-C=/x", "build it"}); p.cd != "/x" || p.isExec {
		t.Errorf("attached -C=: %+v", p)
	}
}
