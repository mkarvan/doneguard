package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func app(dir string, stdin string) (*App, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &App{
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb,
		Getwd:      func() (string, error) { return dir, nil },
		Getenv:     func(string) string { return "" },
		Executable: func() (string, error) { return "/usr/local/bin/doneguard", nil },
		HomeDir:    func() (string, error) { return filepath.Join(dir, "home"), nil },
	}, &out, &errb
}

func TestInitDetectsAndRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("bin/\n"), 0o644)
	a, out, errb := app(dir, "")
	if code := a.Run([]string{"init"}); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errb)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".doneguard.yml"))
	if err != nil || !strings.Contains(string(data), "go test ./...") {
		t.Fatalf("config = %s (%v)", data, err)
	}
	if !strings.Contains(out.String(), "go test ./...") || !strings.Contains(out.String(), "added .doneguard/ to .gitignore") {
		t.Errorf("out = %s", out)
	}
	gi, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if !strings.HasPrefix(string(gi), "bin/\n") || !strings.Contains(string(gi), ".doneguard/") {
		t.Errorf(".gitignore = %q", gi)
	}

	os.WriteFile(filepath.Join(dir, ".doneguard.yml"), []byte("# mine\nverify: []\n"), 0o644)
	a, _, errb = app(dir, "")
	if code := a.Run([]string{"init"}); code == 0 || !strings.Contains(errb.String(), "--force") {
		t.Fatalf("init should refuse: code=%d %s", code, errb)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, ".doneguard.yml")); string(data) != "# mine\nverify: []\n" {
		t.Error("existing config overwritten without --force")
	}
	a, _, _ = app(dir, "")
	if code := a.Run([]string{"init", "--force"}); code != 0 {
		t.Fatal("--force failed")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, ".doneguard.yml")); !strings.Contains(string(data), "go test") {
		t.Error("--force did not overwrite")
	}
}

func TestInstallUninstallViaCLI(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".doneguard.yml"), []byte("verify:\n  - command: make test\n    timeout: 10m\n"), 0o644)
	a, out, errb := app(dir, "")
	if code := a.Run([]string{"install"}); code != 0 {
		t.Fatalf("code=%d %s", code, errb)
	}
	if !strings.Contains(out.String(), "created") || !strings.Contains(out.String(), "trust") {
		t.Errorf("out = %s", out)
	}
	var settings map[string]any
	data, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	json.Unmarshal(data, &settings)
	entry := settings["hooks"].(map[string]any)["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if entry["command"] != "/usr/local/bin/doneguard hook --source claude" || entry["timeout"].(float64) != 660 {
		t.Errorf("entry = %v", entry)
	}
	a, out, _ = app(dir, "")
	a.Run([]string{"install"})
	if strings.Count(out.String(), "unchanged") != 2 {
		t.Errorf("second install: %s", out)
	}
	a, out, _ = app(dir, "")
	if code := a.Run([]string{"uninstall", "--agent", "codex"}); code != 0 || !strings.Contains(out.String(), "removed") {
		t.Fatalf("uninstall: %s", out)
	}
	data, _ = os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if !strings.Contains(string(data), "doneguard hook") {
		t.Error("uninstall --agent codex must leave claude alone")
	}
	a, _, errb = app(dir, "")
	if code := a.Run([]string{"install", "--agent", "cursor"}); code == 0 {
		t.Error("bad agent should fail")
	}
}

func TestInstallGlobalUsesHome(t *testing.T) {
	dir := t.TempDir()
	a, _, errb := app(dir, "")
	if code := a.Run([]string{"install", "--global", "--agent", "claude"}); code != 0 {
		t.Fatalf("%s", errb)
	}
	if _, err := os.Stat(filepath.Join(dir, "home", ".claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude")); err == nil {
		t.Error("--global must not write project config")
	}
}

func TestHookCommandNoConfigIsSilent(t *testing.T) {
	dir := t.TempDir()
	a, out, _ := app(dir, `{"hook_event_name":"Stop","session_id":"x","cwd":"`+filepath.ToSlash(dir)+`"}`)
	if code := a.Run([]string{"hook"}); code != 0 || out.Len() != 0 {
		t.Fatalf("code=%d out=%q", code, out)
	}
	// Bad flags must never block the agent either.
	a, out, _ = app(dir, "{}")
	if code := a.Run([]string{"hook", "--bogus"}); code != 0 || out.Len() != 0 {
		t.Fatalf("bad flag: code=%d out=%q", code, out)
	}
}

func TestHookExit2Mode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	os.WriteFile(filepath.Join(dir, ".doneguard.yml"), []byte("verify:\n  - exit 7\n"), 0o644)
	a, out, errb := app(dir, `{"hook_event_name":"Stop","session_id":"x","stop_hook_active":false}`)
	if code := a.Run([]string{"hook", "--exit2"}); code != 2 || out.Len() != 0 || !strings.Contains(errb.String(), "failed (exit 7)") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errb)
	}
}

func TestReportCommand(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".doneguard.yml"), []byte("verify: []\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".doneguard"), 0o755)
	os.WriteFile(filepath.Join(dir, ".doneguard", "sessions.jsonl"), []byte(
		`{"session_id":"a","source":"claude","decision":"block","verify_ok":false,"protected_violations":[]}
{"session_id":"a","source":"claude","decision":"allow","verify_ok":true,"protected_violations":[]}
`), 0o644)
	sub := filepath.Join(dir, "deep", "er")
	os.MkdirAll(sub, 0o755)
	a, out, errb := app(sub, "")
	if code := a.Run([]string{"report"}); code != 0 {
		t.Fatalf("%s", errb)
	}
	if !strings.Contains(out.String(), "False-done:           1 of 2 stop attempts (50.0%)") {
		t.Errorf("out = %s", out)
	}
	a, out, _ = app(sub, "")
	a.Run([]string{"report", "--json"})
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil || m["false_done_rate"].(float64) != 0.5 {
		t.Errorf("json = %s (%v)", out, err)
	}
	// No log yet: zero report, not an error.
	empty := t.TempDir()
	os.WriteFile(filepath.Join(empty, ".doneguard.yml"), []byte("verify: []\n"), 0o644)
	a, out, _ = app(empty, "")
	if code := a.Run([]string{"report"}); code != 0 || !strings.Contains(out.String(), "No stop attempts") {
		t.Errorf("empty report: %s", out)
	}
}

func TestVersionAndUnknown(t *testing.T) {
	a, out, _ := app(t.TempDir(), "")
	if a.Run([]string{"version"}) != 0 || !strings.HasPrefix(out.String(), "doneguard ") {
		t.Error(out.String())
	}
	a, _, _ = app(t.TempDir(), "")
	if a.Run([]string{"frobnicate"}) != 2 || a.Run(nil) != 2 {
		t.Error("unknown command should exit 2")
	}
	a, _, errb := app(t.TempDir(), "")
	if a.Run([]string{"run", "claude"}) == 0 || !strings.Contains(errb.String(), "only codex") {
		t.Error("run claude should fail")
	}
}

func TestHookIgnoresOversizedStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	os.WriteFile(filepath.Join(dir, ".doneguard.yml"), []byte("verify:\n  - exit 7\n"), 0o644)
	// Valid JSON whose size exceeds the cap: it must be ignored, not parsed
	// from a truncated prefix.
	big := `{"hook_event_name":"Stop","session_id":"x","cwd":"` + filepath.ToSlash(dir) + `","pad":"` + strings.Repeat("a", maxHookInput) + `"}`
	a, out, errb := app(dir, big)
	if code := a.Run([]string{"hook"}); code != 0 || out.Len() != 0 || !strings.Contains(errb.String(), "larger than") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errb)
	}
	if _, err := os.Stat(filepath.Join(dir, ".doneguard")); err == nil {
		t.Error("oversized input must not be treated as a stop")
	}
}

func TestInitRefusesSymlinkedConfigTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim.yml")
	os.Symlink(outside, filepath.Join(dir, ".doneguard.yml"))
	a, _, _ := app(dir, "")
	if code := a.Run([]string{"init", "--force"}); code == 0 {
		t.Fatal("init must not write through a symlink out of the project")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("wrote outside the project")
	}
}

func TestInitYesIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)
	a, out, errb := app(dir, "")
	if code := a.Run([]string{"init", "--yes"}); code != 0 {
		t.Fatalf("first run: code=%d stderr=%s", code, errb)
	}
	cfg := filepath.Join(dir, ".doneguard.yml")
	data, err := os.ReadFile(cfg)
	if err != nil || !strings.Contains(string(data), "go test ./...") {
		t.Fatalf("config = %s (%v)", data, err)
	}
	if !strings.Contains(out.String(), "wrote ") {
		t.Errorf("out = %s", out)
	}
	gi, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if !strings.Contains(string(gi), ".doneguard/") {
		t.Errorf(".gitignore = %q", gi)
	}

	// The user edits the config; a second --yes run must keep it and succeed.
	const mine = "# mine\nverify: []\n"
	os.WriteFile(cfg, []byte(mine), 0o644)
	for i := 0; i < 2; i++ {
		a, out, errb = app(dir, "")
		if code := a.Run([]string{"init", "--yes"}); code != 0 {
			t.Fatalf("rerun %d: code=%d stderr=%s", i, code, errb)
		}
		if !strings.Contains(out.String(), "already exists") {
			t.Errorf("rerun %d: out = %s", i, out)
		}
		if strings.Contains(out.String(), "added .doneguard/") {
			t.Errorf("rerun %d: .gitignore touched again: %s", i, out)
		}
		if data, _ := os.ReadFile(cfg); string(data) != mine {
			t.Fatalf("rerun %d: config overwritten: %q", i, data)
		}
	}
	if gi2, _ := os.ReadFile(filepath.Join(dir, ".gitignore")); string(gi2) != string(gi) {
		t.Errorf(".gitignore changed on rerun: %q", gi2)
	}

	// --force still overwrites, with or without --yes.
	a, _, _ = app(dir, "")
	if code := a.Run([]string{"init", "--yes", "--force"}); code != 0 {
		t.Fatal("--yes --force failed")
	}
	if data, _ := os.ReadFile(cfg); !strings.Contains(string(data), "go test") {
		t.Error("--yes --force did not overwrite")
	}
}

func TestInitYesExistingYamlAndFlags(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "proj")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, ".doneguard.yaml"), []byte("verify: []\n"), 0o644)
	a, out, errb := app(root, "")
	if code := a.Run([]string{"init", "-y", "--dir", dir, "--no-gitignore"}); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errb)
	}
	if !strings.Contains(out.String(), ".doneguard.yaml already exists") {
		t.Errorf("out = %s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".doneguard.yml")); err == nil {
		t.Error("wrote .doneguard.yml next to an existing .doneguard.yaml")
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); err == nil {
		t.Error("--no-gitignore not honoured")
	}

	// Fresh dir via --dir with --no-gitignore.
	fresh := filepath.Join(root, "fresh")
	os.MkdirAll(fresh, 0o755)
	a, _, errb = app(root, "")
	if code := a.Run([]string{"init", "--yes", "--dir", fresh, "--no-gitignore"}); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errb)
	}
	if _, err := os.Stat(filepath.Join(fresh, ".doneguard.yml")); err != nil {
		t.Error("config not written in --dir")
	}
	if _, err := os.Stat(filepath.Join(fresh, ".gitignore")); err == nil {
		t.Error("--no-gitignore not honoured")
	}
}

func TestUsageMentionsInitYes(t *testing.T) {
	a, out, _ := app(t.TempDir(), "")
	a.Run([]string{"help"})
	if !strings.Contains(out.String(), "--yes") {
		t.Errorf("usage does not mention --yes:\n%s", out)
	}
}

// init --yes must only accept a usable existing config. Anything else under the
// config name would make it report success while the hook finds no policy.
func TestInitYesRejectsUnusableConfigPath(t *testing.T) {
	cases := map[string]func(t *testing.T, p string){
		"directory": func(t *testing.T, p string) {
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"dangling symlink": func(t *testing.T, p string) {
			if err := os.Symlink(filepath.Join(filepath.Dir(p), "missing.yml"), p); err != nil {
				t.Skip("symlinks unavailable:", err)
			}
		},
		"symlink to directory": func(t *testing.T, p string) {
			d := filepath.Join(filepath.Dir(p), "realdir")
			os.Mkdir(d, 0o755)
			if err := os.Symlink(d, p); err != nil {
				t.Skip("symlinks unavailable:", err)
			}
		},
	}
	for name, setup := range cases {
		for _, file := range []string{".doneguard.yml", ".doneguard.yaml"} {
			t.Run(name+"/"+file, func(t *testing.T) {
				dir := t.TempDir()
				p := filepath.Join(dir, file)
				setup(t, p)
				a, out, errb := app(dir, "")
				if code := a.Run([]string{"init", "--yes"}); code == 0 {
					t.Fatalf("init --yes succeeded on %s; out=%s", name, out)
				}
				if strings.Contains(out.String(), "leaving it unchanged") {
					t.Errorf("reported an existing config: %s", out)
				}
				if !strings.Contains(errb.String(), p) {
					t.Errorf("stderr does not name %s: %s", p, errb)
				}
			})
		}
	}
}

func TestInitYesKeepsSymlinkToRegularFile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "policy.yml")
	const mine = "verify: []\n"
	os.WriteFile(real, []byte(mine), 0o644)
	link := filepath.Join(dir, ".doneguard.yml")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	a, out, errb := app(dir, "")
	if code := a.Run([]string{"init", "--yes", "--no-gitignore"}); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errb)
	}
	if !strings.Contains(out.String(), "leaving it unchanged") {
		t.Errorf("out = %s", out)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink replaced: %v %v", fi, err)
	}
	if data, _ := os.ReadFile(real); string(data) != mine {
		t.Errorf("target modified: %q", data)
	}
}

func TestInitPropagatesLstatErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "locked")
	os.Mkdir(dir, 0o755)
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Skip(err)
	}
	defer os.Chmod(dir, 0o755)
	a, _, errb := app(dir, "")
	if code := a.Run([]string{"init", "--yes"}); code == 0 {
		t.Fatal("init --yes succeeded in an unreadable directory")
	}
	if !strings.Contains(errb.String(), "permission denied") {
		t.Errorf("stderr = %s", errb)
	}
}
