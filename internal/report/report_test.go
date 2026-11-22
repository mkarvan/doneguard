package report

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/mkarvan/doneguard/internal/sessionlog"
)

const logFixture = `{"timestamp":"2026-11-20T10:00:00Z","session_id":"A","source":"claude","hook_event_name":"Stop","decision":"block","block_kind":"verify","verify_ok":false,"protected_violations":[],"retry_count":1,"total_tokens":1000,"input_tokens":900,"output_tokens":100}
{"timestamp":"2026-11-20T10:05:00Z","session_id":"A","source":"claude","hook_event_name":"Stop","decision":"block","block_kind":"verify","verify_ok":false,"protected_violations":[],"retry_count":2,"total_tokens":1600}
{"timestamp":"2026-11-20T10:09:00Z","session_id":"A","source":"claude","hook_event_name":"Stop","decision":"allow","verify_ok":true,"protected_violations":[],"retry_count":0,"total_tokens":2000}
{"timestamp":"2026-11-21T09:00:00Z","session_id":"B","source":"codex","hook_event_name":"Stop","decision":"block","block_kind":"protected","verify_ok":true,"protected_violations":[".env"],"retry_count":1,"total_tokens":500}
{"timestamp":"2026-11-21T09:01:00Z","session_id":"B","source":"codex","hook_event_name":"Stop","decision":"allow","unverified":true,"verify_ok":false,"protected_violations":[],"retry_count":3,"total_tokens":800}
{"timestamp":"2026-11-21T09:02:00Z","session_id":"C","source":"claude","hook_event_name":"Stop","decision":"allow","verify_skipped":true,"verify_ok":null,"protected_violations":[]}
{"timestamp":"2026-11-21T09:03:00Z","session_id":"C","source":"claude","hook_event_name":"Stop","decision":"block","block_kind":"config","verify_ok":null,"protected_violations":[]}
garbage line
{"timestamp":"2026-11-22T00:00:00Z","session_id":"W","source":"wrapper","hook_event_name":"WrapperStop","decision":"allow","verify_ok":true,"protected_violations":[]}
`

func TestAggregate(t *testing.T) {
	entries, bad, err := sessionlog.Read(strings.NewReader(logFixture))
	if err != nil {
		t.Fatal(err)
	}
	s := Aggregate(entries)
	s.MalformedLines = bad
	check := func(name string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	check("stop attempts", s.StopAttempts, 8)
	check("sessions", s.Sessions, 4)
	check("blocked", s.Blocked, 4)
	check("allowed", s.Allowed, 4)
	check("blocked verify", s.BlockedVerify, 2)
	check("blocked protected", s.BlockedProtect, 1)
	check("blocked config", s.BlockedConfig, 1)
	// false-dones (blocked because checks failed): A1, A2, B1 (protected).
	// B2 is an UNVERIFIED allow at the cap, not an extra false-done; C2 is a
	// config error.
	check("false done", s.FalseDone, 3)
	check("allowed verified", s.AllowedVerified, 2)
	check("allowed unverified", s.AllowedUnverified, 1)
	check("allowed no verify", s.AllowedNoVerify, 1)
	check("unverified sessions", s.UnverifiedSessions, 1)
	check("malformed", s.MalformedLines, 1)
	if math.Abs(s.FalseDoneRate-0.375) > 1e-9 {
		t.Errorf("rate = %v", s.FalseDoneRate)
	}
	// Tokens: A deltas 1000 (fd), 600 (fd), 400; B deltas 500 (fd), 300 (fd).
	if s.TokensTotal == nil || *s.TokensTotal != 2800 {
		t.Fatalf("tokens total = %v", s.TokensTotal)
	}
	if *s.TokensOnFalseDone != 2100 {
		t.Errorf("tokens on false done = %d", *s.TokensOnFalseDone)
	}
	if s.BySource["claude"] != 5 || s.BySource["codex"] != 2 || s.BySource["wrapper"] != 1 {
		t.Errorf("by source = %v", s.BySource)
	}

	var text bytes.Buffer
	s.WriteText(&text)
	for _, want := range []string{"False-done:           3 of 8 stop attempts (37.5%)", "UNVERIFIED 1", "2,800 total; 2,100 (75.0%)", "malformed"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text missing %q:\n%s", want, text.String())
		}
	}
	var js bytes.Buffer
	if err := s.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(js.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["false_done"].(float64) != 3 || m["tokens_on_false_done"].(float64) != 2100 || m["allowed_unverified"].(float64) != 1 {
		t.Errorf("json = %s", js.String())
	}
}

func TestIndeterminateAndUnverifiedAreNotFalseDone(t *testing.T) {
	f := false
	s := Aggregate([]sessionlog.Entry{
		{SessionID: "i", Decision: "block", BlockKind: "indeterminate"},
		{SessionID: "u", Decision: "allow", Unverified: true, VerifyOK: &f},
		{SessionID: "p", Decision: "block", BlockKind: "protected", ProtectedViolations: []string{".doneguard.yml"}},
	})
	if s.FalseDone != 1 || s.BlockedIndeterminate != 1 || s.AllowedUnverified != 1 || s.BlockedProtect != 1 {
		t.Errorf("s = %+v", s)
	}
}

func TestAggregateEmptyAndNoTokens(t *testing.T) {
	s := Aggregate(nil)
	if s.StopAttempts != 0 || s.FalseDoneRate != 0 || s.TokensTotal != nil {
		t.Errorf("s = %+v", s)
	}
	var b bytes.Buffer
	s.WriteText(&b)
	if !strings.Contains(b.String(), "No stop attempts") {
		t.Error(b.String())
	}
	f := false
	s = Aggregate([]sessionlog.Entry{{Decision: "block", VerifyOK: &f}})
	b.Reset()
	s.WriteText(&b)
	if !strings.Contains(b.String(), "n/a") {
		t.Error(b.String())
	}
}

func TestTokenResetHandled(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	f := false
	s := Aggregate([]sessionlog.Entry{
		{SessionID: "x", Decision: "block", VerifyOK: &f, TotalTokens: n(5000)},
		{SessionID: "x", Decision: "allow", TotalTokens: n(1200)}, // compacted
	})
	if *s.TokensTotal != 6200 || *s.TokensOnFalseDone != 5000 {
		t.Errorf("total=%d false=%d", *s.TokensTotal, *s.TokensOnFalseDone)
	}
}

func TestCommas(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -4500: "-4,500"} {
		if got := commas(in); got != want {
			t.Errorf("commas(%d) = %q", in, got)
		}
	}
}
