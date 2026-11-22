package safepath

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolve(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	os.MkdirAll(filepath.Join(root, "real"), 0o755)
	os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "inside"))
	os.Symlink(outside, filepath.Join(root, "escape"))
	os.Symlink(filepath.Join(outside, "settings.json"), filepath.Join(root, "leaf.json"))
	os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "dangling"))

	realRoot, _ := filepath.EvalSymlinks(root)
	if got, err := Resolve(root, filepath.Join(root, "inside", "x", "y.json")); err != nil || got != filepath.Join(realRoot, "real", "x", "y.json") {
		t.Errorf("inside symlink: %q %v", got, err)
	}
	if got, err := Resolve(root, filepath.Join(root, "new", "file")); err != nil || got != filepath.Join(realRoot, "new", "file") {
		t.Errorf("new path: %q %v", got, err)
	}
	for _, bad := range []string{
		filepath.Join(root, "escape", "settings.json"),
		filepath.Join(root, "leaf.json"),
		filepath.Join(root, "dangling"),
		filepath.Join(root, "..", "elsewhere"),
	} {
		if got, err := Resolve(root, bad); err == nil {
			t.Errorf("Resolve(%s) = %q, want error", bad, got)
		}
	}
}
