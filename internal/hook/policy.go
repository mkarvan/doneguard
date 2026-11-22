package hook

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/mkarvan/doneguard/internal/config"
	"github.com/mkarvan/doneguard/internal/gitutil"
)

// configRule labels violations of DoneGuard's own policy file.
const configRule = "(doneguard config)"

// Policy is the outcome of locating the .doneguard.yml that governs cwd.
//
// When the directory is inside a git repository, the committed config is
// the source of truth: the nearest directory (from cwd up to the repo root)
// that has a .doneguard.yml/.yaml in HEAD owns the policy. Deleting,
// renaming or modifying that file, or adding an uncommitted config that
// would shadow it, is a policy-integrity violation. DoneGuard then blocks
// without running any verify command from the working tree, and never falls
// back to a parent project's config.
type Policy struct {
	// Dir is the project directory (owner of .doneguard/). "" means
	// DoneGuard is not active here.
	Dir string
	// Path is the config file to load when there are no violations.
	Path string
	// Committed is the HEAD content of the governing config, if any. When
	// there are violations it supplies max_retries/output_limit so the
	// working-tree copy is never trusted.
	Committed []byte
	// Violations are policy-integrity failures (deleted/renamed/modified
	// config, shadowing config, or a vanished config whose data dir remains).
	Violations []Violation
	// FromCommit means Committed holds the bytes of Path at HEAD; the
	// policy is then loaded from those bytes, never re-read from the work
	// tree (which could have been replaced since the integrity check).
	FromCommit bool
	// GitErr is set when git failed in a way that leaves the policy
	// undetermined (an indeterminate check).
	GitErr error
}

// Active reports whether DoneGuard governs this directory.
func (p *Policy) Active() bool { return p.Dir != "" }

// ResolvePolicy locates the governing config for cwd.
func ResolvePolicy(cwd string) Policy {
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return Policy{}
	}
	nearest := config.Find(cwd)
	root, err := gitutil.Root(cwd)
	if err != nil {
		if errors.Is(err, gitutil.ErrNotRepo) {
			return fromNearest(nearest)
		}
		// Git is missing or failed: whether a committed policy exists (or
		// was deleted) cannot be determined.
		if nearest == "" {
			// No config anywhere. Only a project whose runtime data shows
			// DoneGuard was in use is blocked; a git problem must not brick
			// an unrelated project.
			if d := runtimeDataDir(cwd); d != "" {
				return Policy{Dir: d, GitErr: err}
			}
			return Policy{}
		}
		return Policy{Dir: filepath.Dir(nearest), GitErr: err}
	}
	realRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = r
	}
	realCwd := cwd
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		realCwd = r
	}

	// Directories from cwd up to the repo root, nearest first.
	var dirs []string
	for d := realCwd; ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if d == realRoot || filepath.Dir(d) == d {
			break
		}
	}
	if dirs[len(dirs)-1] != realRoot {
		// cwd is not under the reported root (unusual symlink layout):
		// keep the pre-git behaviour.
		return fromNearest(nearest)
	}
	relOf := func(abs string) string {
		r, _ := filepath.Rel(realRoot, abs)
		return filepath.ToSlash(r)
	}
	var candidates []string
	for _, d := range dirs {
		for _, name := range config.FileNames {
			candidates = append(candidates, relOf(filepath.Join(d, name)))
		}
	}
	committed, err := gitutil.CommittedAtHead(root, candidates)
	if err != nil {
		if nearest == "" {
			for _, d := range dirs {
				if hasRuntimeData(d) {
					return Policy{Dir: d, GitErr: err}
				}
			}
			return Policy{}
		}
		return Policy{Dir: filepath.Dir(nearest), GitErr: err}
	}

	ownerIdx := -1
	for i, d := range dirs {
		for _, name := range config.FileNames {
			if committed[relOf(filepath.Join(d, name))] {
				ownerIdx = i
				break
			}
		}
		if ownerIdx >= 0 {
			break
		}
	}

	if ownerIdx < 0 {
		// Nothing committed (e.g. right after `doneguard init`). A config
		// that vanished while its runtime data remains is still suspicious.
		limit := len(dirs)
		if nearest != "" {
			for i, d := range dirs {
				if sameDir(d, filepath.Dir(nearest)) {
					limit = i
					break
				}
			}
		}
		for _, d := range dirs[:limit] {
			if hasRuntimeData(d) {
				return Policy{Dir: d, Violations: []Violation{{
					Path: config.FileNames[0], Status: "missing; .doneguard/ data shows it was in use", Rule: configRule,
				}}}
			}
		}
		return fromNearest(nearest)
	}

	// A nearer project whose committed config was deleted (and the deletion
	// committed) must not be silently taken over by a parent's config:
	// its runtime data shows it had its own policy.
	for _, d := range dirs[:ownerIdx] {
		if hasRuntimeData(d) && !hasConfigFile(d) {
			return Policy{Dir: d, Violations: []Violation{{
				Path: config.FileNames[0], Status: "missing; .doneguard/ data shows it was in use", Rule: configRule,
			}}}
		}
	}

	owner := dirs[ownerIdx]
	pol := Policy{Dir: owner}
	relToOwner := func(abs string) string {
		r, err := filepath.Rel(owner, abs)
		if err != nil {
			return abs
		}
		return filepath.ToSlash(r)
	}
	for _, d := range dirs[:ownerIdx+1] {
		for _, name := range config.FileNames {
			abs := filepath.Join(d, name)
			isCommitted := committed[relOf(abs)]
			exists := isFile(abs)
			if isCommitted || exists {
				// The policy must be a regular file both in HEAD and in the
				// work tree: a symlink's target can change while the link
				// itself stays clean.
				if fi, err := os.Lstat(abs); err == nil && fi.Mode()&os.ModeSymlink != 0 {
					pol.Violations = append(pol.Violations, Violation{Path: relToOwner(abs), Status: "symlink; the policy must be a regular file", Rule: configRule})
					continue
				}
				if isCommitted {
					mode, err := gitutil.HeadMode(root, relOf(abs))
					if err != nil {
						pol.GitErr = err
						continue
					}
					if mode == "120000" {
						pol.Violations = append(pol.Violations, Violation{Path: relToOwner(abs), Status: "committed as a symlink; the policy must be a regular file", Rule: configRule})
						continue
					}
				}
			}
			switch {
			case isCommitted && !exists:
				pol.Violations = append(pol.Violations, Violation{Path: relToOwner(abs), Status: "deleted or renamed", Rule: configRule})
			case exists && !isCommitted:
				pol.Violations = append(pol.Violations, Violation{Path: relToOwner(abs), Status: "uncommitted config shadowing the committed one", Rule: configRule})
			case isCommitted && pol.Path == "" && d == owner:
				pol.Path = abs
				data, err := gitutil.ShowHead(root, relOf(abs))
				if err != nil {
					pol.GitErr = err
					continue
				}
				pol.Committed = data
				pol.FromCommit = true
				dirty, err := gitutil.PathDirty(root, relOf(abs))
				if err != nil {
					pol.GitErr = err
				} else if dirty {
					pol.Violations = append(pol.Violations, Violation{Path: relToOwner(abs), Status: "modified", Rule: configRule})
				}
			}
		}
	}
	if pol.Committed == nil {
		// Only used for limits on a blocked stop, never for verify commands.
		// Fall back to whichever committed config the owner dir had.
		for _, name := range config.FileNames {
			if rel := relOf(filepath.Join(owner, name)); committed[rel] {
				if data, err := gitutil.ShowHead(root, rel); err == nil {
					pol.Committed = data
					break
				}
			}
		}
	}
	return pol
}

// Load parses the policy for execution. A committed policy is parsed from
// the HEAD bytes that passed the integrity check; only an uncommitted
// policy (no repository, or nothing committed yet) is read from disk.
func (p *Policy) Load() (*config.Config, error) {
	if p.FromCommit {
		cfg, err := config.Parse(p.Committed, filepath.Dir(p.Path))
		if err != nil {
			return nil, err
		}
		cfg.Path = p.Path
		return cfg, nil
	}
	return config.Load(p.Path)
}

func fromNearest(nearest string) Policy {
	if nearest == "" {
		return Policy{}
	}
	return Policy{Dir: filepath.Dir(nearest), Path: nearest}
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// runtimeDataDir returns the nearest directory from dir upwards that has
// DoneGuard runtime data, or "".
func runtimeDataDir(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if hasRuntimeData(d) {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

func hasConfigFile(dir string) bool {
	for _, name := range config.FileNames {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

func hasRuntimeData(dir string) bool {
	dd := filepath.Join(dir, config.DataDirName)
	for _, name := range []string{"state", "sessions.jsonl"} {
		if _, err := os.Lstat(filepath.Join(dd, name)); err == nil {
			return true
		}
	}
	return false
}
