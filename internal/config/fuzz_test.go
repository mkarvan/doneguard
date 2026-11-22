package config

import "testing"

// FuzzParse checks that no input makes the parser panic; seeds run as part
// of `go test`.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"{", "[", "\"", "'", "verify: {", "verify:\n  - {", "- {", "a: [", "a: [{", "a: ]",
		"verify: |", "verify: |9\n  x", "verify:\n  - |-\n    go test\n", "a: \"\\", "? x",
		"verify:\n- - - {", ":", "- :", "a:\n  b: {\n", "verify: [\"a\", {]",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = Parse([]byte(s), "/tmp")
	})
}
