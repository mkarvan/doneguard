// Package glob implements gitignore-flavoured doublestar matching for
// DoneGuard's protected path patterns.
//
// Rules (paths are slash-separated and relative to the project root):
//
//   - A pattern without a slash (".env", "*.pem") matches the file name at
//     any depth.
//   - A pattern with a slash is matched against the whole path; "**" matches
//     zero or more directories ("**/id_rsa", "config/**", "a/**/b").
//   - A leading "/" anchors to the project root (it is otherwise implied for
//     patterns containing a slash).
//   - A trailing "/" ("secrets/") matches everything inside that directory.
//     A pattern with no wildcards ("secrets", "config/prod") also matches
//     everything inside a directory of that name.
//   - "*", "?" and "[...]" work within a single path segment; "{a,b}"
//     alternatives are expanded.
//   - A leading "!" negates: later matching rules win, so "!.env.example"
//     after ".env.*" un-protects the example file.
package glob

import (
	"fmt"
	"path"
	"strings"
)

type rule struct {
	raw      string
	negate   bool
	dirOnly  bool
	hasSlash bool
	literal  bool
	segs     []string
}

// Matcher matches paths against an ordered list of patterns.
type Matcher struct {
	rules []rule
}

// Compile compiles patterns. Empty patterns and "#" comments are ignored.
func Compile(patterns []string) (*Matcher, error) {
	m := &Matcher{}
	for _, raw := range patterns {
		p := strings.TrimSpace(raw)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		negate := false
		if strings.HasPrefix(p, "!") {
			negate = true
			p = p[1:]
		}
		p = strings.ReplaceAll(p, "\\", "/")
		for _, exp := range expandBraces(p) {
			r, err := compileOne(exp)
			if err != nil {
				return nil, fmt.Errorf("invalid protected pattern %q: %w", raw, err)
			}
			r.raw = raw
			r.negate = negate
			m.rules = append(m.rules, r)
		}
	}
	return m, nil
}

// MustCompile is Compile that panics; for tests and constants.
func MustCompile(patterns ...string) *Matcher {
	m, err := Compile(patterns)
	if err != nil {
		panic(err)
	}
	return m
}

func compileOne(p string) (rule, error) {
	var r rule
	p = strings.TrimPrefix(p, "./")
	if strings.HasPrefix(p, "/") {
		p = strings.TrimLeft(p, "/")
		r.hasSlash = true
	}
	if strings.HasSuffix(p, "/") {
		r.dirOnly = true
		p = strings.TrimRight(p, "/")
	}
	if p == "" {
		return r, fmt.Errorf("empty pattern")
	}
	if strings.Contains(p, "/") {
		r.hasSlash = true
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			continue
		}
		if seg != "**" {
			if strings.Contains(seg, "**") {
				// "a**b" behaves like "a*b" within one segment.
				for strings.Contains(seg, "**") {
					seg = strings.ReplaceAll(seg, "**", "*")
				}
			}
			if _, err := path.Match(seg, ""); err != nil {
				return r, err
			}
		}
		r.segs = append(r.segs, seg)
	}
	r.literal = !strings.ContainsAny(p, "*?[")
	return r, nil
}

// Match reports whether the slash-separated relative path is protected.
func (m *Matcher) Match(p string) bool {
	_, ok := m.MatchRule(p)
	return ok
}

// MatchRule is like Match but also returns the (last) pattern that matched.
func (m *Matcher) MatchRule(p string) (string, bool) {
	if m == nil {
		return "", false
	}
	p = strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "./")
	comps := splitPath(p)
	if len(comps) == 0 {
		return "", false
	}
	matched, by := false, ""
	for _, r := range m.rules {
		if r.matches(comps) {
			matched = !r.negate
			by = r.raw
		}
	}
	if !matched {
		return "", false
	}
	return by, true
}

func splitPath(p string) []string {
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return out
}

func (r rule) matches(comps []string) bool {
	// Directory matches: any proper ancestor directory matching the pattern
	// means the file is inside a protected directory.
	if r.dirOnly || r.literal {
		for i := 1; i < len(comps); i++ {
			if r.matchExact(comps[:i]) {
				return true
			}
		}
		if r.dirOnly {
			return false
		}
	}
	return r.matchExact(comps)
}

func (r rule) matchExact(comps []string) bool {
	if !r.hasSlash {
		ok, _ := path.Match(r.segs[0], comps[len(comps)-1])
		return ok
	}
	return matchSegs(r.segs, comps)
}

func matchSegs(pat, comps []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for len(pat) > 0 && pat[0] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 0 {
				return len(comps) > 0
			}
			for i := 0; i <= len(comps); i++ {
				if matchSegs(pat, comps[i:]) {
					return true
				}
			}
			return false
		}
		if len(comps) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], comps[0]); !ok {
			return false
		}
		pat, comps = pat[1:], comps[1:]
	}
	return len(comps) == 0
}

// expandBraces expands "{a,b}" alternatives (non-nested groups are expanded
// left to right; nested groups are expanded recursively).
func expandBraces(p string) []string {
	start := strings.IndexByte(p, '{')
	if start < 0 {
		return []string{p}
	}
	depth := 0
	end := -1
	for i := start; i < len(p); i++ {
		switch p[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return []string{p}
	}
	var alts []string
	depth = 0
	last := start + 1
	for i := start + 1; i < end; i++ {
		switch p[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				alts = append(alts, p[last:i])
				last = i + 1
			}
		}
	}
	alts = append(alts, p[last:end])
	if len(alts) == 1 {
		// "{x}" with no comma: treat literally.
		var out []string
		for _, rest := range expandBraces(p[end+1:]) {
			out = append(out, p[:end+1]+rest)
		}
		return out
	}
	var out []string
	for _, a := range alts {
		out = append(out, expandBraces(p[:start]+a+p[end+1:])...)
	}
	return out
}
