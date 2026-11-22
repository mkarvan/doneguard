// Package state persists per-session retry counters under .doneguard/state.
package state

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mkarvan/doneguard/internal/filelock"
)

// LockTimeout bounds how long a hook waits for another hook holding the same
// retry-state lock. The lock is only held for read-modify-write, never while
// verify commands run, so contention is brief.
var LockTimeout = 5 * time.Second

// Lock serialises read/decide/write of one key's state across processes.
func Lock(dataDir, key string) (release func(), err error) {
	p := Path(dataDir, key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	return filelock.Acquire(p[:len(p)-len(".json")]+".lock", LockTimeout)
}

// State is the per-session (or per-subagent) hook state.
type State struct {
	Key string `json:"key"`
	// Retries is the number of consecutive blocks in the current stop chain.
	Retries int `json:"retries"`
	// BaseHead is HEAD at the last allowed stop; commits made after it are
	// included in the protected-path check.
	BaseHead string `json:"base_head,omitempty"`
	// Pending are block slots reserved by hook invocations that have not
	// settled yet. They count towards Retries and survive a reset by a
	// concurrent passing or fresh stop, so an in-flight block can never
	// be re-issued to another invocation.
	Pending []Reservation `json:"pending,omitempty"`
	Updated string        `json:"updated,omitempty"`
}

// Reservation is one in-flight block slot.
type Reservation struct {
	ID string `json:"id"`
	// Expires bounds how long a crashed invocation can hold its slot.
	Expires time.Time `json:"expires"`
}

// NewReservationID returns a random reservation id.
func NewReservationID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("r-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Reserve takes a block slot if fewer than max are in use.
func (s *State) Reserve(id string, max int, expires time.Time) bool {
	if s.Retries >= max {
		return false
	}
	s.Retries++
	s.Pending = append(s.Pending, Reservation{ID: id, Expires: expires})
	return true
}

// Holds reports whether reservation id is still pending.
func (s *State) Holds(id string) bool {
	for _, r := range s.Pending {
		if r.ID == id {
			return true
		}
	}
	return false
}

// Release drops the reservation id (the slot stays charged in Retries).
func (s *State) Release(id string) {
	out := s.Pending[:0]
	for _, r := range s.Pending {
		if r.ID != id {
			out = append(out, r)
		}
	}
	s.Pending = out
}

// Reset starts a new chain, keeping the slots of in-flight invocations
// other than exceptID charged.
func (s *State) Reset(exceptID string) {
	s.Release(exceptID)
	s.Retries = len(s.Pending)
}

// Prune drops reservations that expired before now.
func (s *State) Prune(now time.Time) {
	out := s.Pending[:0]
	for _, r := range s.Pending {
		if r.Expires.After(now) {
			out = append(out, r)
		}
	}
	s.Pending = out
	if s.Retries < len(s.Pending) {
		s.Retries = len(s.Pending)
	}
}

// ErrCorrupt is returned by Load for a state file that exists but cannot be
// used. Callers must not mistake it for a fresh chain.
var ErrCorrupt = errors.New("retry state is corrupt")

// Path returns the state file for key.
func Path(dataDir, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dataDir, "state", hex.EncodeToString(sum[:12])+".json")
}

// Load reads state for key. A missing file yields a zero State; a file
// that cannot be read or decoded yields an error (wrapping ErrCorrupt for
// bad content) so a retry chain is never silently restarted.
func Load(dataDir, key string) (State, error) {
	st := State{Key: key}
	data, err := os.ReadFile(Path(dataDir, key))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	var loaded State
	if err := json.Unmarshal(data, &loaded); err != nil {
		return st, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if loaded.Key != key {
		return st, fmt.Errorf("%w: key mismatch", ErrCorrupt)
	}
	if loaded.Retries < 0 {
		return st, fmt.Errorf("%w: negative retry count", ErrCorrupt)
	}
	return loaded, nil
}

// Save writes state atomically.
func Save(dataDir string, st State) error {
	p := Path(dataDir, st.Key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	st.Updated = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".state-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), p)
}
