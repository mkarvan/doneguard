package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseFullConfig(t *testing.T) {
	src := `# DoneGuard config
verify:
  - command: go test ./...
    timeout: 120s
  - command: "npm test -- --grep 'a b'"   # trailing comment
  - make lint
protected:
  - ".env"
  - '.env.*'
  - "**/*secret*"
  - **/id_rsa
max_retries: 5
verify_cwd: sub
output_limit: 1000
`
	cfg, err := Parse([]byte(src), "/proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Verify) != 3 {
		t.Fatalf("verify = %+v", cfg.Verify)
	}
	if cfg.Verify[0].Command != "go test ./..." || cfg.Verify[0].Timeout != 120*time.Second {
		t.Errorf("verify[0] = %+v", cfg.Verify[0])
	}
	if cfg.Verify[1].Command != "npm test -- --grep 'a b'" || cfg.Verify[1].Timeout != DefaultTimeout {
		t.Errorf("verify[1] = %+v", cfg.Verify[1])
	}
	if cfg.Verify[2].Command != "make lint" {
		t.Errorf("verify[2] = %+v", cfg.Verify[2])
	}
	want := []string{".env", ".env.*", "**/*secret*", "**/id_rsa"}
	if strings.Join(cfg.Protected, "|") != strings.Join(want, "|") {
		t.Errorf("protected = %q", cfg.Protected)
	}
	if cfg.MaxRetries != 5 || cfg.OutputLimit != 1000 {
		t.Errorf("max_retries=%d output_limit=%d", cfg.MaxRetries, cfg.OutputLimit)
	}
	if cfg.VerifyCwd != filepath.Join("/proj", "sub") {
		t.Errorf("verify_cwd = %q", cfg.VerifyCwd)
	}
}

func TestParseDefaultsAndEmpty(t *testing.T) {
	for _, src := range []string{"", "# only comments\n", "verify: []\nprotected: []\n", "---\nverify:\n"} {
		cfg, err := Parse([]byte(src), "/p")
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if len(cfg.Verify) != 0 || cfg.MaxRetries != DefaultMaxRetries || cfg.OutputLimit != DefaultOutputLimit {
			t.Errorf("%q: %+v", src, cfg)
		}
	}
}

func TestParseSeqAtSameIndentAndFlow(t *testing.T) {
	src := "verify:\n- command: pytest\n  timeout: 2m\nprotected: [\".env\", secrets/, '*.pem']\nmax_retries: 0\n"
	cfg, err := Parse([]byte(src), "/p")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Verify) != 1 || cfg.Verify[0].Command != "pytest" || cfg.Verify[0].Timeout != 2*time.Minute {
		t.Errorf("verify = %+v", cfg.Verify)
	}
	if strings.Join(cfg.Protected, "|") != ".env|secrets/|*.pem" {
		t.Errorf("protected = %q", cfg.Protected)
	}
	if cfg.MaxRetries != 0 || cfg.EffectiveMaxRetries() != 0 {
		t.Errorf("max_retries = %d", cfg.MaxRetries)
	}
}

func TestParseBlockScalar(t *testing.T) {
	src := "verify:\n  - command: |\n      set -e\n      go vet ./...\n\n      go test ./...\n    timeout: 30\n"
	cfg, err := Parse([]byte(src), "/p")
	if err != nil {
		t.Fatal(err)
	}
	want := "set -e\ngo vet ./...\n\ngo test ./..."
	if cfg.Verify[0].Command != want {
		t.Errorf("got %q want %q", cfg.Verify[0].Command, want)
	}
	if cfg.Verify[0].Timeout != 30*time.Second {
		t.Errorf("timeout = %v", cfg.Verify[0].Timeout)
	}
	folded, err := parseYAML("a: >-\n  one\n  two\n\n  three\nb: x\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := folded.get("a").value; got != "one two\nthree" {
		t.Errorf("folded = %q", got)
	}
	if folded.get("b").value != "x" {
		t.Errorf("b = %q", folded.get("b").value)
	}
}

func TestParseQuotedEscapes(t *testing.T) {
	n, err := parseYAML(`k: "a\tb \"q\" é # not a comment"` + "\nj: 'it''s'\nh: don't # c\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := n.get("k").value; got != "a\tb \"q\" é # not a comment" {
		t.Errorf("k = %q", got)
	}
	if got := n.get("j").value; got != "it's" {
		t.Errorf("j = %q", got)
	}
	if got := n.get("h").value; got != "don't" {
		t.Errorf("h = %q", got)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"verfy:\n  - x\n":                              "unknown key",
		"verify: go test\n":                            "verify must be a list",
		"verify:\n  - command: x\n    tmo: 1\n":        "unknown verify key",
		"max_retries: lots\n":                          "max_retries must be an integer",
		"max_retries: -1\n":                            ">= 0",
		"verify:\n\t- x\n":                             "tab",
		"verify:\n  - command: x\n    timeout: soon\n": "invalid timeout",
		"a: 1\na: 2\n":                                 "duplicate key",
		"a: \"unterminated\n":                          "unterminated",
		"- a\n- b\n":                                   "top level must be a mapping",
		"a: 1\n  b: 2\n":                               "unexpected indentation",
		"verify:\n  - command: \"\"\n":                 "empty command",
		"a: &x 1\n":                                    "anchors",
	}
	for src, want := range cases {
		_, err := Parse([]byte(src), "/p")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) error = %v, want containing %q", src, err, want)
		}
	}
}

func TestParseErrorHasLineNumber(t *testing.T) {
	_, err := Parse([]byte("verify:\n  - command: ok\nbogus: 1\n"), "/p")
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("err = %v", err)
	}
}

func TestEffectiveMaxRetriesClamped(t *testing.T) {
	cfg, err := Parse([]byte("max_retries: 50\n"), "/p")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EffectiveMaxRetries() != HardMaxBlocks {
		t.Errorf("effective = %d", cfg.EffectiveMaxRetries())
	}
}

func TestFindWalksUp(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Find(deep); got != "" && strings.HasPrefix(got, root) {
		t.Fatalf("unexpected config %q", got)
	}
	cfgPath := filepath.Join(root, "a", ".doneguard.yml")
	if err := os.WriteFile(cfgPath, []byte("verify: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Find(deep); got != cfgPath {
		t.Fatalf("Find = %q, want %q", got, cfgPath)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Dir != filepath.Join(root, "a") || cfg.LogPath() != filepath.Join(root, "a", ".doneguard", "sessions.jsonl") {
		t.Errorf("dir=%q log=%q", cfg.Dir, cfg.LogPath())
	}
}

func TestQuoteRoundTrip(t *testing.T) {
	for _, s := range []string{"go test ./...", ".env", "**/*secret*", "a: b", "it's", "#x", "true", " x"} {
		src := "k: " + Quote(s) + "\n"
		n, err := parseYAML(src)
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if got := n.get("k").value; got != s {
			t.Errorf("round trip %q -> %q -> %q", s, src, got)
		}
	}
}

func TestMalformedYAMLReturnsErrorNotPanic(t *testing.T) {
	for _, src := range []string{
		"verify: {",
		"protected: {",
		"verify:\n  - {\n",
		"verify:\n  - command: {\n",
		"{",
		"verify: |2\n    go test\n",
		"verify: |-+\n  go test\n",
		"verify:\n  - command: a\n    run: b\n",
		"verify:\n  - command: a\n    timeout: 99999999999999\n",
		"verify:\n  - &x go test\n",
		"verify:\n  - !!str go test\n",
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Parse(%q) panicked: %v", src, r)
				}
			}()
			if _, err := Parse([]byte(src), "/p"); err == nil {
				t.Errorf("Parse(%q) should fail", src)
			}
		}()
	}
}
