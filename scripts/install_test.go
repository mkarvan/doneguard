// Package scripts holds tests for the shell scripts in this directory.
package scripts

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func needSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install.sh supports linux and darwin only")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
}

func TestScriptsParseWithPOSIXSh(t *testing.T) {
	needSh(t)
	shells := []string{"sh"}
	if _, err := exec.LookPath("dash"); err == nil {
		shells = append(shells, "dash") // strict POSIX shell, catches bashisms
	}
	for _, sh := range shells {
		for _, f := range []string{"install.sh", "build-release.sh"} {
			if out, err := exec.Command(sh, "-n", f).CombinedOutput(); err != nil {
				t.Errorf("%s -n %s: %v\n%s", sh, f, err, out)
			}
		}
	}
}

// runInstall runs install.sh with a controlled environment.
func runInstall(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"install.sh"}, args...)...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func goPlatform() (string, string) { return runtime.GOOS, runtime.GOARCH }

func TestInstallHelpAndDryRun(t *testing.T) {
	needSh(t)
	out, err := runInstall(t, nil, "--help")
	if err != nil || !strings.Contains(out, "doneguard-<version>-<os>-<arch>") || !strings.Contains(out, "DONEGUARD_BIN_DIR") {
		t.Fatalf("--help: %v\n%s", err, out)
	}
	if out, err := runInstall(t, nil, "--bogus"); err == nil || !strings.Contains(out, "unknown argument") {
		t.Fatalf("--bogus should fail: %v\n%s", err, out)
	}

	bin := filepath.Join(t.TempDir(), "bin")
	out, err = runInstall(t, []string{"DONEGUARD_VERSION=0.1.0", "DONEGUARD_BIN_DIR=" + bin}, "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run: %v\n%s", err, out)
	}
	goos, goarch := goPlatform()
	if goarch != "amd64" && goarch != "arm64" {
		t.Skip("unsupported arch for prebuilt binaries")
	}
	want := fmt.Sprintf("https://github.com/mkarvan/doneguard/releases/download/v0.1.0/doneguard-v0.1.0-%s-%s", goos, goarch)
	if !strings.Contains(out, want) || !strings.Contains(out, filepath.Join(bin, "doneguard")) {
		t.Errorf("dry run output missing %s:\n%s", want, out)
	}
	if _, err := os.Stat(bin); err == nil {
		t.Error("--dry-run created the bin dir")
	}
}

// TestInstallFromLocalRelease installs from a fake release served via file://,
// checking checksum verification end to end without the network.
func TestInstallFromLocalRelease(t *testing.T) {
	needSh(t)
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	goos, goarch := goPlatform()
	if goarch != "amd64" && goarch != "arm64" {
		t.Skip("unsupported arch for prebuilt binaries")
	}
	const version = "v9.9.9"
	base := t.TempDir()
	rel := filepath.Join(base, version)
	os.MkdirAll(rel, 0o755)
	asset := fmt.Sprintf("doneguard-%s-%s-%s", version, goos, goarch)
	body := []byte("#!/bin/sh\necho \"doneguard " + version + "\"\n")
	os.WriteFile(filepath.Join(rel, asset), body, 0o644)
	sum := fmt.Sprintf("%x", sha256.Sum256(body))
	os.WriteFile(filepath.Join(rel, "checksums.txt"),
		[]byte("0000000000000000000000000000000000000000000000000000000000000000  doneguard-other\n"+sum+"  "+asset+"\n"), 0o644)

	bin := filepath.Join(t.TempDir(), "nested", "bin")
	env := []string{"DONEGUARD_VERSION=" + version, "DONEGUARD_BIN_DIR=" + bin, "DONEGUARD_BASE_URL=file://" + base}
	for i := 0; i < 2; i++ { // re-running replaces the binary in place
		out, err := runInstall(t, env)
		if err != nil {
			t.Fatalf("install run %d: %v\n%s", i, err, out)
		}
		if !strings.Contains(out, "sha256 verified") || !strings.Contains(out, "doneguard "+version) || !strings.Contains(out, "not on your PATH") {
			t.Errorf("run %d output:\n%s", i, out)
		}
	}
	fi, err := os.Stat(filepath.Join(bin, "doneguard"))
	if err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("binary not installed executable: %v %v", fi, err)
	}

	// A tampered asset must be rejected and must not replace the installed binary.
	os.WriteFile(filepath.Join(rel, asset), []byte("#!/bin/sh\necho evil\n"), 0o644)
	out, err := runInstall(t, env)
	if err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("tampered asset accepted: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(bin, "doneguard")); string(got) != string(body) {
		t.Error("installed binary replaced despite checksum mismatch")
	}

	// Missing checksum entry.
	os.WriteFile(filepath.Join(rel, "checksums.txt"), []byte(sum+"  something-else\n"), 0o644)
	if out, err := runInstall(t, env); err == nil || !strings.Contains(out, "no checksum") {
		t.Fatalf("missing checksum accepted: %v\n%s", err, out)
	}
}

// fakeRelease writes a release for the current platform under a temp base dir
// and returns the base, the release dir and the asset name.
func fakeRelease(t *testing.T, version string, body []byte) (base, rel, asset string) {
	t.Helper()
	goos, goarch := goPlatform()
	if goarch != "amd64" && goarch != "arm64" {
		t.Skip("unsupported arch for prebuilt binaries")
	}
	base = t.TempDir()
	rel = filepath.Join(base, version)
	os.MkdirAll(rel, 0o755)
	asset = fmt.Sprintf("doneguard-%s-%s-%s", version, goos, goarch)
	os.WriteFile(filepath.Join(rel, asset), body, 0o644)
	sum := fmt.Sprintf("%x", sha256.Sum256(body))
	os.WriteFile(filepath.Join(rel, "checksums.txt"), []byte(sum+"  "+asset+"\n"), 0o644)
	return base, rel, asset
}

func needCurl(t *testing.T) {
	t.Helper()
	needSh(t)
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
}

func noStagingLeft(t *testing.T, bin string) {
	t.Helper()
	ents, _ := os.ReadDir(bin)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".doneguard.") {
			t.Errorf("staging file left behind: %s", e.Name())
		}
	}
}

func TestInstallRefusesDirectoryDestination(t *testing.T) {
	needCurl(t)
	const version = "v9.9.9"
	base, _, _ := fakeRelease(t, version, []byte("#!/bin/sh\necho doneguard "+version+"\n"))
	bin := t.TempDir()
	env := []string{"DONEGUARD_VERSION=" + version, "DONEGUARD_BIN_DIR=" + bin, "DONEGUARD_BASE_URL=file://" + base}

	// A directory at the destination.
	os.Mkdir(filepath.Join(bin, "doneguard"), 0o755)
	out, err := runInstall(t, env)
	if err == nil || !strings.Contains(out, "is a directory") || strings.Contains(out, "installed ") {
		t.Fatalf("directory destination accepted: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(bin, "doneguard", "doneguard")); err == nil {
		t.Error("binary was moved inside the directory")
	}
	os.Remove(filepath.Join(bin, "doneguard"))

	// A symlink to a directory at the destination.
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(bin, "doneguard")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	out, err = runInstall(t, env)
	if err == nil || !strings.Contains(out, "is a directory") {
		t.Fatalf("symlink-to-directory destination accepted: %v\n%s", err, out)
	}
	if ents, _ := os.ReadDir(other); len(ents) != 0 {
		t.Errorf("wrote into the symlinked directory: %v", ents)
	}
	noStagingLeft(t, bin)
}

func TestInstallRejectsBadVersion(t *testing.T) {
	needSh(t)
	for _, v := range []string{"v1/../../x", "../v1", "v1.0/evil", "v..", "latest", "v1 2", "v1;rm"} {
		out, err := runInstall(t, []string{"DONEGUARD_VERSION=" + v, "DONEGUARD_BIN_DIR=" + t.TempDir()}, "--dry-run")
		if err == nil || !strings.Contains(out, "invalid version") {
			t.Errorf("version %q accepted: %v\n%s", v, err, out)
		}
	}
	// Bare numbers still get the v prefix.
	if out, err := runInstall(t, []string{"DONEGUARD_VERSION=0.1.0", "DONEGUARD_BIN_DIR=" + t.TempDir()}, "--dry-run"); err != nil || !strings.Contains(out, "/v0.1.0/") {
		t.Errorf("0.1.0: %v\n%s", err, out)
	}
}

func TestInstallRejectsNonHTTPSBaseURL(t *testing.T) {
	needSh(t)
	for _, u := range []string{"http://example.com/releases", "ftp://example.com/x", "example.com/x"} {
		out, err := runInstall(t, []string{"DONEGUARD_VERSION=v0.1.0", "DONEGUARD_BASE_URL=" + u, "DONEGUARD_BIN_DIR=" + t.TempDir()}, "--dry-run")
		if err == nil || !strings.Contains(out, "https://") {
			t.Errorf("base URL %q accepted: %v\n%s", u, err, out)
		}
	}
}

// A binary that downloads and verifies but does not run must not be reported
// as installed, and must not replace a working binary.
func TestInstallFailsWhenBinaryDoesNotRun(t *testing.T) {
	needCurl(t)
	const version = "v9.9.9"
	base, _, _ := fakeRelease(t, version, []byte("#!/bin/sh\necho broken >&2\nexit 3\n"))
	bin := t.TempDir()
	prev := []byte("#!/bin/sh\necho doneguard v0.0.1\n")
	os.WriteFile(filepath.Join(bin, "doneguard"), prev, 0o755)
	env := []string{"DONEGUARD_VERSION=" + version, "DONEGUARD_BIN_DIR=" + bin, "DONEGUARD_BASE_URL=file://" + base}
	out, err := runInstall(t, env)
	if err == nil || strings.Contains(out, "installed ") {
		t.Fatalf("non-running binary reported as installed: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(bin, "doneguard")); string(got) != string(prev) {
		t.Error("working binary replaced by a broken one")
	}
	noStagingLeft(t, bin)
}
