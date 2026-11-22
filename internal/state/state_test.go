package state

import (
	"errors"
	"os"
	"testing"
	"time"
)

func mustLoad(t *testing.T, dir, key string) State {
	t.Helper()
	st, err := Load(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestLoadSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if st := mustLoad(t, dir, "s1"); st.Retries != 0 || st.Key != "s1" {
		t.Fatalf("fresh state = %+v", st)
	}
	if err := Save(dir, State{Key: "s1", Retries: 2, BaseHead: "abc"}); err != nil {
		t.Fatal(err)
	}
	st := mustLoad(t, dir, "s1")
	if st.Retries != 2 || st.BaseHead != "abc" || st.Updated == "" {
		t.Fatalf("loaded = %+v", st)
	}
	if other := mustLoad(t, dir, "s2"); other.Retries != 0 {
		t.Fatalf("keys leak: %+v", other)
	}
}

func TestCorruptStateIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, State{Key: "k", Retries: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(dir, "k"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "k"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt state must not load as a fresh chain: %v", err)
	}
}

func TestResetKeepsInFlightReservations(t *testing.T) {
	now := time.Now()
	st := State{Key: "k"}
	if !st.Reserve("a", 1, now.Add(time.Minute)) {
		t.Fatal("first reservation")
	}
	st.Reset("b") // a concurrent passing hook
	if st.Retries != 1 || st.Reserve("c", 1, now.Add(time.Minute)) {
		t.Fatalf("reset reopened an in-flight slot: %+v", st)
	}
	st.Release("a") // a settles its block
	if st.Retries != 1 || len(st.Pending) != 0 {
		t.Fatalf("after settle: %+v", st)
	}
	st.Reset("")
	if st.Retries != 0 {
		t.Fatalf("reset with nothing in flight: %+v", st)
	}
	// A crashed invocation's slot expires.
	st.Reserve("dead", 1, now.Add(-time.Second))
	st.Prune(now)
	st.Reset("")
	if st.Retries != 0 || len(st.Pending) != 0 {
		t.Fatalf("expired reservation kept: %+v", st)
	}
}
