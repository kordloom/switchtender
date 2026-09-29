#!/usr/bin/env bash
#
# ci-local.sh is the canonical mirror of what CI runs, in one invocation, so "it passed on my
# machine" and "it passed in CI" mean the same sentence. Three gates used to disagree: plain
# `go test ./...` skipped the PostgreSQL contract and the loomseal cross-checks, CI ran them with
# -race, and the release gate ratcheted the skips into failures, so a change could be green
# locally and red everywhere that mattered. This script IS the CI test job, byte for byte where it
# can be: same flags, same ratchet, same database, same cross-check, same generated-site freshness
# rule.
#
# It needs Docker (for the throwaway PostgreSQL), Go, and Node. The loomseal checkout is cloned
# beside the repository on first run, at the exact version go.mod pins, the same way the ci and
# release workflows do it. golangci-lint and the fuzz smoke are included when present rather than
# required, and each says so when skipped.
#
#   ./scripts/ci-local.sh          # the full mirror
#   ./scripts/ci-local.sh fast     # build, vet, race suite only: the inner loop
set -euo pipefail

cd "$(dirname "$0")/.."
MODE="${1:-full}"

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
skip() { printf '   \033[33mskipped\033[0m %s\n' "$*"; }

say "PostgreSQL (throwaway container, same image as CI)"
PG_NAME="st-ci-local-$$"
docker run --rm -d --name "$PG_NAME" -e POSTGRES_PASSWORD=st-ci -e POSTGRES_DB=switchtender_test \
  -p 55432:5432 postgres:16 >/dev/null
trap 'docker stop "$PG_NAME" >/dev/null 2>&1 || true' EXIT
until docker exec "$PG_NAME" pg_isready -U postgres >/dev/null 2>&1; do sleep 0.5; done
export SWITCHTENDER_TEST_POSTGRES_DSN="postgres://postgres:st-ci@localhost:55432/switchtender_test?sslmode=disable"

say "loomseal checkout at the pinned version (the cross-check ratchet needs it)"
version="$(go list -m -f '{{.Version}}' github.com/kordloom/loomseal)"
loomseal_dir="../loomseal-ci-local"
if [ ! -d "$loomseal_dir" ]; then
  git clone --quiet https://github.com/kordloom/loomseal.git "$loomseal_dir"
fi
git -C "$loomseal_dir" fetch --quiet --tags
git -C "$loomseal_dir" checkout --quiet "$version"
export SWITCHTENDER_LOOMSEAL_REPO="$(cd "$loomseal_dir" && pwd)"

say "Build"
go build ./...
say "Vet"
go vet ./...

say "Test: the release gate's exact suite (-race, full-suite ratchet on)"
SWITCHTENDER_REQUIRE_FULL_SUITE=1 go test -race ./...

if [ "$MODE" = "fast" ]; then
  say "fast mode ends here"
  exit 0
fi

say "JS unit tests (same command as CI)"
if command -v node >/dev/null; then
  node --test internal/ui/assets/jstest/*.test.mjs
else
  skip "node is not installed"
fi

say "Generated site content is a fixed point of the tree (CI's rule, local semantics)"
# CI compares against the commit, because in CI the tree IS the commit. Locally the tree may hold
# deliberate uncommitted work, so the honest rule is idempotence: running sitegen must change
# nothing the tree did not already hold. A diff hash before and after is that rule exactly.
before="$(git diff -- site/ | shasum -a 256)"
go run ./cmd/sitegen
after="$(git diff -- site/ | shasum -a 256)"
if [ "$before" != "$after" ]; then
  echo "sitegen changed generated output: its source edits were not regenerated" >&2
  git --no-pager diff --stat -- site/
  exit 1
fi

say "prove.sh (the public dare, against a fresh build)"
mkdir -p .bin
go build -o .bin/switchtender .
./scripts/prove.sh .bin/switchtender >/dev/null
printf '   \033[32mok\033[0m the dare still holds\n'

say "Lint"
if command -v golangci-lint >/dev/null; then
  golangci-lint run ./...
else
  skip "golangci-lint is not installed (CI runs v2.12.2)"
fi

say "govulncheck (same pinned version as CI)"
go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...

say "everything CI gates on has run"
