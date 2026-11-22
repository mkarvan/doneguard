package verify

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07`)
	// failRe picks out lines that usually explain a failure across common
	// test runners (go test, pytest, jest/vitest, cargo, make, tsc, eslint).
	failRe = regexp.MustCompile(`(?i)(--- FAIL|^FAIL\b|\bFAILED\b|\bfail(ed|ure|ures|ing)?:|\berror(s)?\b|\bpanic:|\bexception\b|traceback|assert|\bexpected\b|\bnot ok\b|^\s*[✗✘×●]|\bundefined:|cannot find|no such file|\bE\s{2,})`)
	// noiseRe suppresses lines that match failRe but rarely help.
	noiseRe = regexp.MustCompile(`(?i)^\s*(ok\s|PASS\b|=+ \d+ passed|\d+ passing)`)
)

// Clean strips ANSI escapes, normalises newlines and trailing whitespace.
func Clean(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	// Progress bars rewrite the line with bare \r; keep the final state.
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if j := strings.LastIndexByte(l, '\r'); j >= 0 {
			l = l[j+1:]
		}
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// Trim condenses command output to at most limit bytes. When the output is
// too long it keeps (a) the lines that look like failures from the part that
// would otherwise be cut, and (b) as much of the tail as fits, because test
// runners print their summary last.
func Trim(output string, limit int) string {
	if limit <= 0 {
		limit = 4000
	}
	s := Clean(output)
	if len(s) <= limit {
		return s
	}
	lines := strings.Split(s, "\n")

	// Tail: up to ~60% of the budget, whole lines where possible.
	tailBudget := limit * 3 / 5
	tailStart := len(lines)
	used := 0
	for tailStart > 0 {
		l := lines[tailStart-1]
		if used+len(l)+1 > tailBudget {
			break
		}
		used += len(l) + 1
		tailStart--
	}
	var tail string
	if tailStart == len(lines) {
		// A single enormous last line: keep its end.
		tail = "…" + safeSuffix(lines[len(lines)-1], tailBudget-len("…"))
		tailStart = len(lines) - 1
	} else {
		tail = strings.Join(lines[tailStart:], "\n")
	}

	// Relevant earlier lines (plus one line of context after each).
	marker := func(n int) string { return fmt.Sprintf("[... %d lines omitted ...]", n) }
	relBudget := limit - len(tail) - 2*len(marker(len(lines))) - 4
	var rel []string
	relUsed := 0
	lastIncluded := -1
	for i := 0; i < tailStart && relBudget > 0; i++ {
		if !failRe.MatchString(lines[i]) || noiseRe.MatchString(lines[i]) {
			continue
		}
		for j := i; j <= i+1 && j < tailStart; j++ {
			if j <= lastIncluded {
				continue
			}
			l := lines[j]
			if len(l) > 300 {
				l = safePrefix(l, 300) + "…"
			}
			if relUsed+len(l)+1 > relBudget {
				relBudget = 0
				break
			}
			if lastIncluded >= 0 && j > lastIncluded+1 {
				rel = append(rel, "…")
				relUsed += len("…") + 1
			}
			rel = append(rel, l)
			relUsed += len(l) + 1
			lastIncluded = j
		}
	}

	var b strings.Builder
	if len(rel) > 0 {
		b.WriteString(marker(countOmitted(lines, tailStart, rel)) + "\n")
		b.WriteString(strings.Join(rel, "\n"))
		b.WriteString("\n[... output continues; last lines follow ...]\n")
	} else {
		b.WriteString(marker(tailStart) + "\n")
	}
	b.WriteString(tail)
	out := b.String()
	if len(out) > limit {
		out = "…" + safeSuffix(out, limit-len("…"))
	}
	return out
}

func countOmitted(lines []string, tailStart int, rel []string) int {
	n := tailStart
	for _, r := range rel {
		if r != "…" {
			n--
		}
	}
	if n < 0 {
		return 0
	}
	return n
}

func safePrefix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n < 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func safeSuffix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n < 0 {
		return ""
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}
