// Package transcript extracts token usage from agent transcripts.
//
// Supported formats (detected per line, so mixed files are fine):
//
//   - Claude Code JSONL transcripts: assistant lines carry message.usage with
//     input_tokens, cache_creation_input_tokens, cache_read_input_tokens and
//     output_tokens. One API message may be split over several lines sharing
//     message.id, so usage is de-duplicated by id (last value wins).
//   - Codex rollout JSONL: event_msg lines with payload.type == "token_count"
//     carry info.total_token_usage, a running total (last value wins).
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strconv"
)

// Usage is cumulative token usage for a transcript.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	Format       string
}

type claudeLine struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	UUID      string `json:"uuid"`
	Message   *struct {
		ID    string `json:"id"`
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type codexUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

type codexLine struct {
	Type    string `json:"type"`
	Payload *struct {
		Type string `json:"type"`
		Info *struct {
			TotalTokenUsage *codexUsage `json:"total_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

// ParseFile parses the transcript at path. It returns (nil, nil) when the
// file has no recognisable usage; it never fails on malformed lines.
func ParseFile(path string) (*Usage, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

var (
	usageKey      = []byte(`"usage"`)
	tokenCountKey = []byte(`"token_count"`)
)

// Parse reads a JSONL transcript from r.
func Parse(r io.Reader) (*Usage, error) {
	type claudeUsage struct{ in, out int64 }
	claude := map[string]claudeUsage{}
	var order []string
	var codex *codexUsage
	anon := 0

	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			switch {
			case bytes.Contains(line, tokenCountKey):
				var cl codexLine
				if json.Unmarshal(line, &cl) == nil && cl.Payload != nil && cl.Payload.Type == "token_count" &&
					cl.Payload.Info != nil && cl.Payload.Info.TotalTokenUsage != nil {
					u := *cl.Payload.Info.TotalTokenUsage
					codex = &u
				}
			case bytes.Contains(line, usageKey):
				var cl claudeLine
				if json.Unmarshal(line, &cl) == nil && cl.Message != nil && cl.Message.Usage != nil {
					u := cl.Message.Usage
					key := cl.Message.ID
					if key == "" {
						key = cl.RequestID
					}
					if key == "" {
						anon++
						key = "\x00anon" + strconv.Itoa(anon)
					}
					if _, seen := claude[key]; !seen {
						order = append(order, key)
					}
					claude[key] = claudeUsage{
						in:  u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens,
						out: u.OutputTokens,
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if codex != nil {
		total := codex.TotalTokens
		if total == 0 {
			total = codex.InputTokens + codex.OutputTokens
		}
		return &Usage{InputTokens: codex.InputTokens, OutputTokens: codex.OutputTokens, TotalTokens: total, Format: "codex"}, nil
	}
	if len(order) == 0 {
		return nil, nil
	}
	u := &Usage{Format: "claude"}
	for _, k := range order {
		u.InputTokens += claude[k].in
		u.OutputTokens += claude[k].out
	}
	u.TotalTokens = u.InputTokens + u.OutputTokens
	return u, nil
}
