// Package filelock provides a small interprocess exclusive lock with a
// bounded wait, used to serialise retry-state updates and log appends.
package filelock

import (
	"errors"
	"time"
)

// ErrTimeout is returned when the lock could not be acquired in time.
var ErrTimeout = errors.New("timed out waiting for lock")

const pollInterval = 10 * time.Millisecond

// Acquire takes an exclusive lock on path (created if missing), waiting at
// most timeout. The returned function releases it.
func Acquire(path string, timeout time.Duration) (release func(), err error) {
	deadline := time.Now().Add(timeout)
	for {
		release, busy, err := try(path)
		if err != nil {
			return nil, err
		}
		if !busy {
			return release, nil
		}
		if time.Now().After(deadline) {
			return nil, ErrTimeout
		}
		time.Sleep(pollInterval)
	}
}
