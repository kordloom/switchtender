#!/usr/bin/env bash
# Upgrades demo.switchtender.com to the latest tagged release, reseeds, and restarts.
#
# Runs on the droplet itself. Either pipe it over SSH:
#   ssh root@<droplet> bash -s < deploy/demo-upgrade.sh
# or, from a console on the box:
#   bash <(curl -fsSL https://raw.githubusercontent.com/kordloom/switchtender/main/deploy/demo-upgrade.sh)
#
# The box must already be provisioned: the switchtender-demo service, Caddy, reseed.sh, and cosign
# are assumed present. This script only swaps the binary and reseeds; it never touches packages or
# service configuration.
#
# ONE-TIME CHANGE NEEDED ON THE BOX, and this script cannot make it because it does not edit the
# unit: the demo runs behind Caddy, so its ExecStart needs
#
#   --trusted-proxy 127.0.0.1/32 --client-ip-header X-Forwarded-For
#
# Without it every per-client budget resolves every visitor to Caddy's own address, so the
# 32-live-streams-per-caller cap applies to all visitors together rather than to each of them. The
# held terraform destroy at the center of the approvals story never reaches a terminal state, so
# each visitor who opens it and leaves the tab open holds a slot until they close it, and the
# thirty-third concurrent reader anywhere gets a 429 and a run view that never connects.
set -euo pipefail

BIN_DIR=/opt/switchtender/bin
# The identity a genuine release signature carries, the same pair SECURITY.md documents.
SIGNER='^https://github.com/kordloom/switchtender/\.github/workflows/release\.yml@refs/tags/v'
ISSUER=https://token.actions.githubusercontent.com

echo "==> Resolving the latest release"
arch=$(dpkg --print-architecture)
# The redirect GitHub answers for its latest-release download URL names the tag. The redirect
# carries no rate limit, where the API allows sixty anonymous calls an hour per address.
# A lookup that fails leaves the tag empty, so the message below names the problem.
target=$(curl -fsSI -o /dev/null -w '%{redirect_url}' \
  https://github.com/kordloom/switchtender/releases/latest/download/SHA256SUMS) || target=""
tag=""
case "$target" in
  */releases/download/*)
    tag="${target#*/releases/download/}"
    tag="${tag%%/*}"
    ;;
esac
[ -n "$tag" ] || { echo "could not resolve the latest release tag" >&2; exit 1; }
version="${tag#v}"
asset="switchtender_${version}_linux_${arch}.tar.gz"
base="https://github.com/kordloom/switchtender/releases/download/${tag}"
echo "    ${tag} (${arch})"

echo "==> Downloading and verifying the release"
# The checksums come from the same place as the archive, so on their own they establish that the
# download matches what GitHub serves. The cosign signature ties them to a run of the release
# workflow on a tag, and this box runs the result as root, so nothing is installed without it.
command -v cosign >/dev/null 2>&1 || {
  echo "cosign is not installed on this box, so the release signature cannot be checked;" \
    "not installing. Install cosign from https://github.com/sigstore/cosign/releases first." >&2
  exit 1
}
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT
curl -fsSL -o "$workdir/$asset" "$base/$asset"
curl -fsSL -o "$workdir/SHA256SUMS" "$base/SHA256SUMS"
curl -fsSL -o "$workdir/SHA256SUMS.sig" "$base/SHA256SUMS.sig"
curl -fsSL -o "$workdir/SHA256SUMS.pem" "$base/SHA256SUMS.pem"
if ! cosign verify-blob "$workdir/SHA256SUMS" \
    --signature "$workdir/SHA256SUMS.sig" --certificate "$workdir/SHA256SUMS.pem" \
    --certificate-identity-regexp "$SIGNER" --certificate-oidc-issuer "$ISSUER" \
    >/dev/null 2>"$workdir/cosign.log"; then
  cat "$workdir/cosign.log" >&2
  echo "the signature over the checksums for ${tag} did not verify; not installing" >&2
  exit 1
fi
echo "    signature over SHA256SUMS verified"
(cd "$workdir" && grep " ${asset}\$" SHA256SUMS | sha256sum -c - >/dev/null)
echo "    ${asset} checksum verified"

echo "==> Installing ${tag}"
tar -xzf "$workdir/$asset" -C "$workdir"
install -m 0755 "$workdir/switchtender" "$BIN_DIR/switchtender"

echo "==> Reseeding and restarting the service"
"$BIN_DIR/reseed.sh"

echo "==> Demo is on ${tag}"
