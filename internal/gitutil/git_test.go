package gitutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
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

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func statusMap(cs []Change) map[string]string {
	m := map[string]string{}
	for _, c := range cs {
		m[c.Path] = c.Status
	}
	return m
}

func TestChangesIncludesUntrackedAndCommitted(t *testing.T) {
	if !Available() {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	write(t, filepath.Join(dir, "tracked.txt"), "a\n")
	write(t, filepath.Join(dir, "gone.txt"), "x\n")
	write(t, filepath.Join(dir, ".gitignore"), "ignored.txt\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "init")
	base := Head(dir)
	if base == "" {
		t.Fatal("no HEAD")
	}

	write(t, filepath.Join(dir, "tracked.txt"), "b\n")
	write(t, filepath.Join(dir, "deep", "new file.txt"), "n\n")
	write(t, filepath.Join(dir, "ignored.txt"), "i\n")
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	root, err := Root(filepath.Join(dir, "deep"))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := Changes(root, "")
	if err != nil {
		t.Fatal(err)
	}
	m := statusMap(cs)
	if m["tracked.txt"] != "modified" || m["deep/new file.txt"] != "untracked" || m["gone.txt"] != "deleted" {
		t.Errorf("changes = %v", m)
	}
	if _, ok := m["ignored.txt"]; ok {
		t.Errorf("ignored file listed: %v", m)
	}

	// Commit a change; with base it must still be reported.
	write(t, filepath.Join(dir, "committed.txt"), "c\n")
	run(t, dir, "add", "committed.txt")
	run(t, dir, "commit", "-q", "-m", "c")
	cs, err = Changes(root, base)
	if err != nil {
		t.Fatal(err)
	}
	m = statusMap(cs)
	if m["committed.txt"] != "committed" {
		t.Errorf("committed change missing: %v", m)
	}
	cs, _ = Changes(root, "")
	if _, ok := statusMap(cs)["committed.txt"]; ok {
		t.Errorf("without base, committed file should not appear")
	}
}

func TestRootNotRepo(t *testing.T) {
	if !Available() {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	if _, err := Root(dir); err == nil {
		t.Fatal("expected error outside a repo")
	}
}

func TestParseStatusZRename(t *testing.T) {
	out := []byte("R  new.txt\x00old.txt\x00 M a.go\x00?? b.go\x00D  c.go\x00A  d.go\x00")
	m := statusMap(ParseStatusZ(out))
	want := map[string]string{"new.txt": "renamed", "old.txt": "renamed", "a.go": "modified", "b.go": "untracked", "c.go": "deleted", "d.go": "added"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, m[k], v, m)
		}
	}
}

func TestHeadErrAndBaselineErrors(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	if h, err := HeadErr(dir); h != "" || err != nil {
		t.Fatalf("unborn HEAD: %q %v", h, err)
	}
	write(t, filepath.Join(dir, "a.txt"), "a")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "a")
	if h, err := HeadErr(dir); len(h) < 40 || err != nil {
		t.Fatalf("HEAD: %q %v", h, err)
	}
	// A vanished baseline is an error, never "no changes".
	if _, err := Changes(dir, "0123456789abcdef0123456789abcdef01234567"); err == nil {
		t.Fatal("missing baseline must be an error")
	}
	got, err := CommittedAtHead(dir, []string{"a.txt", "missing.txt", "*"})
	if err != nil || !got["a.txt"] || len(got) != 1 {
		t.Fatalf("CommittedAtHead = %v %v (pathspecs must be literal)", got, err)
	}
	// A broken index is an operational error.
	write(t, filepath.Join(dir, ".git", "index"), "garbage")
	if _, err := Changes(dir, ""); err == nil || err == ErrNotRepo {
		t.Fatalf("corrupt index: err = %v", err)
	}
}

func TestRootWithoutGitIsNotNotRepo(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Root(t.TempDir())
	if err == nil || errors.Is(err, ErrNotRepo) || !errors.Is(err, ErrGitMissing) {
		t.Fatalf("missing git must be indeterminate, got %v", err)
	}
}
