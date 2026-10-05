#!/usr/bin/env bash
#
# ci-local.sh runs what GitHub runs on a pull request and on a push to main, on this machine, so a
# run that passes here comes back green there.
#
# It used to run the suite on macOS and stop there, and CI kept failing on what it never ran: a
# misspelling in a Linux-only file the linter reads only for GOOS=linux, fixtures the history secret
# scan flags, the runner's own ansible-core standing in for the pinned ones, ext4 giving a deleted
# directory's inode to the next one made, a probe assuming a newer botocore than the runner's, a
# Playwright locator, downgrade tests without the release tags they build from, and a racy test.
# Each of those now runs here the way CI runs it. A race that depends on timing can still pass one
# run and fail the next, here as in CI, so this narrows that class rather than closing it.
#
# The macOS checks run here. Every workflow job runs in a Linux container built from
# scripts/ci-local.Dockerfile, which is the hosted runner as far as those failures reach, at the
# versions the workflows pin. scripts/ci-local-runner.py runs each job there the way GitHub's runner
# does, executing the workflow's own step scripts, so a step edited in ci.yml is the step run here.
# LINUX_JOBS below says which phase runs which job, and a run stops when a workflow gating main has
# a job or a step that table does not name.
#
#   ./scripts/ci-local.sh                every phase, in order, stopping at the first failure
#   ./scripts/ci-local.sh fast           the macOS build, vet, gofmt, and race suite: the inner loop
#   ./scripts/ci-local.sh e2e gitleaks   only the named phases, in their usual order
#   ./scripts/ci-local.sh --list         the phases, and the CI jobs and steps each one runs
#
# It needs Docker, git, and Go, and for the macOS race suite the engines the full suite requires:
# ansible-core, Terraform, OpenTofu, PowerShell, and a python3 that imports cryptography. Go runs at
# the version go.mod names, as setup-go does. The image builds on first use, in a few minutes, and
# again only when a pinned version or the Playwright lockfile changes.
#
#   SUPERTEST_LICENSE_FILE      The Team license the supertest workflow reads from its secret.
#                               Without it the workflow's license step fails here, as in CI.
#   CI_LOCAL_SUPERTEST_TIER     Set to "community" to run the supertest without its Team phases.
#                               CI never does that, and the result says so when it happens.
#   SWITCHTENDER_LOOMSEAL_REPO  The LoomSeal checkout, ../loomseal-ci-local by default. It is cloned
#                               or moved to the version go.mod pins only when it is not there yet.
#   CI_LOCAL_MATRIX             Matrix values to narrow to, such as "shard=4,5", to rerun some legs.
#   CI_LOCAL_DIR                Where the run directory is made, $TMPDIR by default. It holds the
#                               log, the Playwright report, and the step summaries.
#   CI_LOCAL_KEEP               Set to 1 to keep the run directory after a pass. A failure keeps it.
set -euo pipefail

cd "$(dirname "$0")/.."

# IMAGE is the CI image, rebuilt when its inputs change.
IMAGE="switchtender-ci-local:latest"
# LABEL marks every container a run starts, so the run removes exactly its own.
LABEL="switchtender.ci-local"
# GO_MOD_CACHE and GO_BUILD_CACHE are the volumes Go's caches persist in between runs.
GO_MOD_CACHE="switchtender-ci-local-gomod"
GO_BUILD_CACHE="switchtender-ci-local-gocache"
# RUN_ID names this run's containers and run directory apart from any other run's.
RUN_ID="$(date +%Y%m%d-%H%M%S)-$$"
# PHASES is the order a full run takes: the fast checks first, then the macOS and Linux suites, then
# everything else CI gates on.
PHASES=(build lint race crossverify conformance integration node e2e assess sitegen helm vuln prove
  supertest gitleaks)

# LINUX_JOBS maps each phase to the workflow jobs it runs in the Linux container: phase, workflow,
# job, and, for a job split across phases, the run step this row runs. The runner holds this table
# to the workflows at the start of every run, in both directions.
LINUX_JOBS='
build       | ci.yml          | checks                | Build
build       | ci.yml          | checks                | Vet
lint        | ci.yml          | lint                  |
race        | ci.yml          | test                  |
crossverify | ci.yml          | crossverify           |
conformance | ci.yml          | inventory-conformance |
integration | ci.yml          | integration           |
node        | ci.yml          | checks                | JS unit tests
e2e         | ci.yml          | ui-e2e                |
assess      | ci.yml          | assessment            |
assess      | ci.yml          | verifier              |
assess      | deploy-site.yml | deploy                |
sitegen     | ci.yml          | checks                | Generated site content is current
helm        | ci.yml          | checks                | Helm chart renders
vuln        | ci.yml          | vuln                  |
prove       | ci.yml          | checks                | prove.sh still proves
supertest   | supertest.yml   | supertest             |
gitleaks    | ci.yml          | checks                | Secret scan
'

# RUNNER_CPUS and RUNNER_MEMORY_GIB are what a hosted ubuntu-latest runner has to itself. Each job
# container is given as many CPUs, when Docker has them, and Go sizes GOMAXPROCS, and with it how
# many test binaries go test runs at once, from that limit, as it does on the runner.
RUNNER_CPUS=4
RUNNER_MEMORY_GIB=16

# HOST_NETWORK_JOBS reach containers they start themselves through ports published on the Docker
# host: the integration suite's SSH machines and the supertest's Kind cluster. Only the host's
# network namespace sees those ports, so these jobs run in it. Every other job gets a network
# namespace of its own, as a hosted runner's job has a machine of its own.
HOST_NETWORK_JOBS=" integration supertest "

# HOST_NAME names this machine in the output of the checks that run on it rather than in the
# container.
HOST_NAME="this machine"
if [ "$(uname)" = Darwin ]; then
  HOST_NAME=macOS
fi

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }
ok()   { printf '   \033[32mok\033[0m %s\n' "$*"; }
warn() { printf '   \033[33m%s\033[0m\n' "$*"; }
die()  { printf '\n\033[31mci-local: %s\033[0m\n' "$*" >&2; exit 1; }

# usage prints the header above.
usage() {
  sed -n '3,/^set -euo pipefail$/p' "$0" | sed '$d' | sed -E 's/^# ?//'
}

# table_rows prints the LINUX_JOBS rows of one phase as workflow|job|step, trimmed.
table_rows() {
  printf '%s\n' "$LINUX_JOBS" | awk -F'|' -v p="$1" '
    { for (i = 1; i <= NF; i++) gsub(/^ +| +$/, "", $i) }
    $1 == p { print $2 "|" $3 "|" $4 }'
}

# list prints each phase with what it runs on this machine and the CI jobs and steps it runs in the
# Linux container.
list() {
  local phase workflow job step
  for phase in "${PHASES[@]}"; do
    printf '%s\n' "$phase"
    case "$phase" in
      build) note "$HOST_NAME: go build ./..., go vet ./..., gofmt -l" ;;
      lint) note "$HOST_NAME: golangci-lint run, at the version the lint job pins" ;;
      race) note "$HOST_NAME: go test -race ./..., full-suite switch, PostgreSQL, release tags" ;;
    esac
    while IFS='|' read -r workflow job step; do
      if [ -z "$workflow" ]; then
        continue
      elif [ -n "$step" ]; then
        note "Linux: $workflow $job, step \"$step\""
      else
        note "Linux: $workflow $job, every step"
      fi
    done <<< "$(table_rows "$phase")"
  done
  cat <<'EOF'

Not run here, and why
   deploy-site.yml deploy, "Deploy site to Cloudflare Pages": it publishes the site with the
     repository's Cloudflare token. Every step before it runs.
   supertest.yml supertest, "Write the Team license": it reads the SUPERTEST_LICENSE secret.
     Set SUPERTEST_LICENSE_FILE to the license to run it as CI does.
   upload-artifact steps: the files they would upload are copied into the run directory.
   release.yml, fuzz.yml, demo-refresh.yml: they run on tags, a schedule, or by hand, never on a
     pull request or a push to main.
EOF
}

# only_value prints the one value a pattern's first group takes across the named files. It stops
# the run when the pattern finds nothing or disagrees with itself, because a workflow whose shape
# changed must not quietly feed this run an old pin.
only_value() {
  local what="$1" pattern="$2" values
  shift 2
  values="$(grep -hoE "$pattern" "$@" | sed -E "s#$pattern#\\1#" | sort -u)"
  if [ -z "$values" ] || [ "$(printf '%s\n' "$values" | wc -l | tr -d ' ')" != 1 ]; then
    die "could not read one $what from $*: found [${values//$'\n'/ }]. The workflow changed" \
      "shape, so update the pattern read_pins gives only_value in scripts/ci-local.sh."
  fi
  printf '%s\n' "$values"
}

# read_pins reads every version CI pins, from go.mod and the workflows, into the image's build
# arguments. Each is read into a variable of its own first, because a failure inside an array
# literal's command substitution does not stop a bash script.
read_pins() {
  local wf=.github/workflows matrix pinned ansible node terraform tofu helm gitleaks kind crypto
  GO_VERSION="$(sed -nE 's/^toolchain go([0-9][0-9.]*)$/\1/p' go.mod)"
  if [ -z "$GO_VERSION" ]; then
    GO_VERSION="$(only_value "go directive" '^go ([0-9][0-9.]*)$' go.mod)"
  fi
  GOLANGCI_LINT_VERSION="$(only_value "golangci-lint version" 'version: (v[0-9][0-9.]*)' \
    "$wf/ci.yml")"
  node="$(only_value "Node version" 'node-version: ([0-9]+)' "$wf/ci.yml" "$wf/supertest.yml" \
    "$wf/deploy-site.yml")"
  terraform="$(only_value "Terraform version" 'TERRAFORM_VERSION: "([0-9][0-9.]*)"' "$wf/ci.yml")"
  tofu="$(only_value "OpenTofu version" 'TOFU_VERSION: "([0-9][0-9.]*)"' "$wf/ci.yml")"
  helm="$(only_value "Helm version" 'helm-v([0-9][0-9.]*)-linux-amd64' "$wf/ci.yml" \
    "$wf/supertest.yml")"
  gitleaks="$(only_value "gitleaks version" 'version=([0-9][0-9.]*)$' "$wf/ci.yml")"
  kind="$(only_value "kind version" 'kind\.sigs\.k8s\.io/dl/v([0-9][0-9.]*)/' "$wf/supertest.yml")"
  crypto="$(only_value "cryptography version" 'cryptography==([0-9][0-9.]*)' "$wf/ci.yml")"
  matrix="$(grep -hE '^[[:space:]]*ansible-core: \[' "$wf/ci.yml" |
    grep -oE '[0-9]+\.[0-9]+\.[0-9]+' || true)"
  pinned="$(grep -hoE 'ansible-core==[0-9][0-9.]*' "$wf/ci.yml" | sed 's/.*==//' || true)"
  ansible="$(printf '%s\n%s\n' "$matrix" "$pinned" | grep . | sort -uV | tr '\n' ' ')"
  if [ -z "$matrix" ] || [ -z "$pinned" ]; then
    die "could not read the ansible-core matrix and pins from ci.yml. Update read_pins"
  fi
  BUILD_ARGS=(
    --build-arg "GO_VERSION=$GO_VERSION"
    --build-arg "NODE_MAJOR=$node"
    --build-arg "TERRAFORM_VERSION=$terraform"
    --build-arg "TOFU_VERSION=$tofu"
    --build-arg "GOLANGCI_LINT_VERSION=$GOLANGCI_LINT_VERSION"
    --build-arg "HELM_VERSION=$helm"
    --build-arg "GITLEAKS_VERSION=$gitleaks"
    --build-arg "KIND_VERSION=$kind"
    --build-arg "ANSIBLE_CORE_VERSIONS=${ansible% }"
    --build-arg "CRYPTOGRAPHY_VERSION=$crypto"
  )
}

# tree_files prints, NUL-separated, the files a push would carry: tracked or new, not ignored, and
# still present. Its arguments narrow it to pathspecs.
tree_files() {
  local f
  git ls-files -z --cached --others --exclude-standard -- "$@" |
    while IFS= read -r -d '' f; do
      if [ -e "$f" ] || [ -L "$f" ]; then
        printf '%s\0' "$f"
      fi
    done
}

# ensure_image builds the CI image unless the one present was built from these exact inputs: the
# pins, the Dockerfile, and the Playwright lockfile whose browser it carries.
ensure_image() {
  local inputs have ctx
  inputs="$( { printf '%s\n' "${BUILD_ARGS[@]}"; cat scripts/ci-local.Dockerfile \
    internal/ui/e2e/package.json internal/ui/e2e/package-lock.json; } | shasum -a 256 | cut -c1-64)"
  have="$(docker image inspect --format "{{ index .Config.Labels \"$LABEL.inputs\" }}" "$IMAGE" \
    2>/dev/null || true)"
  if [ "$have" = "$inputs" ]; then
    ok "CI image is current"
    return
  fi
  say "Building the CI image (its pins, Dockerfile, or Playwright lockfile changed)"
  ctx="$RUN_DIR/image"
  mkdir -p "$ctx/e2e"
  cp scripts/ci-local.Dockerfile "$ctx/Dockerfile"
  cp internal/ui/e2e/package.json internal/ui/e2e/package-lock.json "$ctx/e2e/"
  docker build --progress=plain --label "$LABEL.inputs=$inputs" -t "$IMAGE" "${BUILD_ARGS[@]}" \
    "$ctx"
}

# make_snapshot archives the tree under test into the run directory, for every Linux job to check
# out from: the files a push would carry, tracked or new, without the ignored ones a fresh checkout
# never has, and a .git file naming this repository's git directory, which the jobs read without
# writing. It is one file in a host directory rather than a volume, so pruning Docker's volumes
# during a run cannot empty it under a job, and one file rather than a tree, because a container
# reads thousands of small files through Docker Desktop's file sharing slowly.
make_snapshot() {
  local list="$RUN_DIR/files" pointer="$RUN_DIR/pointer" tar_flags=()
  SNAPSHOT="$RUN_DIR/src.tar"
  tree_files > "$list"
  # macOS tar otherwise adds an AppleDouble ._ file beside every file with extended attributes, and
  # Go would try to compile each ._*.go it found.
  if [ "$(uname)" = Darwin ]; then
    tar_flags=(--no-mac-metadata --no-xattrs)
  fi
  COPYFILE_DISABLE=1 tar ${tar_flags[@]+"${tar_flags[@]}"} --null -T "$list" -cf "$SNAPSHOT"
  mkdir -p "$pointer"
  printf 'gitdir: %s\n' "$GIT_DIR" > "$pointer/.git"
  COPYFILE_DISABLE=1 tar ${tar_flags[@]+"${tar_flags[@]}"} -rf "$SNAPSHOT" -C "$pointer" .git
  ok "snapshot of $(tr -cd '\0' < "$list" | wc -c | tr -d ' ') files: commit" \
    "$(git rev-parse --short HEAD) with the changes not committed yet"
}

# in_job runs a command in a fresh container of the CI image, with the snapshot unpacked at
# /ci/source, where the runner reads the tree, the workflows, and itself. Modes are normalized as a
# checkout writes them. Its arguments are docker run options, then --, then the command.
in_job() {
  local options=()
  while [ "$1" != -- ]; do
    options+=("$1")
    shift
  done
  shift
  docker run --rm --label "$LABEL=$RUN_ID" -v "$SNAPSHOT:/ci/src.tar:ro" "${options[@]}" \
    "$IMAGE" sh -c 'mkdir -p /ci/source &&
      tar --no-same-owner -xf /ci/src.tar -C /ci/source &&
      chmod -R u+rwX,go+rX,go-w /ci/source &&
      exec "$@"' sh "$@"
}

# make_caches creates, every run, the volumes that keep Go's module and build caches between runs,
# as setup-go's cache does in CI. They are only caches, so a prune that removes them costs time and
# nothing else, and a job started after one simply finds them empty.
make_caches() {
  docker volume create "$GO_MOD_CACHE" >/dev/null
  docker volume create "$GO_BUILD_CACHE" >/dev/null
}

# shared_tmp makes, once per run, the temporary directory the host-network jobs share with the
# containers they start. A test there bind mounts its own temporary directory into a container,
# and the daemon resolves that path on the host, so the directory is a host directory mounted at the
# same path in the job. It is not a volume under /var/lib/docker, which would also be visible to
# the daemon, because the product refuses to mount anything under that tree, as it should.
shared_tmp() {
  if [ -n "${SHARED_TMP:-}" ]; then
    return
  fi
  SHARED_TMP="$RUN_DIR/t"
  mkdir -p "$SHARED_TMP"
  chmod 1777 "$SHARED_TMP"
}

# loomseal_checkout makes sure the LoomSeal checkout is at the version go.mod pins, looked up under
# either tag form as CI does. A checkout already there is left alone: no fetch, no checkout.
loomseal_checkout() {
  local version dir ref want have
  version="$(go list -m -f '{{.Version}}' github.com/kordloom/loomseal)"
  dir="${SWITCHTENDER_LOOMSEAL_REPO:-../loomseal-ci-local}"
  if [ -e "$dir/.git" ]; then
    ref="$version"
    git -C "$dir" rev-parse --verify --quiet "refs/tags/$ref" >/dev/null || ref="snapshot/$version"
    want="$(git -C "$dir" rev-parse --verify --quiet "$ref^{commit}" || true)"
    have="$(git -C "$dir" rev-parse HEAD)"
    if [ -n "$want" ] && [ "$want" = "$have" ]; then
      LOOMSEAL_DIR="$(cd "$dir" && pwd -P)"
      ok "LoomSeal checkout already at $ref: $LOOMSEAL_DIR"
      return
    fi
  else
    git clone --quiet https://github.com/kordloom/loomseal.git "$dir"
  fi
  git -C "$dir" fetch --quiet --tags
  ref="$version"
  git -C "$dir" rev-parse --verify --quiet "refs/tags/$ref" >/dev/null || ref="snapshot/$version"
  git -C "$dir" checkout --quiet "$ref"
  LOOMSEAL_DIR="$(cd "$dir" && pwd -P)"
  ok "LoomSeal checkout moved to $ref: $LOOMSEAL_DIR"
}

# require_release_tag stops the run when no release tag is older than the version the chart says
# this tree builds. The downgrade tests build that release from its tag, and under the full-suite
# switch they fail without it, which on a fresh clone means a fetch, not a fix.
require_release_tag() {
  local chart previous
  chart="$(sed -nE 's/^appVersion:[[:space:]]*"?([0-9][0-9.]*)"?.*$/\1/p' \
    deploy/helm/switchtender/Chart.yaml | head -1)"
  previous="$( { git tag --list 'v*' 'snapshot/v*' | sed -E 's#^(snapshot/)?v##'; echo "$chart"; } |
    sort -uV | grep -B1 -xF "$chart" | head -1)"
  if [ -z "$previous" ] || [ "$previous" = "$chart" ]; then
    die "no release tag is older than $chart, the version the chart builds, so the downgrade" \
      "tests cannot build the release they start from. Fetch the tags: git fetch --tags"
  fi
  ok "the downgrade tests build v$previous from its tag"
}

# require_engines stops the run when this machine lacks a tool the full suite fails without.
require_engines() {
  local missing=() bin python
  for bin in ansible-playbook ansible-inventory bash git go pwsh python3 terraform tofu; do
    command -v "$bin" >/dev/null 2>&1 || missing+=("$bin")
  done
  python="${SWITCHTENDER_LOOMVERIFY_PYTHON:-python3}"
  if ! "$python" -c 'import cryptography' >/dev/null 2>&1; then
    missing+=("a $python that imports cryptography, for the LoomSeal reference verifier")
  fi
  if [ "${#missing[@]}" -gt 0 ]; then
    die "the full suite fails on this machine without: ${missing[*]}"
  fi
}

# start_postgres starts a throwaway PostgreSQL with CI's image, user, password, and database, on a
# port Docker picks, and exports its DSN. The database name is not cosmetic: tests that need a
# database the suite never made derive one from this DSN, and the license gate is one of them.
start_postgres() {
  local port database
  POSTGRES_NAME="stci-$RUN_ID-postgres"
  docker run --rm -d --name "$POSTGRES_NAME" --label "$LABEL=$RUN_ID" \
    -e POSTGRES_USER=switchtender -e POSTGRES_PASSWORD=switchtender -e POSTGRES_DB=switchtender \
    -p 127.0.0.1::5432 postgres:16 >/dev/null
  until docker exec "$POSTGRES_NAME" pg_isready -U switchtender >/dev/null 2>&1; do sleep 0.5; done
  port="$(docker port "$POSTGRES_NAME" 5432/tcp | head -1 | sed 's/.*://')"
  database="switchtender:switchtender@127.0.0.1:$port/switchtender"
  export SWITCHTENDER_TEST_POSTGRES_DSN="postgres://$database?sslmode=disable"
}

# host_build builds, vets, and format-checks this machine's build of every package, the darwin one
# on macOS.
host_build() {
  local unformatted
  say "$HOST_NAME: go build ./..."
  go build ./...
  say "$HOST_NAME: go vet ./..."
  go vet ./...
  say "$HOST_NAME: gofmt -l"
  unformatted="$(tree_files '*.go' | xargs -0 gofmt -l 2>&1 || true)"
  if [ -n "$unformatted" ]; then
    printf '%s\n' "$unformatted"
    die "gofmt would change the files above"
  fi
  ok "every Go file is formatted"
}

# host_lint runs the linter release the lint job pins over this machine's build of every package,
# the darwin one on macOS. The Linux build is the lint job itself, in the container.
host_lint() {
  local lint=(golangci-lint)
  say "$HOST_NAME: golangci-lint $GOLANGCI_LINT_VERSION, the $(go env GOOS) build of each package"
  if ! golangci-lint version 2>/dev/null | grep -q "version ${GOLANGCI_LINT_VERSION#v} "; then
    note "golangci-lint $GOLANGCI_LINT_VERSION is not on PATH, so it is built with go run"
    lint=(go run "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$GOLANGCI_LINT_VERSION")
  fi
  "${lint[@]}" run
}

# host_race runs CI's suite on this machine: race detector, full-suite switch, PostgreSQL, the
# LoomSeal checkout, and the release tags.
host_race() {
  say "$HOST_NAME: the full race suite, with PostgreSQL, LoomSeal, and the release tags"
  require_engines
  require_release_tag
  start_postgres
  SWITCHTENDER_LOOMSEAL_REPO="$LOOMSEAL_DIR" SWITCHTENDER_REQUIRE_FULL_SUITE=1 go test -race ./...
  docker stop "$POSTGRES_NAME" >/dev/null 2>&1 || true
}

# size_jobs gives each job container the CPUs a hosted runner has, and says so when Docker has much
# less memory than one. The race shards run several race-instrumented test binaries at once, and
# the kernel kills one when the machine runs out, which the runner then reports as the cause.
size_jobs() {
  local cpus memory
  cpus="$(docker info --format '{{.NCPU}}')"
  JOB_CPUS="$RUNNER_CPUS"
  if [ "$cpus" -lt "$RUNNER_CPUS" ]; then
    JOB_CPUS="$cpus"
  fi
  memory="$(docker info --format '{{.MemTotal}}' | awk '{printf "%.1f", $1 / 1073741824}')"
  note "each job gets $JOB_CPUS CPUs, as a hosted runner has $RUNNER_CPUS"
  if awk -v m="$memory" 'BEGIN { exit !(m < 12) }'; then
    warn "Docker has $memory GiB of memory and a hosted runner has $RUNNER_MEMORY_GIB to itself." \
      "The Linux race shards can be killed for memory here. A step that is, says so."
  fi
}

# check_coverage asks the runner whether LINUX_JOBS still names every job and step that gates main.
check_coverage() {
  say "Every job and step that gates main has a phase here"
  printf '%s' "$LINUX_JOBS" | in_job -i -v "$GIT_COMMON:$GIT_COMMON:ro" -- \
    python3 /ci/source/scripts/ci-local-runner.py coverage
}

# linux_job runs one workflow job in a fresh container of the CI image, through the runner.
linux_job() {
  local workflow="$1" job="$2" name license mounts=() network=() env=() matrix=() kv
  shift 2
  # The image is checked again before every job, so one pruned during a long run is rebuilt rather
  # than looked for on Docker Hub.
  docker image inspect "$IMAGE" >/dev/null 2>&1 || ensure_image
  JOB_SEQ=$((JOB_SEQ + 1))
  name="stci-$RUN_ID-$JOB_SEQ-$job"
  mounts=(
    -v "$GIT_COMMON:$GIT_COMMON:ro"
    -v "$LOOMSEAL_DIR:/ci/loomseal:ro"
    -v /var/run/docker.sock:/run/ci-local/host-docker.sock
    -v "$GO_MOD_CACHE:/home/runner/go/pkg/mod"
    -v "$GO_BUILD_CACHE:/home/runner/.cache/go-build"
    -v "$RUN_DIR/artifacts:/ci/artifacts"
  )
  if [ "$LOOMSEAL_COMMON" != "$LOOMSEAL_DIR/.git" ]; then
    mounts+=(-v "$LOOMSEAL_COMMON:$LOOMSEAL_COMMON:ro")
  fi
  if [ -n "${SUPERTEST_LICENSE_FILE:-}" ]; then
    license="$SUPERTEST_LICENSE_FILE"
    [ -r "$license" ] || die "cannot read SUPERTEST_LICENSE_FILE: $license"
    license="$(cd "$(dirname "$license")" && pwd -P)/$(basename "$license")"
    mounts+=(-v "$license:/ci/secrets/SUPERTEST_LICENSE:ro")
  fi
  case "$HOST_NETWORK_JOBS" in
    *" $job "*)
      shared_tmp
      network=(--network host)
      mounts+=(-v "$SHARED_TMP:$SHARED_TMP")
      env+=(-e "TMPDIR=$SHARED_TMP")
      ;;
    *) network=(--hostname ci-local.runner.internal) ;;
  esac
  env+=(-e "CI_LOCAL_RUN_ID=$RUN_ID" -e "CI_LOCAL_CONTAINER=$name"
    -e "CI_LOCAL_SUPERTEST_TIER=${CI_LOCAL_SUPERTEST_TIER:-}")
  if [ -n "${CI_LOCAL_MATRIX:-}" ]; then
    read -ra matrix <<< "$CI_LOCAL_MATRIX"
    for kv in "${matrix[@]}"; do
      set -- "$@" --matrix "$kv"
    done
  fi
  in_job --init --name "$name" --cpus "$JOB_CPUS" "${network[@]}" "${env[@]}" "${mounts[@]}" -- \
    python3 /ci/source/scripts/ci-local-runner.py --workflow "$workflow" --job "$job" "$@"
}

# linux_phase runs the LINUX_JOBS rows of one phase, one container per job, with the steps of a
# split job passed together.
linux_phase() {
  local workflow job step args=() last=""
  while IFS='|' read -r workflow job step; do
    [ -n "$workflow" ] || continue
    if [ -n "$last" ] && [ "$last" != "$workflow|$job" ]; then
      linux_job "${args[@]}"
      args=()
    fi
    if [ "${#args[@]}" -eq 0 ]; then
      args=("$workflow" "$job")
    fi
    if [ -n "$step" ]; then
      args+=(--step "$step")
    fi
    last="$workflow|$job"
  done <<< "$(table_rows "$1")"
  if [ "${#args[@]}" -gt 0 ]; then
    linux_job "${args[@]}"
  fi
}

# run_phase runs one phase: its part on this machine, if it has one, then its Linux jobs.
run_phase() {
  local phase="$1" started=$SECONDS
  CURRENT_PHASE="$phase"
  say "phase $phase"
  case "$phase" in
    build) host_build ;;
    lint) host_lint ;;
    race) host_race ;;
  esac
  if [ "$FAST" != 1 ]; then
    linux_phase "$phase"
  fi
  ok "phase $phase in $((SECONDS - started))s"
  TIMES="$TIMES $phase $((SECONDS - started))s,"
}

# cleanup removes every container this run started, found by this run's own label, and the run
# directory unless the run failed or CI_LOCAL_KEEP asks for it. The EXIT trap calls it.
#
# A run passed only if main reached its end. bash 3.2 hands the EXIT trap a status of zero when it
# stops a script over an unset variable, so the status alone would report such a stop as a pass.
# shellcheck disable=SC2329
cleanup() {
  local code=$? ids
  trap - EXIT INT TERM
  if [ "$PASSED" != 1 ] && [ "$code" -eq 0 ]; then
    code=1
  fi
  ids="$(docker ps -aq --filter "label=$LABEL=$RUN_ID" 2>/dev/null || true)"
  if [ -n "$ids" ]; then
    printf '%s\n' "$ids" | xargs docker rm -f >/dev/null 2>&1 || true
  fi
  if [ "$code" -ne 0 ]; then
    printf '\n\033[31mci-local FAILED in phase %s after %ss.\033[0m\n' "${CURRENT_PHASE:-setup}" \
      "$SECONDS" >&2
    printf 'The log, the Playwright report, and the step summaries are in %s\n' "$RUN_DIR" >&2
  elif [ "${CI_LOCAL_KEEP:-}" = 1 ]; then
    printf 'Kept %s\n' "$RUN_DIR"
  else
    rm -rf "$RUN_DIR"
  fi
  exit "$code"
}

# select_phases reads the command line into FAST and SELECTED.
select_phases() {
  local phase arg
  FAST=0
  SELECTED=()
  case "${1:-}" in
    fast)
      [ "$#" -eq 1 ] || die "fast runs build and race on this machine and takes no phase names"
      FAST=1
      SELECTED=(build race)
      return
      ;;
    "") SELECTED=("${PHASES[@]}"); return ;;
  esac
  for arg in "$@"; do
    case " ${PHASES[*]} " in
      *" $arg "*) ;;
      *) die "no phase named $arg. The phases are: ${PHASES[*]}" ;;
    esac
  done
  for phase in "${PHASES[@]}"; do
    for arg in "$@"; do
      if [ "$arg" = "$phase" ]; then
        SELECTED+=("$phase")
      fi
    done
  done
}

# main runs the selected phases. It is called on the script's last line, beside the exit that ends
# the script, so bash has read every line before any of it runs, and editing the file during a run
# cannot change that run. It is written for the bash 3.2 macOS ships, where expanding an empty array
# under set -u is an error.
main() {
  case "${1:-}" in
    -h | --help) usage; return ;;
    -l | --list) list; return ;;
  esac
  select_phases "$@"
  RUN_DIR="$(mktemp -d "${CI_LOCAL_DIR:-${TMPDIR:-/tmp}}/switchtender-ci-local.XXXXXX")"
  RUN_DIR="$(cd "$RUN_DIR" && pwd -P)"
  mkdir -p "$RUN_DIR/artifacts"
  chmod 1777 "$RUN_DIR/artifacts"
  JOB_SEQ=0
  CURRENT_PHASE=""
  TIMES=""
  PASSED=0
  trap cleanup EXIT
  trap 'exit 130' INT TERM
  exec > >(tee -a "$RUN_DIR/ci-local.log") 2>&1

  command -v docker >/dev/null || die "Docker is required"
  docker info >/dev/null 2>&1 || die "the Docker daemon is not running"
  read_pins
  export GOTOOLCHAIN="go$GO_VERSION"
  say "ci-local $RUN_ID: ${SELECTED[*]}"
  note "Go $GO_VERSION, as setup-go installs it. Run directory: $RUN_DIR"
  GIT_DIR="$(git rev-parse --absolute-git-dir)"
  GIT_COMMON="$(git rev-parse --path-format=absolute --git-common-dir)"
  loomseal_checkout
  LOOMSEAL_COMMON="$(git -C "$LOOMSEAL_DIR" rev-parse --path-format=absolute --git-common-dir)"
  if [ "$FAST" != 1 ]; then
    size_jobs
    ensure_image
    make_caches
    make_snapshot
    check_coverage
  fi
  if [ "${CI_LOCAL_SUPERTEST_TIER:-}" = community ]; then
    warn "CI_LOCAL_SUPERTEST_TIER=community: the supertest skips the Team phases CI runs"
  fi

  for phase in "${SELECTED[@]}"; do
    run_phase "$phase"
  done

  CURRENT_PHASE=""
  PASSED=1
  say "ci-local passed: ${SELECTED[*]}, in $((SECONDS / 60))m$((SECONDS % 60))s"
  note "phase times:${TIMES%,}"
  if [ "$FAST" = 1 ]; then
    warn "fast mode ran the checks on $HOST_NAME only. The Linux jobs CI runs did not run"
  elif [ "${#SELECTED[@]}" -lt "${#PHASES[@]}" ]; then
    warn "only the named phases ran. A push is covered by a run of every phase"
  fi
  if [ -n "${CI_LOCAL_MATRIX:-}" ]; then
    warn "CI_LOCAL_MATRIX=$CI_LOCAL_MATRIX narrowed the matrix jobs to part of what CI runs"
  fi
  if [ "${CI_LOCAL_SUPERTEST_TIER:-}" = community ]; then
    warn "the supertest ran its Community phases only. CI also runs the Team phases"
  fi
}

main "$@"; exit
