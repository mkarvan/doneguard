package verify

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mkarvan/doneguard/internal/config"
)

func TestTrimShortOutputUnchanged(t *testing.T) {
	in := "\x1b[31mFAIL\x1b[0m  pkg   \r\nok\n"
	if got := Trim(in, 4000); got != "FAIL  pkg\nok" {
		t.Errorf("got %q", got)
	}
}

func TestTrimKeepsTailAndFailureLines(t *testing.T) {
	var lines []string
	for i := 0; i < 2000; i++ {
		lines = append(lines, fmt.Sprintf("=== RUN   TestThing%d", i))
		if i == 37 {
			lines = append(lines, "    add_test.go:8: expected 5, got 4", "--- FAIL: TestThing37 (0.00s)")
		}
	}
	lines = append(lines, "FAIL", "FAIL\tsample\t0.004s")
	out := Trim(strings.Join(lines, "\n"), 1500)
	if len(out) > 1500 {
		t.Fatalf("len = %d > limit", len(out))
	}
	for _, want := range []string{"add_test.go:8: expected 5, got 4", "--- FAIL: TestThing37", "FAIL\tsample\t0.004s", "lines omitted"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The tail must be the very end of the output.
	if !strings.HasSuffix(out, "FAIL\tsample\t0.004s") {
		t.Errorf("tail not preserved: %q", out[len(out)-60:])
	}
}

func TestTrimHugeSingleLineAndUTF8(t *testing.T) {
	in := strings.Repeat("é", 10000) + "END"
	out := Trim(in, 500)
	if len(out) > 500 {
		t.Fatalf("len = %d", len(out))
	}
	if !utf8.ValidString(out) {
		t.Fatal("invalid utf8 after trimming")
	}
	if !strings.HasSuffix(out, "END") {
		t.Errorf("suffix lost: %q", out[len(out)-10:])
	}
}

func TestTrimBoundedForManyFailureLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "error: thing %d failed badly with a long explanation\n", i)
	}
	for _, limit := range []int{200, 1000, 4000} {
		out := Trim(b.String(), limit)
		if len(out) > limit {
			t.Errorf("limit %d: len %d", limit, len(out))
		}
	}
}

func TestRunSuccessFailureAndOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	dir := t.TempDir()
	r := Run("echo hello; echo oops >&2; exit 3", dir, 10*time.Second, nil)
	if r.OK() || r.ExitCode != 3 {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(r.Output, "hello") || !strings.Contains(r.Output, "oops") {
		t.Errorf("output = %q", r.Output)
	}
	if r.Summary() != "`echo hello; echo oops >&2; exit 3` failed (exit 3)" {
		t.Errorf("summary = %q", r.Summary())
	}
	r = Run("pwd; test \"$DONEGUARD\" = 1", dir, 10*time.Second, nil)
	if !r.OK() {
		t.Fatalf("expected ok: %+v", r)
	}
	real, _ := filepath.EvalSymlinks(dir)
	if !strings.Contains(r.Output, real) && !strings.Contains(r.Output, dir) {
		t.Errorf("command did not run in dir: %q", r.Output)
	}
}

func TestRunTimeoutKillsProcessTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	start := time.Now()
	r := Run("sleep 30 & sleep 30; echo never", t.TempDir(), 300*time.Millisecond, nil)
	if !r.TimedOut || r.OK() {
		t.Fatalf("expected timeout: %+v", r)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout took too long: %v", time.Since(start))
	}
	if !strings.Contains(r.Summary(), "timed out after 300ms") {
		t.Errorf("summary = %q", r.Summary())
	}
}

func TestRunWaitDelayIsNotSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	old := waitDelay
	waitDelay = 200 * time.Millisecond
	defer func() { waitDelay = old }()
	// The shell exits 0 while a background child still holds stdout.
	r := Run("sleep 5 & echo started; exit 0", t.TempDir(), 10*time.Second, nil)
	if r.OK() || !r.Incomplete || r.TimedOut {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(r.Summary(), "did not finish cleanly") {
		t.Errorf("summary = %q", r.Summary())
	}
	if r.Duration > 4*time.Second {
		t.Errorf("took %s; descendants should be killed", r.Duration)
	}
}

func TestRunAllStopsAtFirstFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "third-ran")
	cmds := []config.Command{
		{Command: "true", Timeout: 5 * time.Second},
		{Command: "exit 2", Timeout: 5 * time.Second},
		{Command: "touch " + marker, Timeout: 5 * time.Second},
	}
	results, failed := RunAll(cmds, dir)
	if failed == nil || failed.Command != "exit 2" || len(results) != 2 {
		t.Fatalf("failed=%+v results=%d", failed, len(results))
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("third command should not have run")
	}
	_, failed = RunAll(cmds[:1], dir)
	if failed != nil {
		t.Errorf("unexpected failure %+v", failed)
	}
}

func TestRunMissingDirIsStartError(t *testing.T) {
	r := Run("true", filepath.Join(t.TempDir(), "nope"), time.Second, nil)
	if r.OK() || r.StartErr == "" {
		t.Fatalf("expected start error: %+v", r)
	}
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{max: 10}
	fmt.Fprint(tb, "0123456789abcdef")
	if got := tb.String(); !strings.HasSuffix(got, "6789abcdef") || !strings.Contains(got, "6 bytes") {
		t.Errorf("got %q", got)
	}
}
