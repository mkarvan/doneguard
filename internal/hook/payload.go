// Package hook implements the Stop / SubagentStop decision logic shared by
// the Claude Code and Codex hooks and the Codex wrapper.
package hook

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Event names as normalised by ParsePayload.
const (
	EventStop         = "Stop"
	EventSubagentStop = "SubagentStop"
)

// Payload is the normalised hook input.
type Payload struct {
	Event                string
	RawEvent             string
	SessionID            string
	TranscriptPath       string
	AgentTranscriptPath  string
	Cwd                  string
	StopHookActive       *bool
	LastAssistantMessage string
	AgentID              string
	AgentType            string
	TurnID               string
	Source               string // detected: claude | codex | "" (unknown)
}

// Field aliases: Claude Code uses snake_case; Codex uses snake_case for most
// fields but some builds/integrations use camelCase or kebab-case.
var aliases = map[string][]string{
	"event":           {"hook_event_name", "hookEventName", "hook_event", "event_name", "eventName", "event"},
	"session":         {"session_id", "sessionId", "conversation_id", "conversationId", "thread_id", "threadId"},
	"transcript":      {"transcript_path", "transcriptPath", "rollout_path", "rolloutPath"},
	"agentTranscript": {"agent_transcript_path", "agentTranscriptPath"},
	"cwd":             {"cwd", "working_directory", "workingDirectory"},
	"active":          {"stop_hook_active", "stopHookActive"},
	"last":            {"last_assistant_message", "lastAssistantMessage", "last-assistant-message"},
	"agentID":         {"agent_id", "agentId", "subagent_id"},
	"agentType":       {"agent_type", "agentType", "subagent_type"},
	"turn":            {"turn_id", "turnId"},
}

// ParsePayload parses hook stdin JSON.
func ParsePayload(data []byte) (*Payload, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("hook input is not a JSON object: %w", err)
	}
	if raw == nil {
		// JSON null (or empty input) decodes without error into a nil map.
		return nil, fmt.Errorf("hook input is not a JSON object")
	}
	str := func(kind string) string {
		for _, k := range aliases[kind] {
			if v, ok := raw[k]; ok {
				switch t := v.(type) {
				case string:
					return t
				}
			}
		}
		return ""
	}
	p := &Payload{
		RawEvent:             str("event"),
		SessionID:            str("session"),
		TranscriptPath:       str("transcript"),
		AgentTranscriptPath:  str("agentTranscript"),
		Cwd:                  str("cwd"),
		LastAssistantMessage: str("last"),
		AgentID:              str("agentID"),
		AgentType:            str("agentType"),
		TurnID:               str("turn"),
	}
	for _, k := range aliases["active"] {
		if v, ok := raw[k]; ok {
			switch t := v.(type) {
			case bool:
				b := t
				p.StopHookActive = &b
			case string:
				// Only clear spellings count; anything else is treated as a
				// continuation (true) so it can never reset the retry chain.
				b := true
				switch strings.ToLower(strings.TrimSpace(t)) {
				case "false", "0", "no":
					b = false
				}
				p.StopHookActive = &b
			}
			break
		}
	}
	p.Event = NormalizeEvent(p.RawEvent)
	p.Source = detectSource(raw, p)
	return p, nil
}

// NormalizeEvent maps event spellings to Stop / SubagentStop. Unknown events
// are returned unchanged and a missing event stays "", so malformed input is
// never mistaken for a real stop.
func NormalizeEvent(e string) string {
	k := strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(e))
	switch k {
	case "stop", "agentstop", "turnstop":
		return EventStop
	case "subagentstop", "subagentcompleted", "subagentend":
		return EventSubagentStop
	}
	return e
}

func detectSource(raw map[string]any, p *Payload) string {
	tp := strings.ToLower(strings.ReplaceAll(p.TranscriptPath, "\\", "/"))
	switch {
	case strings.Contains(tp, "/.codex/") || strings.Contains(tp, "rollout-"):
		return "codex"
	case strings.Contains(tp, "/.claude/"):
		return "claude"
	}
	if _, ok := raw["turn_id"]; ok {
		return "codex"
	}
	if _, ok := raw["thread_id"]; ok {
		return "codex"
	}
	if _, ok := raw["permission_mode"]; ok {
		return "claude"
	}
	return ""
}
