// Package safepath confines project-local file writes to the project
// directory so a checkout cannot redirect them elsewhere through symlinks.
package safepath

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Resolve returns the real location of target, which must be root or a path
// under it. Symlinks in any existing component of target are followed only
// while they stay inside root; a symlink that escapes root, or a dangling
// one, is an error. Components that do not exist yet are fine.
func Resolve(root, target string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil || escapes(rel) {
		return "", fmt.Errorf("%s is outside %s", target, root)
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", err
	}
	cur := realRoot
	if rel == "." {
		return cur, nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for i, comp := range parts {
		next := filepath.Join(cur, comp)
		fi, err := os.Lstat(next)
		if os.IsNotExist(err) {
			return filepath.Join(append([]string{next}, parts[i+1:]...)...), nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(next)
			if err != nil {
				return "", fmt.Errorf("%s is a dangling symlink: %w", next, err)
			}
			if !Within(realRoot, real) {
				return "", fmt.Errorf("refusing to follow symlink %s: it points outside %s (to %s)", next, root, real)
			}
			next = real
		}
		cur = next
	}
	return cur, nil
}

// Within reports whether p is root or lexically inside it.
func Within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && !escapes(rel)
}

func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel)
}
