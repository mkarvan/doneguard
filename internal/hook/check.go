package hook

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mkarvan/doneguard/internal/config"
	"github.com/mkarvan/doneguard/internal/gitutil"
	"github.com/mkarvan/doneguard/internal/glob"
	"github.com/mkarvan/doneguard/internal/verify"
)

// Violation is a protected path that was created, modified or deleted.
type Violation struct {
	Path   string // relative to the config dir (or repo root if outside it)
	Status string
	Rule   string
}

func (v Violation) String() string { return fmt.Sprintf("%s (%s)", v.Path, v.Status) }

// CheckResult is the outcome of protected-path + verify checks.
type CheckResult struct {
	Violations    []Violation
	VerifySkipped bool // no verify commands configured
	VerifyRan     bool
	Failed        *verify.Result
	// GitErr is set when the protected-path check could not be completed
	// (a git failure other than "not a repository"): an indeterminate check.
	GitErr  string
	Notes   []string
	GitRoot string
	Head    string
	// Baseline is the commit the protected-path check compared against
	// before verify ran: the given baseHead, or the pre-verify HEAD when no
	// baseline was stored (gitutil.UnbornBase for a repository without
	// commits). Callers store it when no baseline exists yet, so commits
	// made during this stop stay visible to the next one.
	Baseline string
}

// OK reports whether everything passed.
func (r *CheckResult) OK() bool {
	return len(r.Violations) == 0 && r.Failed == nil && r.GitErr == ""
}

// VerifyOK is nil when verify did not run.
func (r *CheckResult) VerifyOK() *bool {
	if !r.VerifyRan {
		return nil
	}
	ok := r.Failed == nil
	return &ok
}

// ViolationStrings lists violated paths.
func (r *CheckResult) ViolationStrings() []string {
	out := make([]string, 0, len(r.Violations))
	for _, v := range r.Violations {
		out = append(out, v.Path)
	}
	return out
}

// PolicyTampered reports whether DoneGuard's own config was touched.
func (r *CheckResult) PolicyTampered() bool {
	for _, v := range r.Violations {
		if v.Rule == configRule {
			return true
		}
	}
	return false
}

// Kind classifies a failure for logging: protected, verify, indeterminate or "".
func (r *CheckResult) Kind() string {
	switch {
	case len(r.Violations) > 0:
		return "protected"
	case r.Failed != nil:
		return "verify"
	case r.GitErr != "":
		return "indeterminate"
	}
	return ""
}

// Check runs the protected-path check, then the verify commands, then the
// protected-path check again (a test or generator may touch a protected file
// while verifying). baseHead (optional) extends the protected check to
// commits made since.
//
// Verify commands are never run when the policy file itself was changed or
// when the protected-path check is indeterminate: in both cases the commands
// might come from a mutated policy.
func Check(cfg *config.Config, baseHead string) *CheckResult {
	res := &CheckResult{}
	checkProtected(cfg, baseHead, res)
	if len(cfg.Verify) == 0 {
		res.VerifySkipped = true
		res.Notes = append(res.Notes, "verify skipped: no verify commands configured in .doneguard.yml")
		return res
	}
	switch {
	case res.PolicyTampered():
		res.Notes = append(res.Notes, "verify not run: the DoneGuard config was modified")
		return res
	case res.GitErr != "":
		res.Notes = append(res.Notes, "verify not run: protected-path check was indeterminate")
		return res
	}
	res.VerifyRan = true
	_, res.Failed = verify.RunAll(cfg.Verify, cfg.VerifyCwd)

	// Re-check against the state before verify, so commits made by a verify
	// command are seen even when no baseline was stored yet.
	after := &CheckResult{}
	checkProtected(cfg, res.Baseline, after)
	for _, n := range after.Notes {
		res.Notes = append(res.Notes, "after verify: "+n)
	}
	switch {
	case after.GitErr != "":
		if res.GitErr == "" {
			res.GitErr = "after verify: " + after.GitErr
		}
	case res.GitRoot != "" && after.GitRoot != res.GitRoot:
		// The repository vanished or moved while verify ran: the change
		// set cannot be determined.
		if res.GitErr == "" {
			res.GitErr = "after verify: the git repository at " + res.GitRoot + " is no longer available"
		}
	}
	if after.Head != "" {
		res.Head = after.Head
	}
	seen := map[string]int{}
	for i, v := range res.Violations {
		seen[v.Path] = i
	}
	for _, v := range after.Violations {
		if i, ok := seen[v.Path]; ok {
			res.Violations[i] = v
			continue
		}
		v.Status += ", during verify"
		res.Violations = append(res.Violations, v)
		seen[v.Path] = len(res.Violations) - 1
	}
	return res
}

func checkProtected(cfg *config.Config, baseHead string, res *CheckResult) {
	root, err := gitutil.Root(cfg.Dir)
	if err != nil {
		if errors.Is(err, gitutil.ErrNotRepo) {
			res.Notes = append(res.Notes, "protected-path check skipped: not a git repository")
			return
		}
		res.GitErr = err.Error()
		return
	}
	res.GitRoot = root
	head, err := gitutil.HeadErr(root)
	if err != nil {
		res.GitErr = err.Error()
		return
	}
	res.Head = head
	res.Baseline = baseHead
	if res.Baseline == "" {
		res.Baseline = head
		if head == "" {
			res.Baseline = gitutil.UnbornBase
		}
	}
	changes, err := gitutil.Changes(root, baseHead)
	if err != nil {
		res.GitErr = err.Error()
		return
	}
	m, err := glob.Compile(cfg.Protected)
	if err != nil {
		// Config validation already compiled these; be defensive anyway.
		res.GitErr = err.Error()
		return
	}
	cfgName := filepath.Base(cfg.Path)
	for _, c := range changes {
		rel := relToConfig(cfg.Dir, root, c.Path)
		if isNoise(rel) {
			continue
		}
		// The config file itself is implicitly protected once committed, so
		// an agent cannot "pass" by editing or deleting the verify commands.
		if cfg.Path != "" && isConfigName(rel, cfgName) {
			if c.Status != "untracked" {
				res.Violations = append(res.Violations, Violation{Path: rel, Status: c.Status, Rule: configRule})
			}
			continue
		}
		if rule, ok := m.MatchRule(rel); ok {
			res.Violations = append(res.Violations, Violation{Path: rel, Status: c.Status, Rule: rule})
		}
	}
}

// isConfigName matches the governing config and its alternate spelling in
// the same directory (renaming .yml to .yaml is a policy change too).
func isConfigName(rel, cfgName string) bool {
	if rel == cfgName {
		return true
	}
	for _, n := range config.FileNames {
		if rel == n {
			return true
		}
	}
	return false
}

// relToConfig converts a repo-relative path to a config-dir-relative one;
// paths outside the config dir stay repo-relative.
func relToConfig(cfgDir, root, p string) string {
	realCfg := cfgDir
	if r, err := filepath.EvalSymlinks(cfgDir); err == nil {
		realCfg = r
	}
	realRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = r
	}
	abs := filepath.Join(realRoot, filepath.FromSlash(p))
	rel, err := filepath.Rel(realCfg, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.ToSlash(rel)
}

func isNoise(rel string) bool {
	for _, c := range strings.Split(rel, "/") {
		if c == config.DataDirName || c == ".git" {
			return true
		}
	}
	return false
}

// BuildReason renders the concise, actionable block reason.
func BuildReason(cfg *config.Config, res *CheckResult) string {
	var b strings.Builder
	if len(res.Violations) > 0 {
		names := make([]string, 0, len(res.Violations))
		for _, v := range res.Violations {
			names = append(names, v.String())
		}
		label := "protected file touched: "
		if len(names) > 1 {
			label = "protected files touched: "
		}
		const maxNames = 10
		if len(names) > maxNames {
			names = append(names[:maxNames], fmt.Sprintf("and %d more", len(names)-maxNames))
		}
		b.WriteString("DoneGuard blocked stop: " + label + strings.Join(names, ", ") + "\n")
		if res.PolicyTampered() {
			b.WriteString("The DoneGuard config is protected: restore it to the committed version (e.g. `git checkout -- <file>`, or delete an uncommitted copy). Verify commands were not run.")
			if len(res.Violations) > 1 {
				b.WriteString("\n")
			}
		}
		if !res.PolicyTampered() || len(res.Violations) > 1 {
			b.WriteString("Do not modify protected paths. Revert the change (e.g. `git checkout -- <file>` or delete the new file).")
		}
	}
	if res.GitErr != "" {
		if b.Len() > 0 {
			b.WriteString("\n\nAlso: ")
		} else {
			b.WriteString("DoneGuard blocked stop: ")
		}
		b.WriteString("could not check protected paths (git error: " + oneLine(res.GitErr) + ").\n")
		b.WriteString("The stop cannot be verified until git works in this repository again (fix the repository state, e.g. a stale lock or broken HEAD), then you may stop.")
	}
	if res.Failed != nil {
		if b.Len() > 0 {
			b.WriteString("\n\nAlso: ")
		} else {
			b.WriteString("DoneGuard blocked stop: ")
		}
		b.WriteString(res.Failed.Summary() + ".")
		out := verify.Trim(res.Failed.Output, cfg.OutputLimit)
		if out != "" {
			b.WriteString("\n--- output ---\n" + out + "\n---")
		}
		b.WriteString("\n" + fixHint(res.Failed))
	}
	return b.String()
}

func fixHint(r *verify.Result) string {
	cmd := strings.TrimSpace(r.Command)
	if strings.Contains(cmd, "\n") {
		cmd = "the verify command"
	} else {
		cmd = "`" + cmd + "`"
	}
	switch {
	case r.TimedOut:
		return fmt.Sprintf("Make %s finish within %s (fix hangs or slow tests), then you may stop.", cmd, r.Timeout)
	case r.Incomplete:
		return fmt.Sprintf("Make %s exit without leaving background processes holding its output, then you may stop.", cmd)
	case r.StartErr != "":
		return fmt.Sprintf("Make %s runnable (check the command and working directory), then you may stop.", cmd)
	}
	return fmt.Sprintf("Fix the failing tests/checks, re-run %s until it passes, then you may stop.", cmd)
}

// ConfigErrorReason renders the reason for an invalid .doneguard.yml.
func ConfigErrorReason(path string, err error) string {
	return fmt.Sprintf("DoneGuard blocked stop: %s is invalid: %v\nFix the config file (see the DoneGuard README for the format), then you may stop.", path, err)
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}
