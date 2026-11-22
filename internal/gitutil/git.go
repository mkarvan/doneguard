// Package gitutil lists changed files in a git work tree.
package gitutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrNotRepo is returned when git confirmed that the directory is not
// inside a git work tree.
var ErrNotRepo = errors.New("not a git repository")

// ErrGitMissing is returned when no git binary is available. It is not
// ErrNotRepo: without git DoneGuard cannot tell whether the directory is a
// repository, so callers must treat it as indeterminate.
var ErrGitMissing = errors.New("git executable not found")

// UnbornBase is a Changes baseline meaning "HEAD had no commits": every file
// in the current HEAD counts as committed since the baseline.
const UnbornBase = "(unborn)"

// Change is one changed path, relative to the repository root.
type Change struct {
	Path   string // slash-separated, relative to repo root
	Status string // modified, added, deleted, renamed, untracked, committed
}

const gitTimeout = 30 * time.Second

func git(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Avoid taking index.lock while the agent may be using git concurrently.
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "not a git repository") {
			return nil, ErrNotRepo
		}
		return nil, &Error{Args: args, Err: err, Stderr: msg}
	}
	return out, nil
}

// Error is a failed git invocation.
type Error struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *Error) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
	}
	return fmt.Sprintf("git %s: %v", strings.Join(e.Args, " "), e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Available reports whether a git binary is on PATH.
func Available() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// Root returns the work tree root containing dir.
func Root(dir string) (string, error) {
	if !Available() {
		return "", ErrGitMissing
	}
	out, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		// rev-parse succeeded without a top level (e.g. inside .git):
		// not a confirmed non-repository.
		return "", fmt.Errorf("git rev-parse --show-toplevel printed no work tree for %s", dir)
	}
	return filepath.FromSlash(root), nil
}

// Head returns the commit id of HEAD, or "" if there are no commits or git
// fails. Use HeadErr when a failure must not be mistaken for "no commits".
func Head(root string) string {
	h, _ := HeadErr(root)
	return h
}

// HeadErr returns the commit id of HEAD. A repository without commits yields
// ("", nil); any other git failure is returned as an error.
func HeadErr(root string) (string, error) {
	out, err := git(root, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		var ge *Error
		var ee *exec.ExitError
		// `rev-parse --verify -q` exits 1 silently when HEAD is unborn.
		if errors.As(err, &ge) && ge.Stderr == "" && errors.As(err, &ee) && ee.ExitCode() == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CommittedAtHead reports which of the given repo-relative paths exist as
// files in the HEAD commit. Without commits the result is empty.
func CommittedAtHead(root string, paths []string) (map[string]bool, error) {
	found := map[string]bool{}
	if len(paths) == 0 {
		return found, nil
	}
	head, err := HeadErr(root)
	if err != nil || head == "" {
		return found, err
	}
	args := append([]string{"--literal-pathspecs", "-c", "core.quotepath=false", "ls-tree", "-z", "--full-tree", "--name-only", head, "--"}, paths...)
	out, err := git(root, args...)
	if err != nil {
		return nil, err
	}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			found[p] = true
		}
	}
	return found, nil
}

// HeadMode returns the git tree mode (e.g. "100644", "120000") of a
// repo-relative path at HEAD, or "" if it is not in HEAD.
func HeadMode(root, path string) (string, error) {
	out, err := git(root, "--literal-pathspecs", "ls-tree", "-z", "--full-tree", "HEAD", "--", path)
	if err != nil {
		return "", err
	}
	line := strings.TrimSuffix(string(out), "\x00")
	if line == "" {
		return "", nil
	}
	mode, _, _ := strings.Cut(line, " ")
	return mode, nil
}

// ShowHead returns the content of a repo-relative path at HEAD.
func ShowHead(root, path string) ([]byte, error) {
	return git(root, "show", "HEAD:"+path)
}

// PathDirty reports whether a repo-relative path differs from HEAD (staged,
// unstaged, deleted or untracked).
func PathDirty(root, path string) (bool, error) {
	out, err := git(root, "--literal-pathspecs", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--", path)
	if err != nil {
		return false, err
	}
	return len(out) > 0, nil
}

// Changes returns working-tree changes (staged, unstaged and untracked files,
// honouring .gitignore). If base is a commit other than HEAD, files changed
// in commits base..HEAD are included with status "committed". Any git
// failure, including a baseline commit that no longer exists, is returned as
// an error so callers never mistake it for "no changes".
func Changes(root, base string) ([]Change, error) {
	out, err := git(root, "-c", "core.quotepath=false", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var changes []Change
	add := func(p, status string) {
		p = filepath.ToSlash(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		changes = append(changes, Change{Path: p, Status: status})
	}
	for _, c := range ParseStatusZ(out) {
		add(c.Path, c.Status)
	}
	if base == UnbornBase {
		head, err := HeadErr(root)
		if err != nil {
			return nil, err
		}
		if head != "" {
			files, err := git(root, "-c", "core.quotepath=false", "ls-tree", "-r", "-z", "--full-tree", "--name-only", head)
			if err != nil {
				return nil, err
			}
			for _, p := range strings.Split(string(files), "\x00") {
				add(p, "committed")
			}
		}
	} else if base != "" {
		head, err := HeadErr(root)
		if err != nil {
			return nil, err
		}
		if head != base {
			if _, err := git(root, "cat-file", "-e", base+"^{commit}"); err != nil {
				return nil, fmt.Errorf("baseline commit %s is no longer available: %w", short(base), err)
			}
			target := head
			if target == "" {
				// HEAD became unborn (e.g. branch switched); compare to the empty tree.
				return nil, fmt.Errorf("HEAD has no commits but baseline %s exists", short(base))
			}
			diff, err := git(root, "-c", "core.quotepath=false", "diff", "--name-only", "-z", "--no-renames", base, target, "--")
			if err != nil {
				return nil, err
			}
			for _, p := range strings.Split(string(diff), "\x00") {
				add(p, "committed")
			}
		}
	}
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ParseStatusZ parses `git status --porcelain=v1 -z` output.
func ParseStatusZ(out []byte) []Change {
	var changes []Change
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		x, y, p := f[0], f[1], f[3:]
		status := "modified"
		switch {
		case x == '?' && y == '?':
			status = "untracked"
		case x == '!' && y == '!':
			continue
		case x == 'D' || y == 'D':
			status = "deleted"
		case x == 'A' || y == 'A':
			status = "added"
		case x == 'R' || y == 'R' || x == 'C' || y == 'C':
			status = "renamed"
		}
		changes = append(changes, Change{Path: p, Status: status})
		if x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			// The next field is the original path.
			if i+1 < len(fields) && fields[i+1] != "" {
				changes = append(changes, Change{Path: fields[i+1], Status: "renamed"})
			}
			i++
		}
	}
	return changes
}
