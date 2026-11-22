// Package report aggregates .doneguard/sessions.jsonl into false-done stats.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/mkarvan/doneguard/internal/sessionlog"
)

// Summary is the aggregated report. JSON field names are stable.
type Summary struct {
	LogPath        string `json:"log_path"`
	StopAttempts   int    `json:"stop_attempts"`
	Sessions       int    `json:"sessions"`
	Blocked        int    `json:"blocked"`
	Allowed        int    `json:"allowed"`
	BlockedVerify  int    `json:"blocked_verify"`
	BlockedProtect int    `json:"blocked_protected"`
	BlockedConfig  int    `json:"blocked_config"`
	// Blocks where a check could not be completed (e.g. a git error).
	BlockedIndeterminate int `json:"blocked_indeterminate"`

	// A false-done is a stop attempt that DoneGuard blocked because
	// verification failed: a failing verify command or a touched protected
	// path. Config errors and indeterminate checks are not false-dones, and
	// allows at the retry cap are counted in allowed_unverified instead.
	FalseDone     int     `json:"false_done"`
	FalseDoneRate float64 `json:"false_done_rate"`

	AllowedVerified   int `json:"allowed_verified"`
	AllowedUnverified int `json:"allowed_unverified"`
	AllowedNoVerify   int `json:"allowed_no_verify_configured"`

	UnverifiedSessions int            `json:"unverified_sessions"`
	BySource           map[string]int `json:"by_source"`

	// Token usage (null when no transcript was parseable). Attempt tokens are
	// the growth of a transcript's cumulative usage since the previous stop
	// attempt in the same session, i.e. what was spent producing that attempt.
	TokensTotal          *int64   `json:"tokens_total"`
	TokensOnFalseDone    *int64   `json:"tokens_on_false_done"`
	TokensFalseDoneShare *float64 `json:"tokens_false_done_share"`

	FirstTimestamp string `json:"first_timestamp,omitempty"`
	LastTimestamp  string `json:"last_timestamp,omitempty"`
	MalformedLines int    `json:"malformed_lines,omitempty"`
}

// Aggregate computes the summary for entries in log order.
func Aggregate(entries []sessionlog.Entry) *Summary {
	s := &Summary{BySource: map[string]int{}}
	sessions := map[string]bool{}
	unverifiedSessions := map[string]bool{}
	lastTotal := map[string]int64{}
	var tokTotal, tokFalse int64
	haveTokens := false

	for i := range entries {
		e := &entries[i]
		s.StopAttempts++
		sid := e.Source + "\x00" + e.SessionID
		sessions[sid] = true
		if e.Source != "" {
			s.BySource[e.Source]++
		}
		if s.FirstTimestamp == "" || e.Timestamp < s.FirstTimestamp {
			s.FirstTimestamp = e.Timestamp
		}
		if e.Timestamp > s.LastTimestamp {
			s.LastTimestamp = e.Timestamp
		}
		failed := e.FalseDone()
		if failed {
			s.FalseDone++
		}
		switch e.Decision {
		case "block":
			s.Blocked++
			switch {
			case e.BlockKind == "config":
				s.BlockedConfig++
			case e.BlockKind == "indeterminate":
				s.BlockedIndeterminate++
			case len(e.ProtectedViolations) > 0:
				s.BlockedProtect++
			default:
				s.BlockedVerify++
			}
		default:
			s.Allowed++
			switch {
			case e.Unverified:
				s.AllowedUnverified++
				unverifiedSessions[sid] = true
			case e.VerifySkipped:
				s.AllowedNoVerify++
			default:
				s.AllowedVerified++
			}
		}
		if e.TotalTokens != nil {
			haveTokens = true
			key := sid + "\x00" + e.AgentID
			prev := lastTotal[key]
			delta := *e.TotalTokens - prev
			if delta < 0 {
				delta = *e.TotalTokens // transcript was reset/compacted
			}
			lastTotal[key] = *e.TotalTokens
			tokTotal += delta
			if failed {
				tokFalse += delta
			}
		}
	}
	s.Sessions = len(sessions)
	s.UnverifiedSessions = len(unverifiedSessions)
	if s.StopAttempts > 0 {
		s.FalseDoneRate = float64(s.FalseDone) / float64(s.StopAttempts)
	}
	if haveTokens {
		s.TokensTotal, s.TokensOnFalseDone = &tokTotal, &tokFalse
		if tokTotal > 0 {
			share := float64(tokFalse) / float64(tokTotal)
			s.TokensFalseDoneShare = &share
		}
	}
	return s
}

// WriteJSON writes the summary as indented JSON.
func (s *Summary) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// WriteText writes a human-readable report.
func (s *Summary) WriteText(w io.Writer) {
	fmt.Fprintf(w, "DoneGuard report — %s\n", s.LogPath)
	if s.StopAttempts == 0 {
		fmt.Fprintln(w, "  No stop attempts logged yet.")
		return
	}
	if s.FirstTimestamp != "" {
		fmt.Fprintf(w, "  Period:               %s → %s\n", s.FirstTimestamp, s.LastTimestamp)
	}
	fmt.Fprintf(w, "  Stop attempts:        %d across %d session(s)%s\n", s.StopAttempts, s.Sessions, sourceBreakdown(s.BySource))
	fmt.Fprintf(w, "  Blocked:              %d (verify failed %d, protected path %d, config error %d, check incomplete %d)\n", s.Blocked, s.BlockedVerify, s.BlockedProtect, s.BlockedConfig, s.BlockedIndeterminate)
	fmt.Fprintf(w, "  Allowed:              %d (verified %d, UNVERIFIED %d, no verify configured %d)\n", s.Allowed, s.AllowedVerified, s.AllowedUnverified, s.AllowedNoVerify)
	fmt.Fprintf(w, "  False-done:           %d of %d stop attempts (%.1f%%) blocked because checks failed\n", s.FalseDone, s.StopAttempts, 100*s.FalseDoneRate)
	fmt.Fprintf(w, "  Unverified sessions:  %d\n", s.UnverifiedSessions)
	if s.TokensTotal != nil {
		share := ""
		if s.TokensFalseDoneShare != nil {
			share = fmt.Sprintf(" (%.1f%%)", 100**s.TokensFalseDoneShare)
		}
		fmt.Fprintf(w, "  Tokens:               %s total; %s%s spent on false-done attempts\n", commas(*s.TokensTotal), commas(*s.TokensOnFalseDone), share)
	} else {
		fmt.Fprintln(w, "  Tokens:               n/a (no parseable transcripts)")
	}
	if s.MalformedLines > 0 {
		fmt.Fprintf(w, "  (skipped %d malformed log line(s))\n", s.MalformedLines)
	}
}

func sourceBreakdown(m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return " [" + strings.Join(parts, ", ") + "]"
}

func commas(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
