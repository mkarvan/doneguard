package hook

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mkarvan/doneguard/internal/config"
	"github.com/mkarvan/doneguard/internal/state"
)

// markerConfig is a config whose verify command leaves a marker file, so a
// test can prove whether verify commands ran.
func markerConfig(marker string) string {
	return "verify:\n  - command: \"touch " + marker + "\"\n    timeout: 10s\nprotected:\n  - \".env\"\n"
}

func notExists(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err == nil {
		t.Errorf("%s exists: verify commands must not have run", p)
	}
}

func TestDeletedConfigBlocksWithoutParentFallback(t *testing.T) {
	dir := project(t, "")
	sub := filepath.Join(dir, "svc")
	// The parent config would pass; the subproject's config is the owner.
	writeFile(t, filepath.Join(dir, ".doneguard.yml"), "verify:\n  - \"true\"\n")
	writeFile(t, filepath.Join(sub, ".doneguard.yml"), markerConfig(filepath.Join(dir, "RAN")))
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "configs")
	os.Remove(filepath.Join(dir, "RAN"))

	if err := os.Remove(filepath.Join(sub, ".doneguard.yml")); err != nil {
		t.Fatal(err)
	}
	d, out, _ := handle(t, payload(t, claudeStop(sub, false)))
	r := decode(t, out)["reason"]
	if !d.Block || !strings.Contains(r, ".doneguard.yml (deleted or renamed)") || !strings.Contains(r, "restore it") {
		t.Fatalf("deleting the committed config must block: %q", out)
	}
	if d.Entry.BlockKind != "protected" || !d.Entry.FalseDone() {
		t.Errorf("entry = %+v", d.Entry)
	}
	notExists(t, filepath.Join(dir, "RAN"))
	if _, err := os.Stat(filepath.Join(sub, ".doneguard", "state")); err != nil {
		t.Errorf("state should live with the subproject: %v", err)
	}
}

func TestRenamedConfigBlocks(t *testing.T) {
	dir := project(t, "")
	gitRun(t, dir, "mv", ".doneguard.yml", ".doneguard.yaml")
	writeFile(t, filepath.Join(dir, ".doneguard.yaml"), "verify: []\n")
	d, out, _ := handle(t, payload(t, claudeStop(dir, false)))
	r := decode(t, out)["reason"]
	if !d.Block || !strings.Contains(r, ".doneguard.yml (deleted or renamed)") || !strings.Contains(r, ".doneguard.yaml (uncommitted config") {
		t.Fatalf("renaming the config must block: %q", out)
	}
	// Plain rename to a non-config name.
	dir = project(t, "")
	if err := os.Rename(filepath.Join(dir, ".doneguard.yml"), filepath.Join(dir, "old.yml")); err != nil {
		t.Fatal(err)
	}
	if d, out, _ := handle(t, payload(t, claudeStop(dir, false))); !d.Block || !strings.Contains(out, "deleted or renamed") {
		t.Fatalf("rename away must block: %q", out)
	}
}

func TestCommittedConfigDeletionWithRuntimeDataBlocks(t *testing.T) {
	dir := project(t, "")
	if d, _, _ := handle(t, payload(t, claudeStop(dir, false))); !d.Block {
		t.Fatal("setup: expected block") // creates .doneguard/state
	}
	gitRun(t, dir, "rm", "-q", ".doneguard.yml")
	gitRun(t, dir, "commit", "-q", "-m", "drop config")
	d, out, _ := handle(t, payload(t, claudeStop(dir, true)))
	if !d.Block || !strings.Contains(out, "missing; .doneguard/ data shows it was in use") {
		t.Fatalf("a config that vanished must block: %q", out)
	}
}

func TestNearerUncommittedConfigCannotShadow(t *testing.T) {
	dir := project(t, "")
	sub := filepath.Join(dir, "pkg")
	writeFile(t, filepath.Join(sub, ".doneguard.yml"), markerConfig(filepath.Join(dir, "RAN")))
	d, out, _ := handle(t, payload(t, claudeStop(sub, false)))
	r := decode(t, out)["reason"]
	if !d.Block || !strings.Contains(r, "pkg/.doneguard.yml (uncommitted config shadowing the committed one)") {
		t.Fatalf("shadowing config must block: %q", out)
	}
	notExists(t, filepath.Join(dir, "RAN"))
	// The committed root config owns the data dir.
	if _, err := os.Stat(filepath.Join(sub, ".doneguard")); err == nil {
		t.Error("shadow config must not get its own data dir")
	}
	// Same directory, alternate spelling.
	dir = project(t, "")
	writeFile(t, filepath.Join(dir, ".doneguard.yaml"), "verify: []\n")
	if d, _, _ := handle(t, payload(t, claudeStop(dir, false))); !d.Block {
		t.Fatal("uncommitted .doneguard.yaml next to the committed .yml must block")
	}
}

func TestModifiedConfigNeverRunsVerify(t *testing.T) {
	dir := project(t, "max_retries: 2\n")
	marker := filepath.Join(dir, "RAN")
	// The agent rewrites the policy to an always-passing command and audit
	// mode; neither may take effect.
	writeFile(t, filepath.Join(dir, ".doneguard.yml"), markerConfig(marker)+"max_retries: 0\n")
	for i := 1; i <= 2; i++ {
		d, out, _ := handle(t, payload(t, claudeStop(dir, i > 1)))
		if !d.Block || !strings.Contains(out, ".doneguard.yml (modified)") || !strings.Contains(out, "Verify commands were not run") {
			t.Fatalf("attempt %d: modified config must block: %q", i, out)
		}
	}
	notExists(t, marker)
	// The committed max_retries (2) applies, not the edited 0.
	d, _, _ := handle(t, payload(t, claudeStop(dir, true)))
	if d.Block || !d.Entry.Unverified || d.Entry.MaxRetries != 2 {
		t.Fatalf("cap from committed config: %+v", d.Entry)
	}
	notExists(t, marker)

	// Check() alone (used by the wrapper) also refuses to run verify.
	res := Check(mustLoad(t, filepath.Join(dir, ".doneguard.yml")), "")
	if res.VerifyRan || !res.PolicyTampered() {
		t.Fatalf("Check ran verify on a tampered policy: %+v", res)
	}
	notExists(t, marker)
}

func TestGitFailureIsIndeterminateNotAllow(t *testing.T) {
	dir := project(t, "")
	writeFile(t, filepath.Join(dir, "PASS"), "")
	gitRun(t, dir, "add", "PASS")
	gitRun(t, dir, "commit", "-q", "-m", "pass")
	// Corrupt the index: `git status` fails, `rev-parse` still works.
	writeFile(t, filepath.Join(dir, ".git", "index"), "garbage")
	d, out, _ := handle(t, payload(t, claudeStop(dir, false)))
	r := decode(t, out)["reason"]
	if !d.Block || !strings.Contains(r, "git error") || d.Entry.BlockKind != "indeterminate" {
		t.Fatalf("git failure must block as indeterminate: %q (%+v)", out, d.Entry)
	}
	if d.Entry.FalseDone() {
		t.Error("indeterminate checks are not false-dones")
	}
}

func TestMissingBaselineCommitIsIndeterminate(t *testing.T) {
	dir := project(t, "")
	writeFile(t, filepath.Join(dir, "PASS"), "")
	gitRun(t, dir, "add", "PASS")
	gitRun(t, dir, "commit", "-q", "-m", "pass")
	writeFile(t, filepath.Join(dir, "x.txt"), "x")
	gitRun(t, dir, "add", "x.txt")
	gitRun(t, dir, "commit", "-q", "-m", "x")
	if err := state.Save(filepath.Join(dir, ".doneguard"), state.State{Key: "sess-1", BaseHead: "0123456789abcdef0123456789abcdef01234567"}); err != nil {
		t.Fatal(err)
	}
	d, out, _ := handle(t, payload(t, claudeStop(dir, false)))
	if !d.Block || !strings.Contains(out, "baseline commit 0123456789ab is no longer available") {
		t.Fatalf("failed commit-range diff must not become an allow: %q", out)
	}
}

func TestProtectedRecheckedAfterVerify(t *testing.T) {
	dir := project(t, "")
	writeFile(t, filepath.Join(dir, ".doneguard.yml"), "verify:\n  - \"echo TOKEN=from-test > .env\"\nprotected:\n  - \".env\"\n")
	gitRun(t, dir, "commit", "-q", "-am", "verify touches .env")
	d, out, _ := handle(t, payload(t, claudeStop(dir, false)))
	if !d.Block || !strings.Contains(decode(t, out)["reason"], ".env (modified, during verify)") {
		t.Fatalf("a protected file touched during verify must block: %q", out)
	}
}

func TestUnverifiedAllowKeepsBaseline(t *testing.T) {
	dir := project(t, "max_retries: 0\n")
	writeFile(t, filepath.Join(dir, "PASS"), "")
	gitRun(t, dir, "add", "PASS")
	gitRun(t, dir, "commit", "-q", "-m", "pass")
	if d, _, _ := handle(t, payload(t, claudeStop(dir, false))); d.Block || d.Entry.Unverified {
		t.Fatalf("clean tree should allow verified: %+v", d.Entry)
	}
	writeFile(t, filepath.Join(dir, ".env"), "TOKEN=leaked\n")
	gitRun(t, dir, "commit", "-q", "-am", "agent commits the secret")
	for i := 0; i < 2; i++ {
		d, _, _ := handle(t, payload(t, claudeStop(dir, false)))
		if !d.Entry.Unverified || len(d.Entry.ProtectedViolations) != 1 {
			t.Fatalf("round %d: committed violation must stay visible after an UNVERIFIED allow: %+v", i, d.Entry)
		}
	}
}

func TestConcurrentHooksCannotExceedCap(t *testing.T) {
	dir := project(t, "max_retries: 3\n")
	const n = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	blocks, unverified := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var stderr bytes.Buffer
			d := Handle(payload(t, claudeStop(dir, true)), Options{Getenv: func(string) string { return "" }, Stderr: &stderr})
			mu.Lock()
			defer mu.Unlock()
			if d.Block {
				blocks++
			} else if d.Entry != nil && d.Entry.Unverified {
				unverified++
			}
		}()
	}
	wg.Wait()
	if blocks != 3 || unverified != n-3 {
		t.Fatalf("blocks=%d unverified=%d; want exactly 3 blocks", blocks, unverified)
	}
}

func TestUnpersistableStateNeverBlocksUncharged(t *testing.T) {
	dir := project(t, "")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".doneguard")); err != nil {
		t.Skip(err)
	}
	d, out, _ := handle(t, payload(t, claudeStop(dir, false)))
	m := decode(t, out)
	if d.Block || !d.Entry.Unverified || !strings.Contains(m["systemMessage"], "retry state unavailable") {
		t.Fatalf("uncharged block emitted: %q", out)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("wrote through a symlink out of the project: %v", entries)
	}
}

func TestMalformedPayloadsNeverCountAsStops(t *testing.T) {
	dir := project(t, "")
	for _, in := range []string{"", "null", "[]", `"Stop"`, "42",
		`{"session_id":"s","cwd":"` + dir + `"}`,
		`{"hook_event_name":"Stop","cwd":"` + dir + `"}`,
		`{"hook_event_name":"Stop","session_id":7,"cwd":"` + dir + `"}`,
	} {
		d, out, code := handle(t, []byte(in))
		if !d.Silent || d.Block || out != "" || code != 0 {
			t.Errorf("input %q: d=%+v out=%q", in, d, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".doneguard", "state")); err == nil {
		t.Error("malformed input must not create retry state")
	}
}

func TestCodexUnverifiedGetsSystemMessage(t *testing.T) {
	for _, exit2 := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		code := (Decision{Source: "codex", SystemMessage: "DoneGuard: UNVERIFIED — x"}).Write(&stdout, &stderr, exit2)
		if code != 0 || !strings.Contains(stdout.String(), `"systemMessage":"DoneGuard: UNVERIFIED`) || strings.Contains(stdout.String(), "decision") {
			t.Errorf("exit2=%v: code=%d stdout=%q", exit2, code, stdout.String())
		}
	}
}

func mustLoad(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
