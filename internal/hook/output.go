package hook

import (
	"encoding/json"
	"fmt"
	"io"
)

// Write emits the decision using the hook protocol and returns the process
// exit code.
//
// Default (preferred) protocol, shared by Claude Code 2.1 and Codex 0.170:
// exit 0 and print {"decision":"block","reason":"..."} to block; print
// nothing to allow. With exit2 set, the documented fallback is used instead:
// the reason goes to stderr and the exit code is 2.
//
// Never emits "continue": false, which would end the turn instead of
// sending the agent back to work.
func (d Decision) Write(stdout, stderr io.Writer, exit2 bool) int {
	if d.Block {
		if exit2 {
			fmt.Fprintln(stderr, d.Reason)
			return 2
		}
		writeJSON(stdout, map[string]string{"decision": "block", "reason": d.Reason})
		return 0
	}
	if d.SystemMessage != "" {
		// Claude Code and Codex both show systemMessage to the user as a
		// warning; no decision = allow. Allows always exit 0, so stdout JSON
		// is honoured in exit-2 mode too.
		writeJSON(stdout, map[string]string{"systemMessage": d.SystemMessage})
	}
	return 0
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
