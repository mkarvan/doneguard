#!/bin/sh
# DoneGuard installer: downloads a prebuilt release binary, verifies its
# SHA-256 against the release's checksums.txt and installs it. No sudo.
#
#   curl -fsSL -o install.sh https://raw.githubusercontent.com/mkarvan/doneguard/main/scripts/install.sh
#   sh install.sh
#
# (Piping into sh works too, after you have read the script.)
# curl is the preferred downloader: it is restricted to HTTPS, including
# redirects. The wget fallback refuses non-HTTPS URLs but cannot restrict
# redirect protocols on every wget build.
#
# Release asset naming (raw binaries, no archive, no extension):
#   doneguard-<version>-<os>-<arch>      e.g. doneguard-v0.1.0-linux-amd64
#   <os>   = linux | darwin
#   <arch> = amd64 | arm64
# plus checksums.txt with lines "<sha256>  doneguard-<version>-<os>-<arch>".
#
# Environment:
#   DONEGUARD_VERSION   release tag to install (default: latest release), e.g. v0.1.0
#   DONEGUARD_BIN_DIR   install directory (default: $HOME/.local/bin)
#   DONEGUARD_BASE_URL  release download base (default:
#                       https://github.com/mkarvan/doneguard/releases/download);
#                       files are fetched from $DONEGUARD_BASE_URL/<version>/<file>.
#                       Must be https://, or file:// (for tests; curl only).
#
# Options:
#   --dry-run   print what would be downloaded and where it would go; change nothing
#   -h, --help  show this help

set -eu

REPO_URL="https://github.com/mkarvan/doneguard"
BASE_URL="${DONEGUARD_BASE_URL:-$REPO_URL/releases/download}"
BIN_DIR="${DONEGUARD_BIN_DIR:-${HOME:?HOME is not set}/.local/bin}"
VERSION="${DONEGUARD_VERSION:-}"
DRY_RUN=0

say() { printf '%s\n' "$*"; }
err() { printf 'doneguard install: error: %s\n' "$*" >&2; exit 1; }

usage() {
	sed -n '2,30p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//' || true
}

for arg in "$@"; do
	case "$arg" in
	--dry-run) DRY_RUN=1 ;;
	-h | --help)
		if [ -f "$0" ]; then
			usage
		else
			say "usage: install.sh [--dry-run]   (env: DONEGUARD_VERSION, DONEGUARD_BIN_DIR, DONEGUARD_BASE_URL)"
		fi
		exit 0
		;;
	*) err "unknown argument: $arg (try --help)" ;;
	esac
done

# --- platform -------------------------------------------------------------
os=$(uname -s 2>/dev/null || echo unknown)
case "$os" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) err "unsupported OS '$os' (prebuilt binaries: linux, darwin). Install with Go 1.23+: go install github.com/mkarvan/doneguard/cmd/doneguard@latest" ;;
esac

arch=$(uname -m 2>/dev/null || echo unknown)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) err "unsupported architecture '$arch' (prebuilt binaries: amd64, arm64). Install with Go 1.23+: go install github.com/mkarvan/doneguard/cmd/doneguard@latest" ;;
esac

# --- download helpers -----------------------------------------------------
# Production downloads are HTTPS only. file:// is accepted solely so the test
# suite can serve a fake release from disk; it never comes from a redirect.
case "$BASE_URL" in
https://*) protos='=https' ;;
file://*) protos='=file' ;;
*) err "DONEGUARD_BASE_URL must start with https:// (got '$BASE_URL')" ;;
esac

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL --proto "$protos" --proto-redir '=https' -o "$2" "$1"; }
	latest_url() { curl -fsSLI --proto '=https' --proto-redir '=https' -o /dev/null -w '%{url_effective}' "$1"; }
elif command -v wget >/dev/null 2>&1; then
	# Fallback only. The requested URLs are always https:// (checked above),
	# but wget has no portable way to refuse a downgrade on redirect, so the
	# integrity guarantee rests on the HTTPS checksums.txt. Prefer curl.
	[ "$protos" = '=https' ] || err "file:// DONEGUARD_BASE_URL needs curl"
	say "note: curl not found; falling back to wget (curl is preferred: it restricts redirects to HTTPS)" >&2
	fetch() { wget -q -O "$2" "$1"; }
	latest_url() { wget -q -S --spider "$1" 2>&1 | sed -n 's/^ *[Ll]ocation: *//p' | tail -n 1 | tr -d '\r'; }
else
	err "need curl (preferred) or wget"
fi

# sha256 prints the lowercase hex SHA-256 of $1, or fails.
sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		_s=$(sha256sum "$1") || return 1
	elif command -v shasum >/dev/null 2>&1; then
		_s=$(shasum -a 256 "$1") || return 1
	else
		return 1
	fi
	_s=${_s%%[!0-9A-Fa-f]*}
	[ ${#_s} -eq 64 ] || return 1
	printf '%s\n' "$_s" | tr 'A-F' 'a-f'
}
command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 ||
	err "need sha256sum or shasum to verify the download"

# valid_version accepts a single release-tag path component: v<digit>...,
# letters, digits, '.', '_', '-' only, no '..'.
valid_version() {
	case "$1" in
	v[0-9]*) ;;
	*) return 1 ;;
	esac
	case "$1" in
	*[!A-Za-z0-9._-]* | *..*) return 1 ;;
	esac
	return 0
}

# --- version --------------------------------------------------------------
if [ -z "$VERSION" ]; then
	if [ "$DRY_RUN" = 1 ]; then
		VERSION="<latest>"
	else
		# /releases/latest redirects to /releases/tag/<version>.
		resolved=$(latest_url "$REPO_URL/releases/latest") || err "could not resolve the latest release (set DONEGUARD_VERSION, e.g. v0.1.0)"
		case "$resolved" in
		https://github.com/mkarvan/doneguard/releases/tag/*) ;;
		*) err "could not resolve the latest release (got '$resolved'); set DONEGUARD_VERSION, e.g. v0.1.0" ;;
		esac
		VERSION=${resolved##*/}
		valid_version "$VERSION" || err "could not resolve the latest release (got '$resolved'); set DONEGUARD_VERSION, e.g. v0.1.0"
	fi
fi
if [ "$VERSION" != "<latest>" ]; then
	case "$VERSION" in
	v*) ;;
	*) VERSION="v$VERSION" ;;
	esac
	valid_version "$VERSION" || err "invalid version '$VERSION' (expected a release tag such as v0.1.0)"
fi

asset="doneguard-$VERSION-$os-$arch"
asset_url="$BASE_URL/$VERSION/$asset"
sums_url="$BASE_URL/$VERSION/checksums.txt"
dest="$BIN_DIR/doneguard"

# The destination must be a regular file or absent: mv onto a directory would
# put the binary inside it and the script would report a bogus success.
if [ -d "$dest" ]; then
	err "$dest is a directory; remove it or set DONEGUARD_BIN_DIR"
fi
if [ -e "$dest" ] && [ ! -f "$dest" ]; then
	err "$dest exists and is not a regular file; remove it or set DONEGUARD_BIN_DIR"
fi
if [ -e "$BIN_DIR" ] && [ ! -d "$BIN_DIR" ]; then
	err "$BIN_DIR exists and is not a directory"
fi

if [ "$DRY_RUN" = 1 ]; then
	say "doneguard install (dry run)"
	say "  platform:  $os/$arch"
	say "  version:   $VERSION"
	say "  asset:     $asset_url"
	say "  checksums: $sums_url"
	say "  install:   $dest"
	exit 0
fi

# --- download and verify --------------------------------------------------
tmp=$(mktemp -d 2>/dev/null || mktemp -d -t doneguard) || err "cannot create a temporary directory"
stage=
trap 'rm -rf "$tmp"; if [ -n "$stage" ]; then rm -f "$stage"; fi' EXIT
trap 'exit 1' INT TERM HUP

say "downloading $asset_url"
fetch "$asset_url" "$tmp/$asset" || err "download failed: $asset_url"
fetch "$sums_url" "$tmp/checksums.txt" || err "download failed: $sums_url"

want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/checksums.txt")
[ -n "$want" ] || err "no checksum for $asset in checksums.txt"
got=$(sha256 "$tmp/$asset") || err "could not compute the SHA-256 of $asset"
want=$(printf '%s' "$want" | tr 'A-F' 'a-f')
[ "$got" = "$want" ] || err "checksum mismatch for $asset (expected $want, got $got)"
say "sha256 verified: $got"

# --- install --------------------------------------------------------------
mkdir -p "$BIN_DIR" || err "cannot create $BIN_DIR"
# Stage under an unpredictable name next to the destination (same filesystem),
# then rename, so a running binary is never truncated mid-write.
stage=$(mktemp "$BIN_DIR/.doneguard.XXXXXXXX") || err "cannot write to $BIN_DIR"
cp "$tmp/$asset" "$stage" || err "cannot write to $BIN_DIR"
chmod 0755 "$stage" || err "cannot make $stage executable"
# Refuse to replace anything with a binary that does not run here (wrong
# platform, noexec mount, ...).
"$stage" version >/dev/null 2>&1 || err "downloaded binary does not run on this system ('doneguard version' failed in $BIN_DIR; noexec mount?)"
if [ -d "$dest" ]; then
	err "$dest is a directory; remove it or set DONEGUARD_BIN_DIR"
fi
mv -f "$stage" "$dest" || err "cannot install to $dest"
stage=

ver=$("$dest" version 2>&1) || err "installed $dest but '$dest version' failed: $ver"
say "installed $dest"
say "$ver"

case ":${PATH:-}:" in
*":$BIN_DIR:"*) ;;
*)
	say ""
	say "note: $BIN_DIR is not on your PATH. Add it, e.g.:"
	say "  export PATH=\"$BIN_DIR:\$PATH\""
	say "(add that line to ~/.profile, ~/.bashrc or ~/.zshrc to make it permanent)"
	;;
esac
