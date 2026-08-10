#!/usr/bin/env bash
#
# Build the browser verifier on site/verify from a released LoomSeal tag.
#
# The verifier that runs on the site must be a released one, never a working copy. When it was built
# from a checkout, the site and the published `loomseal verify` binary drifted onto two different
# chain link constructions: the page said VERIFIED and the released binary said NOT VERIFIED for the
# same bundle, and nothing detected it for two releases. Building from a module version pins the
# provenance, because the module proxy serves exactly what the tag says and nothing else.
#
# Usage: deploy/build-verifier.sh v0.9.0
#
# It writes site/verify/loomseal.wasm, syncs wasm_exec.js from the toolchain that compiled it, and
# stamps the version into site/verify/index.html so the page states which verifier it is running.

set -euo pipefail

version="${1:-}"
if [[ -z "$version" ]]; then
	echo "usage: $0 <loomseal-version>   e.g. $0 v0.9.0" >&2
	exit 2
fi
if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "refusing to build from '$version': pass a released tag such as v0.9.0, not a branch or a commit" >&2
	exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="$repo_root/site/verify"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Build from the published module rather than any local checkout, so the bytes come from the tag.
cd "$work"
go mod init verifierbuild >/dev/null 2>&1
GOFLAGS=-mod=mod go get "github.com/kordloom/loomseal@$version"
GOOS=js GOARCH=wasm GOFLAGS=-mod=mod go build -o "$out/loomseal.wasm" \
	"github.com/kordloom/loomseal/wasm"

# wasm_exec.js is part of the Go toolchain and must match the compiler that produced the module.
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$out/wasm_exec.js"

# Stamp the version into the page. A verifier whose version is invisible is one nobody can tell has
# gone stale, which is the failure this script exists to prevent.
python3 - "$out/index.html" "$version" <<'PY'
import re, sys

path, version = sys.argv[1], sys.argv[2]
with open(path, encoding="utf-8") as handle:
	page = handle.read()
pattern = r'(<span id="verifier-version"[^>]*>)[^<]*(</span>)'
if not re.search(pattern, page):
	sys.exit("index.html has no verifier-version element to stamp")
# Rewriting to the same value is the normal case on a rebuild, so only a missing element is an error.
stamped = re.sub(pattern, lambda m: m.group(1) + "loomseal " + version + m.group(2), page)
with open(path, "w", encoding="utf-8") as handle:
	handle.write(stamped)
PY

echo "built site/verify/loomseal.wasm from loomseal $version"
shasum -a 256 "$out/loomseal.wasm"
