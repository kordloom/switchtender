#!/usr/bin/env bash
# Upgrades demo.switchtender.com to the latest tagged release, reseeds, and restarts.
#
# Runs on the droplet itself. Either pipe it over SSH:
#   ssh root@<droplet> bash -s < deploy/demo-upgrade.sh
# or, from a console on the box:
#   bash <(curl -fsSL https://raw.githubusercontent.com/kordloom/switchtender/main/deploy/demo-upgrade.sh)
#
# The box must already be provisioned: the switchtender-demo service, Caddy, and reseed.sh are
# assumed present. This script only swaps the binary and reseeds; it never touches packages or
# service configuration.
set -euo pipefail

BIN_DIR=/opt/switchtender/bin

echo "==> Resolving the latest release"
arch=$(dpkg --print-architecture)
# Fetch first, parse second: piping curl straight into grep -m1 makes grep close the pipe
# early, which kills the whole script under pipefail.
release_json=$(curl -fsSL https://api.github.com/repos/kordloom/switchtender/releases/latest)
tag=$(grep -m1 '"tag_name"' <<<"$release_json" | cut -d '"' -f 4)
[ -n "$tag" ] || { echo "could not resolve the latest release tag" >&2; exit 1; }
version="${tag#v}"
asset="switchtender_${version}_linux_${arch}.tar.gz"
base="https://github.com/kordloom/switchtender/releases/download/${tag}"
echo "    ${tag} (${arch})"

echo "==> Downloading and verifying the checksum"
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT
curl -fsSL -o "$workdir/$asset" "$base/$asset"
curl -fsSL -o "$workdir/SHA256SUMS" "$base/SHA256SUMS"
(cd "$workdir" && grep " ${asset}\$" SHA256SUMS | sha256sum -c - >/dev/null)
echo "    ${asset} verified"

echo "==> Installing ${tag}"
tar -xzf "$workdir/$asset" -C "$workdir"
install -m 0755 "$workdir/switchtender" "$BIN_DIR/switchtender"

echo "==> Reseeding and restarting the service"
"$BIN_DIR/reseed.sh"

echo "==> Demo is on ${tag}"
