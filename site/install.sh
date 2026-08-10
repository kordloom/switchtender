#!/bin/sh
# SwitchTender installer. It downloads the latest release binary for this machine, verifies it
# against the published checksums, and installs it. Nothing is built and no root is needed for a
# per-user install.
#
#   curl -fsSL https://switchtender.com/install.sh | sh
#
# Override where it lands with PREFIX (default /usr/local/bin, falling back to ~/.local/bin when that
# is not writable), or pin a version with VERSION=v1.62.0.
set -eu

REPO="kordloom/switchtender"
BIN="switchtender"
PREFIX="${PREFIX:-/usr/local/bin}"

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# fetch writes a URL to stdout with whichever downloader is present.
fetch() {
	if have curl; then curl -fsSL "$1"; elif have wget; then wget -qO- "$1"; else die "need curl or wget"; fi
}
# download saves a URL to a file.
download() {
	if have curl; then curl -fsSL -o "$2" "$1"; else wget -qO "$2" "$1"; fi
}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
	linux) os=linux ;;
	darwin) os=darwin ;;
	*) die "unsupported OS: $os. Download a release from https://github.com/$REPO/releases" ;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported architecture: $arch" ;;
esac

# The macOS build is a single universal binary, so both Mac arches take the darwin_all archive.
if [ "$os" = darwin ]; then
	slug="darwin_all"
else
	slug="${os}_${arch}"
fi

version="${VERSION:-}"
if [ -z "$version" ]; then
	say "Finding the latest release..."
	version=$(fetch "https://api.github.com/repos/$REPO/releases/latest" \
		| sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)
	[ -n "$version" ] || die "could not determine the latest version; set VERSION=vX.Y.Z"
fi
num="${version#v}"

archive="${BIN}_${num}_${slug}.tar.gz"
base="https://github.com/$REPO/releases/download/$version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading $BIN $version for $os/$arch..."
download "$base/$archive" "$tmp/$archive" || die "download failed: $base/$archive"

# Verify the archive against the published checksums before touching anything.
if download "$base/SHA256SUMS" "$tmp/SHA256SUMS" 2>/dev/null; then
	want=$(grep " ${archive}\$" "$tmp/SHA256SUMS" | awk '{print $1}')
	if [ -n "$want" ]; then
		if have sha256sum; then got=$(sha256sum "$tmp/$archive" | awk '{print $1}');
		elif have shasum; then got=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}');
		else got=""; say "no sha256 tool found; skipping checksum verification"; fi
		if [ -n "$got" ] && [ "$got" != "$want" ]; then
			die "checksum mismatch for $archive; refusing to install"
		fi
		[ -n "$got" ] && say "Checksum verified."
	fi
else
	say "Checksums not available for this release; installing without verification."
fi

tar -xzf "$tmp/$archive" -C "$tmp" || die "could not extract $archive"
[ -f "$tmp/$BIN" ] || die "the archive did not contain $BIN"
chmod +x "$tmp/$BIN"

# Install into PREFIX, falling back to ~/.local/bin when PREFIX is not writable without root.
dest="$PREFIX"
if [ ! -d "$dest" ] || [ ! -w "$dest" ]; then
	if [ "$(id -u)" = 0 ]; then
		mkdir -p "$dest"
	else
		dest="$HOME/.local/bin"
		mkdir -p "$dest"
		say "No write access to $PREFIX; installing to $dest instead."
	fi
fi
mv "$tmp/$BIN" "$dest/$BIN"

say ""
say "Installed $dest/$BIN"
case ":$PATH:" in
	*":$dest:"*) : ;;
	*) say "Add $dest to your PATH to run it by name: export PATH=\"$dest:\$PATH\"" ;;
esac
say "Verify what you got:"
say "  $BIN version --verify"
say "Start a local server:"
say "  $BIN serve"
