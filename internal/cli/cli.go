// Package cli implements the doneguard command line.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mkarvan/doneguard/internal/config"
	"github.com/mkarvan/doneguard/internal/detect"
	"github.com/mkarvan/doneguard/internal/gitutil"
	"github.com/mkarvan/doneguard/internal/hook"
	"github.com/mkarvan/doneguard/internal/install"
	"github.com/mkarvan/doneguard/internal/report"
	"github.com/mkarvan/doneguard/internal/safepath"
	"github.com/mkarvan/doneguard/internal/sessionlog"
	"github.com/mkarvan/doneguard/internal/wrapper"
)

// Version is set at build time with -ldflags "-X .../cli.Version=...".
var Version = "0.1.0"

// App holds process dependencies so commands are testable.
type App struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getwd  func() (string, error)
	Getenv func(string) string
	// Executable returns the path registered in hook configs.
	Executable func() (string, error)
	HomeDir    func() (string, error)
}

// New returns an App wired to the real process.
func New() *App {
	return &App{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Getwd: os.Getwd, Getenv: os.Getenv, Executable: executable, HomeDir: os.UserHomeDir,
	}
}

func executable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Clean(p), nil
}

const usage = `DoneGuard — don't let coding agents say "done" until the work is verified.

Usage:
  doneguard init [--yes] [--force] [--dir DIR] [--no-gitignore]
                                         write .doneguard.yml (auto-detects verify commands);
                                         --yes: non-interactive, exits 0 if a config already exists
  doneguard install [flags]              merge Stop/SubagentStop hooks into .claude/settings.json and .codex/hooks.json
  doneguard uninstall [flags]            remove only DoneGuard's hook entries
  doneguard hook [--source S] [--exit2]  hook entry point: reads hook JSON on stdin, prints a decision
  doneguard report [--json]              false-done stats from .doneguard/sessions.jsonl
  doneguard run codex -- <codex args>    fallback wrapper: run codex, verify, resume until verified
  doneguard version

Run "doneguard <command> -h" for command flags.
`

// Run dispatches args (without the program name) and returns the exit code.
func (a *App) Run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.Stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "init":
		return a.cmdInit(rest)
	case "install":
		return a.cmdInstall(rest, false)
	case "uninstall":
		return a.cmdInstall(rest, true)
	case "hook":
		return a.cmdHook(rest)
	case "report":
		return a.cmdReport(rest)
	case "run":
		return a.cmdRun(rest)
	case "version", "--version", "-v":
		fmt.Fprintf(a.Stdout, "doneguard %s\n", Version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(a.Stdout, usage)
		return 0
	}
	fmt.Fprintf(a.Stderr, "doneguard: unknown command %q\n\n%s", cmd, usage)
	return 2
}

func (a *App) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("doneguard "+name, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	return fs
}

func (a *App) errorf(format string, args ...any) int {
	fmt.Fprintf(a.Stderr, "doneguard: "+format+"\n", args...)
	return 1
}

func (a *App) cmdInit(args []string) int {
	fs := a.flags("init")
	force := fs.Bool("force", false, "overwrite an existing .doneguard.yml")
	dirFlag := fs.String("dir", "", "project directory (default: current directory)")
	noGitignore := fs.Bool("no-gitignore", false, "do not add .doneguard/ to .gitignore")
	yes := fs.Bool("yes", false, "non-interactive and idempotent: if a config already exists, keep it and exit 0 (use --force to overwrite)")
	fs.BoolVar(yes, "y", false, "shorthand for --yes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		return a.errorf("init: unexpected argument %q", fs.Arg(0))
	}
	dir := *dirFlag
	if dir == "" {
		wd, err := a.Getwd()
		if err != nil {
			return a.errorf("%v", err)
		}
		dir = wd
	}
	dir, _ = filepath.Abs(dir)
	path := filepath.Join(dir, config.FileNames[0])
	if !*force {
		for _, name := range config.FileNames {
			p := filepath.Join(dir, name)
			if _, err := os.Lstat(p); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return a.errorf("%v", err)
			}
			if *yes {
				return a.initExisting(dir, p, *noGitignore)
			}
			return a.errorf("%s already exists (use --force to overwrite)", p)
		}
	}
	target, err := safepath.Resolve(dir, path)
	if err != nil {
		return a.errorf("%v", err)
	}
	found := detect.Detect(dir)
	if err := writeConfig(target, []byte(detect.Render(found)), *force); err != nil {
		if errors.Is(err, os.ErrExist) {
			if *yes {
				return a.initExisting(dir, path, *noGitignore)
			}
			return a.errorf("%s already exists (use --force to overwrite)", path)
		}
		return a.errorf("%v", err)
	}
	fmt.Fprintf(a.Stdout, "wrote %s\n", path)
	if len(found) == 0 {
		fmt.Fprintln(a.Stdout, "  no test command detected — edit verify: in the file and add one (only protected paths are checked until you do)")
	}
	for _, d := range found {
		fmt.Fprintf(a.Stdout, "  verify: %s  (detected: %s)\n", d.Command, d.Why)
	}
	fmt.Fprintf(a.Stdout, "  protected: %s\n", strings.Join(detect.DefaultProtected, ", "))
	if !*noGitignore {
		a.ensureGitignore(dir)
	}
	fmt.Fprintln(a.Stdout, "next: commit .doneguard.yml, then run `doneguard install`")
	return 0
}

// initExisting is `init --yes` on a project that already has a config: the
// config is left untouched (overwriting stays --force only) and init succeeds,
// so agents can run it repeatedly.
//
// Only a regular file (or a symlink to one) counts as an existing config. A
// directory, a dangling symlink or any other entry under the config name is an
// error: reporting success there would let the agent believe a policy exists
// when the hook will find none and allow every stop.
func (a *App) initExisting(dir, path string, noGitignore bool) int {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return a.errorf("%s is a dangling symlink; remove it or fix its target, then re-run init", path)
		}
		return a.errorf("%s exists but cannot be read: %v", path, err)
	}
	if !fi.Mode().IsRegular() {
		return a.errorf("%s exists but is not a regular file (%s); remove or rename it, then re-run init", path, describeMode(fi.Mode()))
	}
	fmt.Fprintf(a.Stdout, "%s already exists; leaving it unchanged (use --force to overwrite)\n", path)
	if !noGitignore {
		a.ensureGitignore(dir)
	}
	return 0
}

func describeMode(m os.FileMode) string {
	switch {
	case m.IsDir():
		return "a directory"
	case m&os.ModeNamedPipe != 0:
		return "a named pipe"
	case m&os.ModeSocket != 0:
		return "a socket"
	case m&os.ModeDevice != 0:
		return "a device"
	default:
		return "mode " + m.String()
	}
}

func (a *App) ensureGitignore(dir string) {
	changed, err := detect.EnsureGitignore(dir)
	if err != nil {
		fmt.Fprintf(a.Stderr, "doneguard: could not update .gitignore: %v\n", err)
	} else if changed {
		fmt.Fprintln(a.Stdout, "added .doneguard/ to .gitignore")
	}
}

// writeConfig creates the config exclusively unless force is set, so two
// concurrent `init`s cannot both write it.
func writeConfig(path string, data []byte, force bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (a *App) projectDir() (string, *config.Config, error) {
	wd, err := a.Getwd()
	if err != nil {
		return "", nil, err
	}
	if p := config.Find(wd); p != "" {
		cfg, err := config.Load(p)
		if err != nil {
			return filepath.Dir(p), nil, err
		}
		return cfg.Dir, cfg, nil
	}
	if root, err := gitutil.Root(wd); err == nil {
		return root, nil, nil
	}
	return wd, nil, nil
}

func (a *App) cmdInstall(args []string, uninstall bool) int {
	name := "install"
	if uninstall {
		name = "uninstall"
	}
	fs := a.flags(name)
	agent := fs.String("agent", "all", "which agent configs to change: claude, codex or all")
	global := fs.Bool("global", false, "use ~/.claude/settings.json and ~/.codex/hooks.json instead of the project")
	local := fs.Bool("local", false, "Claude Code: use .claude/settings.local.json (not committed)")
	exeFlag := fs.String("exe", "", "path of the doneguard binary to register (default: this binary)")
	timeout := fs.Int("timeout", 0, "hook timeout in seconds (default: max(300, sum of verify timeouts + 60))")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var agents []string
	switch *agent {
	case "all", "":
	case "claude", "codex":
		agents = []string{*agent}
	default:
		return a.errorf("--agent must be claude, codex or all")
	}
	dir, cfg, cfgErr := a.projectDir()
	if dir == "" {
		return a.errorf("%v", cfgErr)
	}
	opts := install.Options{ProjectDir: dir, Agents: agents, Global: *global, ClaudeLocal: *local, Timeout: *timeout}
	if *global {
		home, err := a.HomeDir()
		if err != nil {
			return a.errorf("%v", err)
		}
		opts.Home = home
	}
	var results []install.Result
	var err error
	if uninstall {
		// Also claim hooks registered for a renamed binary: the --exe path,
		// or this binary.
		exe := *exeFlag
		if exe == "" && a.Executable != nil {
			exe, _ = a.Executable()
		}
		if exe != "" {
			if abs, err := filepath.Abs(exe); err == nil {
				exe = abs
			}
		}
		opts.Exe = exe
		results, err = install.Uninstall(opts)
	} else {
		exe := *exeFlag
		if exe == "" {
			if exe, err = a.Executable(); err != nil {
				return a.errorf("cannot determine executable path: %v (use --exe)", err)
			}
		}
		if abs, err := filepath.Abs(exe); err == nil {
			exe = abs
		}
		if strings.Contains(exe, "go-build") {
			fmt.Fprintf(a.Stderr, "doneguard: warning: %s looks like a temporary `go run` binary; build with `go build` and install from that binary\n", exe)
		}
		opts.Exe = exe
		if opts.Timeout == 0 {
			opts.Timeout = install.DefaultTimeout
			if cfg != nil {
				if s := int(cfg.TotalTimeout().Seconds()) + 60; s > opts.Timeout {
					opts.Timeout = s
				}
			}
		}
		results, err = install.Install(opts)
	}
	for _, r := range results {
		fmt.Fprintln(a.Stdout, r.String())
	}
	if err != nil {
		return a.errorf("%v", err)
	}
	if !uninstall {
		if cfgErr != nil {
			fmt.Fprintf(a.Stderr, "doneguard: warning: .doneguard.yml is invalid (%v); stops will be blocked until it is fixed\n", cfgErr)
		} else if cfg == nil && !*global {
			fmt.Fprintln(a.Stderr, "doneguard: warning: no .doneguard.yml found — run `doneguard init`; until then the hook allows every stop")
		}
		for _, r := range results {
			if r.Target.Agent == "codex" && r.Action != "unchanged" {
				fmt.Fprintln(a.Stdout, "note: Codex asks you to review/trust new hooks on first use (or pass --dangerously-bypass-hook-trust in automation).")
				fmt.Fprintln(a.Stdout, "      If hooks are unavailable, use `doneguard run codex -- exec \"...\"` instead.")
				break
			}
		}
		fmt.Fprintln(a.Stdout, "Claude Code: restart the session (or review hooks via /hooks) so the new hooks load.")
	}
	return 0
}

// maxHookInput caps hook stdin; larger input is ignored, never truncated.
const maxHookInput = 32 << 20

func (a *App) cmdHook(args []string) int {
	fs := a.flags("hook")
	source := fs.String("source", "", "claude or codex (default: auto-detect)")
	exit2 := fs.Bool("exit2", false, "block via exit code 2 + stderr instead of JSON on stdout")
	verbose := fs.Bool("verbose", false, "log diagnostics to stderr")
	if err := fs.Parse(args); err != nil {
		// Never block the agent because of a bad hook command line.
		return 0
	}
	if v := a.Getenv("DONEGUARD_HOOK_MODE"); v == "exit2" {
		*exit2 = true
	}
	data, err := io.ReadAll(io.LimitReader(a.Stdin, maxHookInput+1))
	if err != nil {
		fmt.Fprintf(a.Stderr, "doneguard: reading stdin: %v\n", err)
		return 0
	}
	if len(data) > maxHookInput {
		// Truncated JSON could parse as something it is not; ignore it.
		fmt.Fprintf(a.Stderr, "doneguard: ignoring hook input larger than %d MiB\n", maxHookInput>>20)
		return 0
	}
	d := hook.Handle(data, hook.Options{
		Source: *source, Getwd: a.Getwd, Getenv: a.Getenv, Stderr: a.Stderr, Verbose: *verbose,
	})
	return d.Write(a.Stdout, a.Stderr, *exit2)
}

func (a *App) findLog() (string, error) {
	wd, err := a.Getwd()
	if err != nil {
		return "", err
	}
	if p := config.Find(wd); p != "" {
		return filepath.Join(filepath.Dir(p), config.DataDirName, "sessions.jsonl"), nil
	}
	dir := wd
	for {
		p := filepath.Join(dir, config.DataDirName, "sessions.jsonl")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no .doneguard.yml or .doneguard/sessions.jsonl found (run `doneguard init` in your project)")
		}
		dir = parent
	}
}

func (a *App) cmdReport(args []string) int {
	fs := a.flags("report")
	asJSON := fs.Bool("json", false, "print machine-readable JSON")
	file := fs.String("file", "", "path to sessions.jsonl (default: found by walking up from cwd)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path := *file
	if path == "" {
		p, err := a.findLog()
		if err != nil {
			return a.errorf("%v", err)
		}
		path = p
	}
	entries, bad, err := sessionlog.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return a.errorf("%v", err)
	}
	s := report.Aggregate(entries)
	s.LogPath = path
	s.MalformedLines = bad
	if *asJSON {
		if err := s.WriteJSON(a.Stdout); err != nil {
			return a.errorf("%v", err)
		}
		return 0
	}
	s.WriteText(a.Stdout)
	return 0
}

func (a *App) cmdRun(args []string) int {
	if len(args) == 0 || args[0] != "codex" {
		return a.errorf("usage: doneguard run codex -- <codex args>  (only codex is supported)")
	}
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	return wrapper.Run(wrapper.Options{
		Args: rest, Stdin: a.Stdin, Stdout: a.Stdout, Stderr: a.Stderr, Getwd: a.Getwd, Getenv: a.Getenv,
	})
}
