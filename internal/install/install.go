// Package install merges DoneGuard's Stop / SubagentStop hooks into Claude
// Code (.claude/settings.json) and Codex (.codex/hooks.json) configs without
// clobbering anything else.
package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mkarvan/doneguard/internal/safepath"
)

// StatusMessage marks DoneGuard hook entries (also shown as a spinner label).
const StatusMessage = "DoneGuard verification"

// Events DoneGuard registers for.
var Events = []string{"Stop", "SubagentStop"}

// DefaultTimeout is the hook timeout in seconds (generous so suites finish).
const DefaultTimeout = 300

// Target is one hook config file.
type Target struct {
	Agent  string // "claude" | "codex"
	Label  string
	Path   string
	Source string
}

// Options configures Install / Uninstall.
type Options struct {
	ProjectDir  string
	Exe         string   // absolute path of the doneguard binary (Uninstall: optional, also removes hooks registered for it)
	Agents      []string // subset of "claude", "codex"; empty = both
	Global      bool     // write to ~/.claude and ~/.codex instead
	Home        string   // home dir for Global (defaults to os.UserHomeDir)
	ClaudeLocal bool     // use .claude/settings.local.json (not committed)
	Timeout     int      // seconds; 0 = DefaultTimeout
}

// Result describes what happened to one file.
type Result struct {
	Target Target
	Action string // created | updated | unchanged | removed | absent
	Detail string
}

func (r Result) String() string {
	s := fmt.Sprintf("%-9s %s (%s)", r.Action, r.Target.Path, r.Target.Label)
	if r.Detail != "" {
		s += ": " + r.Detail
	}
	return s
}

// Targets resolves the config files for opts.
func Targets(opts Options) ([]Target, error) {
	agents := opts.Agents
	if len(agents) == 0 {
		agents = []string{"claude", "codex"}
	}
	base := opts.ProjectDir
	if opts.Global {
		base = opts.Home
		if base == "" {
			h, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			base = h
		}
	}
	var out []Target
	for _, a := range agents {
		switch a {
		case "claude":
			name := "settings.json"
			if opts.ClaudeLocal && !opts.Global {
				name = "settings.local.json"
			}
			out = append(out, Target{Agent: "claude", Label: "Claude Code", Path: filepath.Join(base, ".claude", name), Source: "claude"})
		case "codex":
			out = append(out, Target{Agent: "codex", Label: "Codex", Path: filepath.Join(base, ".codex", "hooks.json"), Source: "codex"})
		default:
			return nil, fmt.Errorf("unknown agent %q (want claude or codex)", a)
		}
	}
	return out, nil
}

// HookCommand is the command string registered in hook configs.
func HookCommand(exe, source string) string {
	return shellQuote(exe) + " hook --source " + source
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"$`\\!*?[]{}()<>|&;#~") {
		return s
	}
	if runtime.GOOS == "windows" {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// IsDoneGuardHook reports whether a hook entry is one DoneGuard manages.
//
// Only the exact managed command shape is claimed: a single program whose
// file name is doneguard (or doneguard.exe), at any path, followed by
// `hook` and optionally DoneGuard's own hook flags. This covers the command
// written by `doneguard install` (absolute path + ` hook --source X`) and
// the documented legacy forms (`doneguard hook`, `/path/doneguard hook`).
// Compound shell commands, wrapper scripts and entries that merely carry
// DoneGuard's statusMessage are never claimed, so install/uninstall cannot
// remove or replace someone else's hook.
func IsDoneGuardHook(v any) bool { return isManagedHook(v, "") }

// isManagedHook is IsDoneGuardHook that also claims hooks whose program is
// exactly exe (the binary registered with `install --exe`, whatever its
// file name).
func isManagedHook(v any, exe string) bool {
	obj, ok := v.(*Object)
	if !ok {
		return false
	}
	cmd, _ := obj.Get("command")
	s, ok := cmd.(string)
	return ok && isManagedCommand(s, exe)
}

func isManagedCommand(s, exe string) bool {
	argv, ok := splitCommand(s)
	if !ok || len(argv) < 2 {
		return false
	}
	prog := argv[0]
	if i := strings.LastIndexAny(prog, `/\`); i >= 0 {
		prog = prog[i+1:]
	}
	if prog != "doneguard" && !strings.EqualFold(prog, "doneguard.exe") && (exe == "" || argv[0] != exe) {
		return false
	}
	if argv[1] != "hook" {
		return false
	}
	rest := argv[2:]
	for i := 0; i < len(rest); i++ {
		switch a := rest[i]; {
		case a == "--exit2" || a == "--verbose":
		case a == "--source" && i+1 < len(rest) && (rest[i+1] == "claude" || rest[i+1] == "codex"):
			i++
		case a == "--source=claude" || a == "--source=codex":
		default:
			return false
		}
	}
	return true
}

// splitCommand splits a simple shell command into words, honouring single
// and double quotes. It reports false for anything beyond a single simple
// command (operators, redirections, expansions, comments).
func splitCommand(s string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case c == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '$' || s[i] == '`' {
					return nil, false
				}
				if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, false
			}
			inWord = true
		case strings.IndexByte(";&|<>$`()#\n\r*?[]{}~!\\", c) >= 0:
			if c == '\\' && runtime.GOOS == "windows" {
				cur.WriteByte(c)
				inWord = true
				continue
			}
			return nil, false
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, true
}

// unrestricted reports whether a matcher group applies to every stop (no
// matcher, empty or "*"), the only kind DoneGuard places its hook in.
func unrestricted(g *Object) bool {
	m, ok := g.Get("matcher")
	if !ok || m == nil {
		return true
	}
	ms, isStr := m.(string)
	return isStr && (ms == "" || ms == "*")
}

func hookEntry(exe, source string, timeout int) *Object {
	o := NewObject()
	o.Set("type", "command")
	o.Set("command", HookCommand(exe, source))
	o.Set("timeout", timeout)
	o.Set("statusMessage", StatusMessage)
	return o
}

// Install merges hooks into each target.
func Install(opts Options) ([]Result, error) {
	if opts.Exe == "" {
		return nil, errors.New("install: executable path is required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	targets, err := Targets(opts)
	if err != nil {
		return nil, err
	}
	var results []Result
	for _, t := range targets {
		r, err := installOne(t, opts)
		if err != nil {
			return results, fmt.Errorf("%s: %w", t.Path, err)
		}
		results = append(results, r)
	}
	return results, nil
}

// Uninstall removes DoneGuard hook entries from each target.
func Uninstall(opts Options) ([]Result, error) {
	targets, err := Targets(opts)
	if err != nil {
		return nil, err
	}
	var results []Result
	for _, t := range targets {
		r, err := uninstallOne(t, opts)
		if err != nil {
			return results, fmt.Errorf("%s: %w", t.Path, err)
		}
		results = append(results, r)
	}
	return results, nil
}

func load(path string) (root *Object, existed bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewObject(), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return NewObject(), true, nil
	}
	v, err := Parse(data)
	if err != nil {
		return nil, true, fmt.Errorf("not valid JSON (left untouched): %w", err)
	}
	obj, ok := v.(*Object)
	if !ok {
		return nil, true, errors.New("top-level JSON value is not an object (left untouched)")
	}
	return obj, true, nil
}

func hooksObject(root *Object, create bool) (*Object, error) {
	v, ok := root.Get("hooks")
	if !ok || v == nil {
		if !create {
			return nil, nil
		}
		h := NewObject()
		root.Set("hooks", h)
		return h, nil
	}
	h, ok := v.(*Object)
	if !ok {
		return nil, errors.New(`"hooks" is not an object (left untouched)`)
	}
	return h, nil
}

// mergeEvent replaces DoneGuard entries for one event with desired (or
// removes them when desired is nil). It reports how many DoneGuard entries
// were found.
func mergeEvent(hooks *Object, event string, desired *Object, exe string) (found int, err error) {
	var groups []any
	if v, ok := hooks.Get(event); ok && v != nil {
		arr, ok := v.([]any)
		if !ok {
			return 0, fmt.Errorf("hooks.%s is not an array (left untouched)", event)
		}
		groups = arr
	}
	placed := false
	var out []any
	for _, g := range groups {
		gobj, ok := g.(*Object)
		if !ok {
			out = append(out, g)
			continue
		}
		innerV, _ := gobj.Get("hooks")
		inner, ok := innerV.([]any)
		if !ok {
			out = append(out, g)
			continue
		}
		var kept []any
		had := false
		for _, h := range inner {
			if isManagedHook(h, exe) {
				had = true
				found++
				if desired != nil && !placed && unrestricted(gobj) {
					kept = append(kept, desired)
					placed = true
				}
				continue
			}
			kept = append(kept, h)
		}
		if had && len(kept) == 0 {
			continue // group only held DoneGuard; drop it
		}
		if had {
			if kept == nil {
				kept = []any{}
			}
			gobj.Set("hooks", kept)
		}
		out = append(out, gobj)
	}
	if desired != nil && !placed {
		g := NewObject()
		g.Set("hooks", []any{desired})
		out = append(out, g)
	}
	if len(out) == 0 {
		hooks.Delete(event)
	} else {
		hooks.Set(event, out)
	}
	return found, nil
}

// resolveTarget confines project-local targets to the project directory:
// symlinks (in the file or any parent) that lead outside it are refused.
// Global targets follow symlinks so dotfile managers keep working.
func resolveTarget(t Target, opts Options) (string, error) {
	if opts.Global {
		if real, err := filepath.EvalSymlinks(t.Path); err == nil {
			return real, nil
		}
		return t.Path, nil
	}
	return safepath.Resolve(opts.ProjectDir, t.Path)
}

func installOne(t Target, opts Options) (Result, error) {
	path, err := resolveTarget(t, opts)
	if err != nil {
		return Result{}, err
	}
	root, existed, err := load(path)
	if err != nil {
		return Result{}, err
	}
	before := Marshal(root)
	if !existed && t.Agent == "codex" {
		root.Set("description", "Project hooks for Codex. DoneGuard entries are managed by `doneguard install` / `doneguard uninstall`.")
	}
	hooks, err := hooksObject(root, true)
	if err != nil {
		return Result{}, err
	}
	for _, ev := range Events {
		if _, err := mergeEvent(hooks, ev, hookEntry(opts.Exe, t.Source, opts.Timeout), opts.Exe); err != nil {
			return Result{}, err
		}
	}
	after := Marshal(root)
	res := Result{Target: t}
	switch {
	case existed && string(before) == string(after):
		res.Action = "unchanged"
		res.Detail = "DoneGuard Stop/SubagentStop hooks already installed"
		return res, nil
	case existed:
		res.Action = "updated"
		res.Detail = "merged DoneGuard Stop + SubagentStop hooks (other settings kept)"
	default:
		res.Action = "created"
		res.Detail = "added DoneGuard Stop + SubagentStop hooks"
	}
	return res, writeFileAtomic(path, after)
}

func uninstallOne(t Target, opts Options) (Result, error) {
	res := Result{Target: t}
	path, err := resolveTarget(t, opts)
	if err != nil {
		return res, err
	}
	root, existed, err := load(path)
	if err != nil {
		return res, err
	}
	if !existed {
		res.Action = "absent"
		return res, nil
	}
	hooks, err := hooksObject(root, false)
	if err != nil {
		return res, err
	}
	total := 0
	if hooks != nil {
		for _, ev := range Events {
			n, err := mergeEvent(hooks, ev, nil, opts.Exe)
			if err != nil {
				return res, err
			}
			total += n
		}
		if hooks.Len() == 0 {
			root.Delete("hooks")
		}
	}
	if total == 0 {
		res.Action = "unchanged"
		res.Detail = "no DoneGuard hooks found"
		return res, nil
	}
	res.Action = "removed"
	res.Detail = fmt.Sprintf("removed %d DoneGuard hook entr%s (other hooks kept)", total, map[bool]string{true: "y", false: "ies"}[total == 1])
	return res, writeFileAtomic(path, Marshal(root))
}

// writeFileAtomic replaces path (already resolved by resolveTarget).
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
