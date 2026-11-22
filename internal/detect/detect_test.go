package detect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mkarvan/doneguard/internal/config"
)

func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func cmds(ds []Detected) string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Command)
	}
	return strings.Join(out, " | ")
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"npm", map[string]string{"package.json": `{"scripts":{"test":"jest"}}`}, "npm test"},
		{"npm default placeholder ignored", map[string]string{"package.json": `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`}, ""},
		{"npm without test script", map[string]string{"package.json": `{"scripts":{"build":"tsc"}}`}, ""},
		{"pnpm", map[string]string{"package.json": `{"scripts":{"test":"jest"}}`, "pnpm-lock.yaml": ""}, "pnpm test"},
		{"yarn", map[string]string{"package.json": `{"scripts":{"test":"jest"}}`, "yarn.lock": ""}, "yarn test"},
		{"packageManager field", map[string]string{"package.json": `{"packageManager":"pnpm@9.0.0","scripts":{"test":"jest"}}`}, "pnpm test"},
		{"bun", map[string]string{"package.json": `{"scripts":{"test":"vitest run"}}`, "bun.lock": ""}, "bun run test"},
		{"vitest watch mode", map[string]string{"package.json": `{"scripts":{"test":"vitest"}}`}, "npm test -- --run"},
		{"pytest.ini", map[string]string{"pytest.ini": "[pytest]\n"}, "python -m pytest"},
		{"pyproject pytest", map[string]string{"pyproject.toml": "[tool.pytest.ini_options]\n"}, "python -m pytest"},
		{"pyproject without pytest", map[string]string{"pyproject.toml": "[project]\nname='x'\n"}, ""},
		{"tox", map[string]string{"tox.ini": "[testenv]\ncommands = pytest\n"}, "python -m pytest"},
		{"tests dir", map[string]string{"tests/unit/test_math.py": "def test_x(): pass\n"}, "python -m pytest"},
		{"uv", map[string]string{"pytest.ini": "", "uv.lock": ""}, "uv run pytest"},
		{"poetry", map[string]string{"pytest.ini": "", "poetry.lock": ""}, "poetry run pytest"},
		{"go", map[string]string{"go.mod": "module x\n"}, "go test ./..."},
		{"cargo", map[string]string{"Cargo.toml": "[package]\n"}, "cargo test"},
		{"make", map[string]string{"Makefile": ".PHONY: test\nbuild:\n\tgo build\ntest: build\n\tgo test ./...\n"}, "make test"},
		{"make wins over go", map[string]string{"Makefile": "test:\n\tgo test ./...\n", "go.mod": "module x\n"}, "make test"},
		{"make without test target", map[string]string{"Makefile": "TEST := 1\ntest_data:\n\techo\nbuild:\n\techo\n", "go.mod": "module x\n"}, "go test ./..."},
		{"make variable named test is not a target", map[string]string{"Makefile": "test := 1\n"}, ""},
		{"polyglot", map[string]string{"package.json": `{"scripts":{"test":"jest"}}`, "go.mod": "module x\n", "Cargo.toml": ""}, "npm test | go test ./... | cargo test"},
		{"nothing", map[string]string{"README.md": "hi"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cmds(Detect(fixture(t, c.files))); got != c.want {
				t.Errorf("Detect = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRenderParsesBack(t *testing.T) {
	src := Render([]Detected{{Command: "go test ./...", Timeout: 300e9, Why: "go.mod"}, {Command: "npm test -- --run", Timeout: 600e9, Why: "x: y"}})
	cfg, err := config.Parse([]byte(src), "/p")
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	if len(cfg.Verify) != 2 || cfg.Verify[0].Command != "go test ./..." || cfg.Verify[1].Command != "npm test -- --run" || cfg.Verify[1].Timeout.Minutes() != 10 {
		t.Errorf("verify = %+v", cfg.Verify)
	}
	if strings.Join(cfg.Protected, ",") != strings.Join(DefaultProtected, ",") {
		t.Errorf("protected = %q", cfg.Protected)
	}
	if cfg.MaxRetries != 3 {
		t.Errorf("max_retries = %d", cfg.MaxRetries)
	}
}

func TestRenderEmptyHasPlaceholder(t *testing.T) {
	src := Render(nil)
	if !strings.Contains(src, "verify: []") || !strings.Contains(src, "ADD ONE") {
		t.Errorf("src = %s", src)
	}
	cfg, err := config.Parse([]byte(src), "/p")
	if err != nil || len(cfg.Verify) != 0 || len(cfg.Protected) == 0 {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

func TestEnsureGitignore(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".gitignore")
	os.WriteFile(p, []byte("node_modules\n*.log"), 0o644)
	changed, err := EnsureGitignore(dir)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(data), "node_modules\n*.log\n") || !strings.HasSuffix(string(data), ".doneguard/\n") {
		t.Errorf("gitignore = %q", data)
	}
	if changed, _ := EnsureGitignore(dir); changed {
		t.Error("second call should be a no-op")
	}
	empty := t.TempDir()
	if changed, _ := EnsureGitignore(empty); !changed {
		t.Error("should create .gitignore")
	}
}
