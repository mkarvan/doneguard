package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const claudeTranscript = `{"type":"user","message":{"role":"user","content":"fix it"},"uuid":"u1"}
{"type":"assistant","requestId":"req_1","message":{"id":"msg_1","role":"assistant","content":[{"type":"thinking"}],"usage":{"input_tokens":10,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":5}}}
{"type":"assistant","requestId":"req_1","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":10,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":50}}}
not even json but mentions "usage"
{"type":"assistant","requestId":"req_2","message":{"id":"msg_2","role":"assistant","content":[],"usage":{"input_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":2000,"output_tokens":70}}}
`

func TestParseClaudeDedupesByMessageID(t *testing.T) {
	u, err := Parse(strings.NewReader(claudeTranscript))
	if err != nil {
		t.Fatal(err)
	}
	if u == nil || u.Format != "claude" {
		t.Fatalf("usage = %+v", u)
	}
	if u.InputTokens != 1110+2020 || u.OutputTokens != 120 || u.TotalTokens != 3130+120 {
		t.Errorf("usage = %+v", u)
	}
}

func TestParseCodexUsesLastTotal(t *testing.T) {
	in := `{"timestamp":"t","type":"session_meta","payload":{"id":"x"}}
{"timestamp":"t","type":"event_msg","payload":{"type":"token_count","info":null}}
{"timestamp":"t","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":15843,"cached_input_tokens":13696,"output_tokens":5,"reasoning_output_tokens":0,"total_tokens":15848}}}}
{"timestamp":"t","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":40170,"cached_input_tokens":30976,"output_tokens":380,"reasoning_output_tokens":0,"total_tokens":40550}}}}
{"timestamp":"t","type":"response_item","payload":{"type":"message","usage":{"input_tokens":1}}}
`
	u, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if u == nil || u.Format != "codex" || u.InputTokens != 40170 || u.OutputTokens != 380 || u.TotalTokens != 40550 {
		t.Errorf("usage = %+v", u)
	}
}

func TestParseUnknownAndMissing(t *testing.T) {
	u, err := Parse(strings.NewReader("{\"hello\":1}\n"))
	if err != nil || u != nil {
		t.Fatalf("u=%+v err=%v", u, err)
	}
	if _, err := ParseFile(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Error("expected error for missing file")
	}
	if u, err := ParseFile(""); u != nil || err != nil {
		t.Error("empty path should be (nil, nil)")
	}
}

func TestParseFileVeryLongLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	big := `{"type":"user","message":{"content":"` + strings.Repeat("a", 3<<20) + `"}}` + "\n"
	line := `{"type":"assistant","message":{"id":"m","usage":{"input_tokens":1,"output_tokens":2}}}`
	if err := os.WriteFile(p, []byte(big+line), 0o644); err != nil {
		t.Fatal(err)
	}
	u, err := ParseFile(p)
	if err != nil || u == nil || u.TotalTokens != 3 {
		t.Fatalf("u=%+v err=%v", u, err)
	}
}
