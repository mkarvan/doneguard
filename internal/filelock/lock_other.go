//go:build !unix

package filelock

import (
	"errors"
	"os"
	"time"
)

// staleAfter removes lock files left behind by a crashed process.
const staleAfter = 10 * time.Minute

func try(path string) (func(), bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > staleAfter {
				_ = os.Remove(path)
			}
			return nil, true, nil
		}
		return nil, false, err
	}
	f.Close()
	return func() { _ = os.Remove(path) }, false, nil
}
