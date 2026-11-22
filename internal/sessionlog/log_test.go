package sessionlog

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAppendConcurrentAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".doneguard", "sessions.jsonl")
	var wg sync.WaitGroup
	const n = 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := &Entry{
				Timestamp: "2026-11-22T00:00:00Z", SessionID: fmt.Sprintf("s%d", i),
				Decision: "block", Reason: strings.Repeat("x", 8000), Source: "claude",
			}
			if err := Append(path, e); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	entries, bad, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n || bad != 0 {
		t.Fatalf("entries=%d bad=%d", len(entries), bad)
	}
	if entries[0].ProtectedViolations == nil {
		t.Error("protected_violations should serialise as []")
	}
}

func TestReadSkipsMalformed(t *testing.T) {
	in := `{"decision":"allow","session_id":"a"}
not json
{"session_id":"no decision"}

{"decision":"block","verify_ok":false}`
	entries, bad, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || bad != 2 {
		t.Fatalf("entries=%d bad=%d", len(entries), bad)
	}
	if !entries[1].CheckFailed() || entries[0].CheckFailed() {
		t.Error("CheckFailed wrong")
	}
}
