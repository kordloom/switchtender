#!/bin/sh
# SwitchTender installer. It downloads the latest release binary for this machine, verifies it
# against the published checksums, checks the cosign signature over those checksums when cosign is
# installed, and installs it. Nothing is built and no root is needed for a per-user install.
#
#   curl -fsSL https://switchtender.com/install.sh | sh
#
# Override where it lands with PREFIX (default /usr/local/bin, falling back to ~/.local/bin when that
# is not writable), or pin a version with VERSION=v1.101.0.
#
# Everything runs from main, called on the last line, so a connection that drops partway through
# leaves the shell a function it never calls rather than a prefix of the script to run.
set -eu

REPO="kordloom/switchtender"
BIN="switchtender"
PREFIX="${PREFIX:-/usr/local/bin}"
# The identity a genuine release signature carries: this repository's release workflow, run on a
# tag, issued through GitHub's OIDC provider. SECURITY.md documents the same pair.
SIGNER='^https://github.com/kordloom/switchtender/\.github/workflows/release\.yml@refs/tags/v'
ISSUER="https://token.actions.githubusercontent.com"

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# download saves a URL to a file with whichever downloader is present.
download() {
	if have curl; then curl -fsSL -o "$2" "$1"; elif have wget; then wget -qO "$2" "$1"; else die "need curl or wget"; fi
}

# latest_version prints the newest release tag, read from the redirect GitHub answers for the
# latest-release download URL. The redirect carries no rate limit, where the API allows sixty
# anonymous calls an hour per address and an office or a CI farm that installs often runs past it.
# wget follows every hop and prints each one's headers under -S, in GNU and BusyBox alike, and the
# first Location names the tag. BusyBox, the only downloader on a stock Alpine, has no option to
# stop at the first hop. A lookup that fails prints nothing, so the caller can say what went wrong
# instead of the shell exiting on curl's status with no message.
latest_version() {
	url="https://github.com/$REPO/releases/latest/download/SHA256SUMS"
	if have curl; then
		target=$(curl -fsSI -o /dev/null -w '%{redirect_url}' "$url") || target=""
	elif have wget; then
		target=$(wget -q -S --spider "$url" 2>&1 | awk 'tolower($1) == "location:" { print $2; exit }')
	else
		die "need curl or wget"
	fi
	target=$(printf '%s' "$target" | tr -d '\r')
	case "$target" in
		*/releases/download/*) ;;
		*) return 0 ;;
	esac
	target="${target#*/releases/download/}"
	printf '%s\n' "${target%%/*}"
}

# verify_signature checks the cosign signature over the checksums when cosign is installed, and
# says plainly what was not checked when it is not. The checksums come from the same place as the
# archive, so on their own they establish that the download matches what GitHub serves and nothing
# more. The signature ties them to a run of this repository's release workflow on a tag.
verify_signature() {
	if ! have cosign; then
		say "The signature over the checksums was not checked: cosign is not installed."
		say "To check it, install cosign (https://docs.sigstore.dev) and re-run, or follow SECURITY.md."
		return 0
	fi
	download "$base/SHA256SUMS.sig" "$tmp/SHA256SUMS.sig" 2>/dev/null \
		|| die "no signature is published for $version; refusing to install unverified"
	download "$base/SHA256SUMS.pem" "$tmp/SHA256SUMS.pem" 2>/dev/null \
		|| die "no signing certificate is published for $version; refusing to install unverified"
	# cosign's own output is kept for the failure case alone: on success it is a line of deprecation
	# notices from cosign v3 about the detached signature. The detached pair is what the installer
	# checks because cosign v2 and v3 both verify it with the same flags.
	if ! cosign verify-blob "$tmp/SHA256SUMS" \
		--signature "$tmp/SHA256SUMS.sig" --certificate "$tmp/SHA256SUMS.pem" \
		--certificate-identity-regexp "$SIGNER" --certificate-oidc-issuer "$ISSUER" \
		>/dev/null 2>"$tmp/cosign.log"; then
		cat "$tmp/cosign.log" >&2
		die "the signature over the checksums for $version did not verify; refusing to install"
	fi
	say "Signature verified: the checksums were signed by this project's release workflow."
}

main() {
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
		version=$(latest_version)
		[ -n "$version" ] || die "could not determine the latest version; set VERSION=vX.Y.Z"
	fi
	num="${version#v}"

	archive="${BIN}_${num}_${slug}.tar.gz"
	base="https://github.com/$REPO/releases/download/$version"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	say "Downloading $BIN $version for $os/$arch..."
	download "$base/$archive" "$tmp/$archive" || die "download failed: $base/$archive"

	# Verify the archive against the published checksums before touching anything. A download that
	# cannot be verified is not installed: skipping the check quietly would hand whoever controls the
	# network exactly the window this product exists to close.
	download "$base/SHA256SUMS" "$tmp/SHA256SUMS" 2>/dev/null \
		|| die "could not download the checksums for $version; refusing to install unverified"
	verify_signature
	want=$(grep " ${archive}\$" "$tmp/SHA256SUMS" | awk '{print $1}')
	[ -n "$want" ] || die "no checksum published for $archive; refusing to install unverified"
	if have sha256sum; then got=$(sha256sum "$tmp/$archive" | awk '{print $1}');
	elif have shasum; then got=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}');
	else die "need sha256sum or shasum to verify the download; install one and re-run"; fi
	[ "$got" = "$want" ] || die "checksum mismatch for $archive; refusing to install"
	say "Checksum verified."

	tar -xzf "$tmp/$archive" -C "$tmp" || die "could not extract $archive"
	[ -f "$tmp/$BIN" ] || die "the archive did not contain $BIN"
	chmod +x "$tmp/$BIN"

	# Install into PREFIX, falling back to ~/.local/bin when PREFIX is not writable without root.
	dest="$PREFIX"
	# A PREFIX that does not exist yet is not a permission problem. PREFIX=$HOME/bin is the common
	# spelling of it, and treating a missing directory as unwritable ignored the setting while printing
	# "No write access to $HOME/bin", which was not true: the parent was writable and the directory
	# only needed creating.
	if [ ! -d "$dest" ] && mkdir -p "$dest" 2>/dev/null; then
		:
	fi
	if [ ! -d "$dest" ] || [ ! -w "$dest" ]; then
		if [ "$(id -u)" = 0 ]; then
			mkdir -p "$dest"
		else
			dest="$HOME/.local/bin"
			mkdir -p "$dest"
			say "No write access to $PREFIX; installing to $dest instead."
		fi
	fi
	# Copied rather than moved, so the installed file belongs to the account running this script. A moved
	# file keeps the owner the archive recorded, and tar run as root restores that owner. The copy lands
	# beside the target and is renamed over it, so a running binary is replaced in one step.
	cp "$tmp/$BIN" "$dest/.$BIN.new"
	chmod 0755 "$dest/.$BIN.new"
	mv -f "$dest/.$BIN.new" "$dest/$BIN"

	say ""
	say "Installed $dest/$BIN"
	# The closing commands are printed the way they will actually work from this shell. Printing the
	# bare name after saying the directory is not on PATH sent readers straight into command not found
	# on a stock Mac and on every non-root Linux install, where the fallback directory is the normal
	# outcome rather than the exception.
	run="$BIN"
	case ":$PATH:" in
		*":$dest:"*) : ;;
		*)
			run="$dest/$BIN"
			say "$dest is not on your PATH. Add it to run it by name:"
			say "  export PATH=\"$dest:\$PATH\""
			say "Until you do, use the full path:"
			;;
	esac
	# An earlier install from go install, Homebrew, or a package manager can sit earlier on PATH, and
	# then every command below runs the old binary while this script reports success. Saying which one
	# will actually run costs one lookup.
	shadow="$(command -v "$BIN" 2>/dev/null || true)"
	if [ -n "$shadow" ] && [ "$shadow" != "$dest/$BIN" ]; then
		say ""
		say "Note: $shadow comes first on your PATH, so \"$BIN\" still runs that one."
		say "Use $dest/$BIN, or remove the older install."
		run="$dest/$BIN"
	fi
	say "Verify what you got:"
	say "  $run version --verify"
	say "Start a local server:"
	say "  $run serve"
	# The Bash tool, and every starter template, runs bash by name. Most systems ship it, and Alpine and
	# some minimal images do not, where the first run fails with "bash: executable file not found".
	if ! have bash; then
		say ""
		say "Note: bash is not installed here, and the Bash tool and the starter templates need it."
		say "Install it with your package manager first, for example: apk add bash"
	fi
}

main "$@"
