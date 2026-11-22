#!/bin/sh
# Cross-compile DoneGuard release binaries into dist/ and write dist/checksums.txt.
#
#   scripts/build-release.sh [VERSION]     e.g. scripts/build-release.sh v0.1.0
#
# VERSION defaults to the git tag on HEAD (git describe --tags --exact-match).
# Output names match what scripts/install.sh downloads:
#   dist/doneguard-<version>-<os>-<arch>   (linux|darwin x amd64|arm64, raw binaries)
#   dist/checksums.txt                     ("<sha256>  <file>" per line)
#
# This script only builds. Tagging and `gh release create` are done by hand.

set -eu

cd "$(dirname "$0")/.."

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
	VERSION=$(git describe --tags --exact-match 2>/dev/null) ||
		{ echo "build-release: no VERSION given and HEAD is not tagged" >&2; exit 1; }
fi
case "$VERSION" in
v*) ;;
*) VERSION="v$VERSION" ;;
esac
# Same rule as install.sh: a single release-tag path component.
case "$VERSION" in
v[0-9]*) ;;
*) echo "build-release: invalid version '$VERSION' (expected e.g. v0.1.0)" >&2; exit 1 ;;
esac
case "$VERSION" in
*[!A-Za-z0-9._-]* | *..*) echo "build-release: invalid version '$VERSION' (expected e.g. v0.1.0)" >&2; exit 1 ;;
esac

# sha256 prints the lowercase hex SHA-256 of $1, or fails.
sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		_s=$(sha256sum "$1") || return 1
	elif command -v shasum >/dev/null 2>&1; then
		_s=$(shasum -a 256 "$1") || return 1
	else
		echo "build-release: need sha256sum or shasum" >&2
		return 1
	fi
	_s=${_s%%[!0-9A-Fa-f]*}
	[ ${#_s} -eq 64 ] || return 1
	printf '%s\n' "$_s" | tr 'A-F' 'a-f'
}

rm -rf dist
mkdir -p dist
: >dist/checksums.txt

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
	os=${target%/*}
	arch=${target#*/}
	name="doneguard-$VERSION-$os-$arch"
	echo "building $name"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X github.com/mkarvan/doneguard/internal/cli.Version=$VERSION" \
		-o "dist/$name" ./cmd/doneguard
	sum=$(sha256 "dist/$name") || { echo "build-release: checksum failed for dist/$name" >&2; exit 1; }
	printf '%s  %s\n' "$sum" "$name" >>dist/checksums.txt
done

echo
cat dist/checksums.txt
