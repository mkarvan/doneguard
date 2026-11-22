package hook

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mkarvan/doneguard/internal/state"
)

// commitConfig replaces the project's committed .doneguard.yml.
func commitConfig(t *testing.T, dir, cfg string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, ".doneguard.yml"), cfg)
	gitRun(t, dir, "commit", "-q", "-am", "config")
}

func TestPolicyLoadUsesCommittedBytesNotWorkTree(t *testing.T) {
	dir := project(t, "")
	committed := filepath.Join(dir, "COMMITTED")
	commitConfig(t, dir, markerConfig(committed))
	pol := ResolvePolicy(dir)
	if !pol.FromCommit || len(pol.Violations) != 0 || pol.GitErr != nil {
		t.Fatalf("policy = %+v", pol)
	}
	// The file is replaced after the integrity check (TOCTOU).
	writeFile(t, filepath.Join(dir, ".doneguard.yml"), markerConfig(filepath.Join(dir, "INJECTED")))
	cfg, err := pol.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Verify) != 1 || !strings.Contains(cfg.Verify[0].Command, "COMMITTED") || cfg.Path != filepath.Join(dir, ".doneguard.yml") {
		t.Fatalf("executed policy is not the committed one: %+v", cfg)
	}
}

func TestCommittedSymlinkPolicyBlocks(t *testing.T) {
	dir := project(t, "")
	writeFile(t, filepath.Join(dir, "real.yml"), markerConfig(filepath.Join(dir, "RAN1")))
	os.Remove(filepath.Join(dir, ".doneguard.yml"))
	if err := os.Symlink("real.yml", filepath.Join(dir, ".doneguard.yml")); err != nil {
		t.Skip(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "symlinked policy")
	for i, target := range []string{"RAN1", "RAN2"} {
		// The target changes; the symlink itself stays clean.
		writeFile(t, filepath.Join(dir, "real.yml"), markerConfig(filepath.Join(dir, target)))
		if i == 1 {
			gitRun(t, dir, "commit", "-q", "-am", "retarget")
		}
		d, out, _ := handle(t, payload(t, claudeStop(dir, i > 0)))
		if !d.Block || !strings.Contains(decode(t, out)["reason"], ".doneguard.yml (symlink") {
			t.Fatalf("round %d: symlinked policy must block: %q", i, out)
		}
		notExists(t, filepath.Join(dir, target))
	}
}

func TestMissingGitBinaryIsIndeterminate(t *testing.T) {
	dir := project(t, "")
	marker := filepath.Join(dir, "RAN")
	commitConfig(t, dir, markerConfig(marker))
	t.Setenv("PATH", t.TempDir())
	d, out, _ := handle(t, payload(t, claudeStop(dir, false)))
	if !d.Block || d.Entry.BlockKind != "indeterminate" || !strings.Contains(out, "git executable not found") {
		t.Fatalf("missing git must block as indeterminate: %q (%+v)", out, d.Entry)
	}
	notExists(t, marker)

	// Config gone too: runtime data still marks the project as governed.
	os.Remove(filepath.Join(dir, ".doneguard.yml"))
	if d, out, _ := handle(t, payload(t, claudeStop(dir, true))); !d.Block || d.Entry.BlockKind != "indeterminate" {
		t.Fatalf("missing git + missing config with runtime data must block: %q", out)
	}
}

func TestCommittedNestedDeletionDoesNotFallBackToParent(t *testing.T) {
	dir := project(t, "")
	sub := filepath.Join(dir, "svc")
	parentRan := filepath.Join(dir, "PARENT_RAN")
	writeFile(t, filepath.Join(dir, ".doneguard.yml"), markerConfig(parentRan))
	writeFile(t, filepath.Join(sub, ".doneguard.yml"), "verify:\n  - \"true\"\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "configs")
	if d, out, _ := handle(t, payload(t, claudeStop(sub, false))); d.Block {
		t.Fatalf("setup: %q", out) // creates svc/.doneguard/state
	}
	gitRun(t, dir, "rm", "-q", "svc/.doneguard.yml")
	gitRun(t, dir, "commit", "-q", "-m", "drop nested policy")
	d, out, _ := handle(t, payload(t, claudeStop(sub, false)))
	if !d.Block || !strings.Contains(out, "missing; .doneguard/ data shows it was in use") {
		t.Fatalf("deleted nested policy must block, not fall back to the parent: %q", out)
	}
	notExists(t, parentRan)
}

func TestVerifyCommitDuringFirstStopIsSeen(t *testing.T) {
	dir := project(t, "")
	commitConfig(t, dir, "verify:\n  - \"if [ ! -f DONE ]; then echo leaked > .env && git add .env && git -c user.name=t -c user.email=t@example.invalid commit -qm leak && touch DONE; fi\"\nprotected:\n  - \".env\"\n")
	d, out, _ := handle(t, payload(t, claudeStop(dir, false)))
	if !d.Block || !strings.Contains(decode(t, out)["reason"], ".env (committed, during verify)") {
		t.Fatalf("a protected commit made by verify must block: %q", out)
	}
	// The baseline from before that verify is kept, so the next stop (clean
	// tree, verify now a no-op) still sees the commit.
	d, out, _ = handle(t, payload(t, claudeStop(dir, true)))
	if !d.Block || !strings.Contains(decode(t, out)["reason"], ".env (committed)") {
		t.Fatalf("second stop must still see the committed protected change: %q", out)
	}
}

func TestRepoDeletedDuringVerifyIsIndeterminate(t *testing.T) {
	dir := project(t, "")
	commitConfig(t, dir, "verify:\n  - \"rm -rf .git\"\n")
	d, out, _ := handle(t, payload(t, claudeStop(dir, false)))
	if !d.Block || d.Entry.BlockKind != "indeterminate" || !strings.Contains(out, "no longer available") {
		t.Fatalf("repo vanishing during verify must be indeterminate: %q (%+v)", out, d.Entry)
	}
	found := false
	for _, n := range d.Entry.Notes {
		found = found || strings.Contains(n, "after verify: protected-path check skipped")
	}
	if !found {
		t.Errorf("after-verify note dropped: %q", d.Entry.Notes)
	}
}

func TestPassingHookDuringInFlightBlockKeepsCap(t *testing.T) {
	dir := project(t, "")
	commitConfig(t, dir, "verify:\n  - command: \"if [ -f PASS ]; then exit 0; fi; if [ -f SLOW ]; then rm SLOW; while [ ! -f GO ]; do sleep 0.02; done; fi; exit 1\"\n    timeout: 30s\nmax_retries: 1\n")
	run := func() Decision {
		var stderr bytes.Buffer
		return Handle(payload(t, claudeStop(dir, true)), Options{Getenv: func(string) string { return "" }, Stderr: &stderr})
	}
	touch := func(name string) { writeFile(t, filepath.Join(dir, name), "") }

	// A reserves slot 1 and blocks in a slow failing verify.
	touch("SLOW")
	var wg sync.WaitGroup
	var a Decision
	wg.Add(1)
	go func() { defer wg.Done(); a = run() }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "SLOW")); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hook A never started verifying")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// B passes while A is in flight; it must not free A's slot.
	touch("PASS")
	if b := run(); b.Block || b.Entry.Unverified {
		t.Fatalf("B should allow verified: %+v", b.Entry)
	}
	os.Remove(filepath.Join(dir, "PASS"))
	// C fails: the only slot is still A's.
	if c := run(); c.Block || !c.Entry.Unverified {
		t.Fatalf("C must not get a second block on cap 1: %+v", c.Entry)
	}
	touch("GO")
	wg.Wait()
	if !a.Block {
		t.Fatalf("A should block with its own reservation: %+v", a.Entry)
	}
}

func TestCorruptStateFailsClosedToUnverified(t *testing.T) {
	dir := project(t, "")
	dd := filepath.Join(dir, ".doneguard")
	if _, err := PrepareDataDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dd, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, state.Path(dd, "sess-1"), "{garbage")
	d, out, _ := handle(t, payload(t, claudeStop(dir, true)))
	if d.Block || !d.Entry.Unverified || !strings.Contains(decode(t, out)["systemMessage"], "corrupt") {
		t.Fatalf("corrupt state mid-chain must not restart the counter: %q", out)
	}
	// A fresh turn starts a new chain and repairs the file.
	if d, out, _ := handle(t, payload(t, claudeStop(dir, false))); !d.Block {
		t.Fatalf("fresh turn should block normally: %q", out)
	}
}

func TestUnrecognisedStopHookActiveIsNotFresh(t *testing.T) {
	for in, want := range map[string]bool{"maybe": true, "TRUE": true, "1": true, "false": false, "0": false} {
		p, err := ParsePayload([]byte(`{"hook_event_name":"Stop","session_id":"s","stop_hook_active":"` + in + `"}`))
		if err != nil || p.StopHookActive == nil || *p.StopHookActive != want {
			t.Errorf("%q: got %v, want %v", in, p.StopHookActive, want)
		}
	}
}
