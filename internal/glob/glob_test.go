package glob

import "testing"

var defaults = []string{".env", ".env.*", "**/*secret*", "**/*credential*", "**/id_rsa", "**/*.pem"}

func TestDefaultProtectedGlobs(t *testing.T) {
	m := MustCompile(defaults...)
	cases := map[string]bool{
		".env":                      true,
		"config/.env":               true,
		".env.local":                true,
		"deploy/.env.production":    true,
		"secrets.yaml":              true,
		"config/db_secret.json":     true,
		"src/credentials.go":        true,
		"home/.ssh/id_rsa":          true,
		"id_rsa":                    true,
		"certs/server.pem":          true,
		"server.pem":                true,
		"README.md":                 false,
		"src/main.go":               false,
		"id_rsa.pub":                false,
		".envrc":                    false,
		"environment.go":            false,
		"docs/secrets/overview.md":  false, // wildcard patterns match names, not directories
		"internal/pem/parse.go":     false,
		"node_modules/x/.env.local": true,
	}
	for p, want := range cases {
		if got := m.Match(p); got != want {
			t.Errorf("Match(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestDoublestarAndAnchoring(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"config/**", "config/a/b.yml", true},
		{"config/**", "config", false},
		{"config/**", "x/config/a", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/y/c", false},
		{"/Makefile", "Makefile", true},
		{"/Makefile", "sub/Makefile", false},
		{"Makefile", "sub/Makefile", true},
		{"src/*.go", "src/a.go", true},
		{"src/*.go", "src/x/a.go", false},
		{"secrets/", "secrets/db.yml", true},
		{"secrets/", "app/secrets/db.yml", true},
		{"secrets/", "secrets", false},
		{"secrets", "secrets", true},
		{"secrets", "secrets/k/db.yml", true},
		{"config/prod", "config/prod/app.yml", true},
		{"config/prod", "x/config/prod/app.yml", false},
		{"*.{pem,key}", "a/b.key", true},
		{"*.{pem,key}", "a/b.crt", false},
		{"migrations/*.sql", "migrations/001.sql", true},
		{"file?.txt", "file1.txt", true},
		{"file[0-9].txt", "filea.txt", false},
		{"./.env", ".env", true},
	}
	for _, c := range cases {
		m := MustCompile(c.pat)
		if got := m.Match(c.path); got != c.want {
			t.Errorf("pattern %q path %q = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestNegationLastMatchWins(t *testing.T) {
	m := MustCompile(".env", ".env.*", "!.env.example")
	if !m.Match(".env.local") {
		t.Error(".env.local should be protected")
	}
	if m.Match(".env.example") {
		t.Error(".env.example should be un-protected")
	}
	if m.Match("app/.env.example") {
		t.Error("app/.env.example should be un-protected")
	}
	by, ok := m.MatchRule("x/.env")
	if !ok || by != ".env" {
		t.Errorf("MatchRule = %q %v", by, ok)
	}
}

func TestCompileErrorsAndEmpty(t *testing.T) {
	if _, err := Compile([]string{"[abc"}); err == nil {
		t.Error("expected bad pattern error")
	}
	m, err := Compile([]string{"", "  ", "# comment"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Match(".env") {
		t.Error("empty matcher should not match")
	}
	var nilM *Matcher
	if nilM.Match("x") {
		t.Error("nil matcher should not match")
	}
}

func TestExpandBraces(t *testing.T) {
	got := expandBraces("a/{b,c}/{d,e{f,g}}")
	want := []string{"a/b/d", "a/b/ef", "a/b/eg", "a/c/d", "a/c/ef", "a/c/eg"}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q want %q", got, want)
		}
	}
}
