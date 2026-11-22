// Package detect guesses verify commands for a project and renders the
// initial .doneguard.yml.
package detect

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mkarvan/doneguard/internal/config"
	"github.com/mkarvan/doneguard/internal/safepath"
)

// Detected is one auto-detected verify command.
type Detected struct {
	Command string
	Timeout time.Duration
	Why     string
}

// DefaultProtected are the protected globs written by init.
var DefaultProtected = []string{
	".env",
	".env.*",
	"**/*secret*",
	"**/*credential*",
	"**/id_rsa",
	"**/*.pem",
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func read(dir, name string) string {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return string(data)
}

// npm's placeholder test script.
const npmDefaultTest = `echo "Error: no test specified" && exit 1`

// Detect inspects dir. A Makefile `test` target is treated as the project's
// canonical entry point and used alone; otherwise every clearly present
// ecosystem contributes one command (npm/pnpm/yarn/bun, pytest, go, cargo).
func Detect(dir string) []Detected {
	if target := makefileWithTest(dir); target != "" {
		return []Detected{{Command: "make test", Timeout: 300 * time.Second, Why: target + " has a test target"}}
	}
	var out []Detected
	if d, ok := detectNode(dir); ok {
		out = append(out, d)
	}
	if d, ok := detectPython(dir); ok {
		out = append(out, d)
	}
	if exists(dir, "go.mod") {
		out = append(out, Detected{Command: "go test ./...", Timeout: 300 * time.Second, Why: "go.mod"})
	}
	if exists(dir, "Cargo.toml") {
		out = append(out, Detected{Command: "cargo test", Timeout: 600 * time.Second, Why: "Cargo.toml"})
	}
	return out
}

func detectNode(dir string) (Detected, bool) {
	data := read(dir, "package.json")
	if data == "" {
		return Detected{}, false
	}
	var pkg struct {
		Scripts        map[string]string `json:"scripts"`
		PackageManager string            `json:"packageManager"`
	}
	if json.Unmarshal([]byte(data), &pkg) != nil {
		return Detected{}, false
	}
	test := strings.TrimSpace(pkg.Scripts["test"])
	if test == "" || test == npmDefaultTest {
		return Detected{}, false
	}
	pm := "npm"
	switch {
	case exists(dir, "pnpm-lock.yaml") || strings.HasPrefix(pkg.PackageManager, "pnpm"):
		pm = "pnpm"
	case exists(dir, "yarn.lock") || strings.HasPrefix(pkg.PackageManager, "yarn"):
		pm = "yarn"
	case exists(dir, "bun.lockb") || exists(dir, "bun.lock") || strings.HasPrefix(pkg.PackageManager, "bun"):
		// `bun test` would run bun's own runner; `bun run test` runs the script.
		return Detected{Command: "bun run test", Timeout: 300 * time.Second, Why: "package.json test script + bun lockfile"}, true
	}
	cmd := pm + " test"
	// Watch-mode runners never exit; force a single run where we can tell.
	if strings.Contains(test, "vitest") && !strings.Contains(test, "run") && !strings.Contains(test, "--run") {
		cmd += " -- --run"
	}
	why := "package.json test script"
	if pm != "npm" {
		why += " + " + pm + " lockfile"
	}
	return Detected{Command: cmd, Timeout: 300 * time.Second, Why: why}, true
}

func detectPython(dir string) (Detected, bool) {
	why := ""
	switch {
	case exists(dir, "pytest.ini"):
		why = "pytest.ini"
	case strings.Contains(read(dir, "pyproject.toml"), "pytest"):
		why = "pyproject.toml mentions pytest"
	case strings.Contains(read(dir, "tox.ini"), "pytest"):
		why = "tox.ini mentions pytest"
	case strings.Contains(read(dir, "setup.cfg"), "[tool:pytest]"):
		why = "setup.cfg [tool:pytest]"
	case exists(dir, "conftest.py"):
		why = "conftest.py"
	case hasPyTests(filepath.Join(dir, "tests")) || hasPyTests(filepath.Join(dir, "test")):
		why = "tests/ contains test_*.py"
	}
	if why == "" {
		return Detected{}, false
	}
	cmd := "python -m pytest"
	switch {
	case exists(dir, "uv.lock"):
		cmd = "uv run pytest"
	case exists(dir, "poetry.lock"):
		cmd = "poetry run pytest"
	}
	return Detected{Command: cmd, Timeout: 300 * time.Second, Why: why}, true
}

func hasPyTests(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if found {
			return filepath.SkipAll
		}
		if err != nil {
			return filepath.SkipDir
		}
		if d.IsDir() && strings.Count(strings.TrimPrefix(p, dir), string(filepath.Separator)) > 3 {
			return filepath.SkipDir
		}
		name := d.Name()
		if !d.IsDir() && strings.HasSuffix(name, ".py") && (strings.HasPrefix(name, "test_") || strings.HasSuffix(name, "_test.py")) {
			found = true
		}
		return nil
	})
	return found
}

var makeTestRe = regexp.MustCompile(`^test\s*:([^=]|$)`)

func makefileWithTest(dir string) string {
	for _, name := range []string{"GNUmakefile", "makefile", "Makefile"} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if makeTestRe.MatchString(line) {
				f.Close()
				return name
			}
			// ".PHONY: test" alone is not enough; a rule must exist.
		}
		f.Close()
	}
	return ""
}

// Render produces the .doneguard.yml contents.
func Render(cmds []Detected) string {
	var b strings.Builder
	b.WriteString("# DoneGuard config — commit this file.\n")
	b.WriteString("# The agent may only stop when every verify command passes and no\n")
	b.WriteString("# protected path was created, modified or deleted.\n")
	if len(cmds) == 0 {
		b.WriteString("#\n# No test command was detected. ADD ONE: with an empty verify list DoneGuard\n")
		b.WriteString("# only checks protected paths. Example:\n")
		b.WriteString("#\n# verify:\n#   - command: make test\n#     timeout: 300s\n")
		b.WriteString("verify: []\n")
	} else {
		b.WriteString("verify:\n")
		for _, c := range cmds {
			fmt.Fprintf(&b, "  - command: %s    # detected: %s\n", config.Quote(c.Command), c.Why)
			fmt.Fprintf(&b, "    timeout: %s\n", formatDuration(c.Timeout))
		}
	}
	b.WriteString("protected:\n")
	for _, p := range DefaultProtected {
		fmt.Fprintf(&b, "  - %s\n", config.Quote(p))
	}
	fmt.Fprintf(&b, "max_retries: %d    # blocked stops per turn before allowing as UNVERIFIED (max 7)\n", config.DefaultMaxRetries)
	b.WriteString("# optional:\n")
	b.WriteString("# verify_cwd: .          # directory to run verify commands from (default: this file's dir)\n")
	fmt.Fprintf(&b, "# output_limit: %d     # max chars of failing output to include in the block reason\n", config.DefaultOutputLimit)
	return b.String()
}

func formatDuration(d time.Duration) string {
	if d%time.Minute == 0 && d >= 10*time.Minute {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

// EnsureGitignore appends ".doneguard/" to dir/.gitignore if missing.
// It returns true when the file was changed.
func EnsureGitignore(dir string) (bool, error) {
	p, err := safepath.Resolve(dir, filepath.Join(dir, ".gitignore"))
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		switch strings.TrimSpace(line) {
		case ".doneguard", ".doneguard/", "/.doneguard", "/.doneguard/", ".doneguard/*", "/.doneguard/*":
			return false, nil
		}
	}
	var b strings.Builder
	b.Write(data)
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		b.WriteString("\n")
	}
	if len(data) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("# DoneGuard runtime data (session logs, retry state)\n.doneguard/\n")
	mode := os.FileMode(0o644)
	if st, err := os.Stat(p); err == nil {
		mode = st.Mode().Perm()
	}
	return true, os.WriteFile(p, []byte(b.String()), mode)
}
