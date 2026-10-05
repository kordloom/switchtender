#!/usr/bin/env python3
"""Run one job of this repository's GitHub workflows the way a hosted runner does.

scripts/ci-local.sh starts a container of scripts/ci-local.Dockerfile for each job it mirrors and
runs this inside it. The job's run steps execute verbatim: each step's own script, under bash -e,
with the job's env, the step's env, the matrix values, the secrets this machine was given, and what
earlier steps wrote to GITHUB_ENV and GITHUB_PATH. Copying the scripts into ci-local.sh instead is
how a local mirror drifts from CI one edit at a time, so nothing here restates what a step does.

What cannot run verbatim maps to a local equivalent, each with its reason:

  - The uses steps call actions that exist only on GitHub. Checkout, setup-go, setup-node, the
    linter's action, and upload-artifact have local equivalents in USES, and the site deploy is
    declined there by name. Anything else stops the job with the action's name, so a new action
    cannot be skipped by accident.
  - A few run steps reach GitHub or compare the tree against the commit. Their local equivalents
    are in LOCAL_STEPS, each pinned to a digest of the CI script it stands in for: when that step
    changes in the workflow, the job stops and says so, instead of quietly running the old version.
  - The downloads CI's own steps make are served from the copies the image fetched at build time,
    through the curl the steps call. See serve_download.

It runs as root only long enough to give the runner account the Docker socket and the curl that
serves those downloads, and then runs the job as runner, as GitHub does.
"""

import argparse
import glob
import hashlib
import itertools
import json
import os
import pwd
import re
import shlex
import shutil
import subprocess
import sys
import time

import yaml

# SOURCE is the tree under test, unpacked from the run's snapshot before this starts: the files a
# push would commit, and a .git file naming the repository's own git directory, which is mounted
# read-only. Every checkout copies from it, and nothing writes to it.
SOURCE = "/ci/source"
# LOOMSEAL is the LoomSeal checkout at the version go.mod pins, mounted read-only.
LOOMSEAL = "/ci/loomseal"
# ARTIFACTS is where upload-artifact and the step summaries land, a directory on the host.
ARTIFACTS = "/ci/artifacts"
# SECRETS holds one file per secret this machine was given, named for the secret.
SECRETS = "/ci/secrets"
# HOST_DOCKER is the host daemon's socket, which only root can open.
HOST_DOCKER = "/run/ci-local/host-docker.sock"
# DOCKER_SOCKET is where the runner account finds a Docker socket it may use, as on a hosted runner.
DOCKER_SOCKET = "/var/run/docker.sock"
# MEMORY_EVENTS counts, among other things, the processes of this container the kernel killed for
# want of memory, on a host with cgroup v2.
MEMORY_EVENTS = "/sys/fs/cgroup/memory.events"
# DOWNLOADS holds the files CI's steps download, indexed by the URL each step names.
DOWNLOADS = "/opt/ci-local/downloads"
# REAL_CURL is the system curl, which serve_download hands every other request to.
REAL_CURL = "/usr/bin/curl"
# CURL_SHIM is the curl the steps find first on PATH.
CURL_SHIM = "/usr/local/bin/curl"
# WORK is the runner's work directory, laid out as GitHub lays out its own.
WORK = "/home/runner/work"
# REPOSITORY is the GitHub repository the workflows run in.
REPOSITORY = "kordloom/switchtender"
# RUN_LABEL is the label every container this run starts carries, so ci-local.sh removes them all.
RUN_LABEL = "switchtender.ci-local"


class JobError(Exception):
    """JobError reports a job that cannot run or did not pass, with the reason to print."""


def say(text):
    """say prints a progress line in bold, flushed so it orders correctly with step output."""
    print(f"\n\033[1m== {text}\033[0m", flush=True)


def note(text):
    """note prints an indented detail line."""
    print(f"   {text}", flush=True)


def digest(text):
    """digest returns the short SHA-256 of a step script, which LOCAL_STEPS pins."""
    return hashlib.sha256(text.encode()).hexdigest()[:16]


def loomseal_script(dest, export):
    """loomseal_script stands in for CI's clone of the LoomSeal repository.

    CI clones the repository from GitHub and checks out the version go.mod pins, looked up under
    either tag form. This looks the version up the same way in the checkout ci-local.sh mounted,
    refuses one at any other commit, and puts it where CI's clone would be. dest is that place, as
    the step names it, and export is the GITHUB_ENV line the step writes afterward, if any.
    """
    lines = [
        "set -euo pipefail",
        "version=\"$(go list -m -f '{{.Version}}' github.com/kordloom/loomseal)\"",
        "echo \"cross-checking against loomseal $version\"",
        "ref=\"$version\"",
        f"git -C {LOOMSEAL} rev-parse --verify --quiet \"refs/tags/$ref\" >/dev/null || "
        "ref=\"snapshot/$version\"",
        f"want=\"$(git -C {LOOMSEAL} rev-parse \"$ref^{{commit}}\")\"",
        f"have=\"$(git -C {LOOMSEAL} rev-parse HEAD)\"",
        "if [ \"$want\" != \"$have\" ]; then",
        "  echo \"the LoomSeal checkout is at $have, not at $ref ($want)\" >&2",
        "  exit 1",
        "fi",
        f"ln -s {LOOMSEAL} {dest}",
    ]
    if export:
        lines.append(export)
    return "\n".join(lines) + "\n"


# SITEGEN_LOCAL holds CI's freshness rule with the commit replaced by the tree under test. CI's tree
# is the commit, so "sitegen changes nothing the commit holds" is "git diff is empty afterward".
# Here the tree may carry changes not committed yet, so the same rule is "sitegen changes nothing
# the tree holds", which is the diff unchanged by running it.
SITEGEN_LOCAL = """set -euo pipefail
before="$(git diff -- site/ | sha256sum)"
go run ./cmd/sitegen
if [ "$(git diff -- site/ | sha256sum)" != "$before" ]; then
  echo "::error title=Generated site content is stale::run 'go run ./cmd/sitegen' and commit the \
result. Never edit the generated output by hand: sitegen rewrites it."
  git --no-pager diff --stat -- site/
  exit 1
fi
"""

# UNCOMMITTED_SCAN scans what the push will add that is not a commit yet. The history scan before it
# reads commits only, so a secret in a file not yet committed passes it here and fails in CI, where
# the push has made that file part of the history.
UNCOMMITTED_SCAN = """set -euo pipefail
pending="$(mktemp -d)"
{ git diff --name-only HEAD --; git ls-files --others --exclude-standard; } | sort -u |
  while IFS= read -r f; do
    if [ -f "$f" ]; then
      mkdir -p "$pending/$(dirname "$f")"
      cp "$f" "$pending/$f"
    fi
  done
count="$(find "$pending" -type f | wc -l | tr -d ' ')"
echo "scanning $count changed or new files the history does not hold yet"
cd "$pending"
/tmp/gitleaks dir . --config "$GITHUB_WORKSPACE/.gitleaks.toml" --redact --no-banner --verbose
"""

# LOCAL_STEPS are the run steps that cannot run off GitHub as written, keyed by workflow, job, and
# step name. Each records the digest of the CI script it replaces, its local script, and why.
LOCAL_STEPS = {
    ("ci.yml", "test", "Check out the loomseal version this module depends on"): {
        "ci": "055b561833276635",
        "why": "CI clones LoomSeal from GitHub. The checkout ci-local.sh mounted is used instead, "
               "after the same version lookup.",
        "script": loomseal_script(
            "\"$RUNNER_TEMP/loomseal\"",
            "echo \"SWITCHTENDER_LOOMSEAL_REPO=$RUNNER_TEMP/loomseal\" >> \"$GITHUB_ENV\""),
    },
    ("ci.yml", "crossverify", "Check out the loomseal version this module depends on"): {
        "ci": "200e064fef79ca1e",
        "why": "CI clones LoomSeal from GitHub beside the repository. The checkout ci-local.sh "
               "mounted is placed there instead, after the same version lookup.",
        "script": loomseal_script("../loomseal", ""),
    },
    ("ci.yml", "checks", "Generated site content is current"): {
        "ci": "6d6e2ffe3a7218c6",
        "why": "CI compares the regenerated site with the commit, and this tree is not committed "
               "yet, so the rule is held against the tree instead.",
        "script": SITEGEN_LOCAL,
    },
}

# COMMUNITY_STEPS replace the supertest's two license steps when CI_LOCAL_SUPERTEST_TIER is
# "community", for a machine that does not hold the Team license. CI always runs the Team phases,
# so ci-local.sh says in its result when a run did not.
COMMUNITY_STEPS = {
    ("supertest.yml", "supertest", "Write the Team license"): {
        "ci": "e48e2df71e05a567",
        "why": "CI_LOCAL_SUPERTEST_TIER=community: no Team license is written.",
        "script": "echo 'the Team phases will not run'\n",
    },
    ("supertest.yml", "supertest", "Run the supertest"): {
        "ci": "95f5c02e6bd8378a",
        "why": "CI_LOCAL_SUPERTEST_TIER=community: the supertest runs with -skip-team.",
        "script": "set -euo pipefail\n"
                  "go run ./test/supertest \\\n"
                  "  -skip-team \\\n"
                  "  -shots /tmp/supertest-shots \\\n"
                  "  -report \"$GITHUB_STEP_SUMMARY\"\n",
    },
}

# LOCAL_EXTRA_STEPS run after the CI step they are keyed by, for what a local run must check that
# CI checks some other way.
LOCAL_EXTRA_STEPS = {
    ("ci.yml", "checks", "Secret scan"): {
        "name": "Secret scan of the changes not committed yet",
        "script": UNCOMMITTED_SCAN,
    },
}

# USES maps each action a workflow calls to the local function that stands in for it.
USES = {}


def uses(name):
    """uses registers fn as the local equivalent of the action called name."""
    def register(fn):
        USES[name] = fn
        return fn
    return register


class Job:
    """Job is one run of a workflow job: one matrix combination on a fresh workspace."""

    def __init__(self, workflow, job_id, spec, matrix, head):
        # workflow is the workflow file's name, such as ci.yml.
        self.workflow = workflow
        # job_id is the job's key in the workflow.
        self.job_id = job_id
        # spec is the job's parsed definition.
        self.spec = spec
        # matrix holds this combination's matrix values.
        self.matrix = matrix
        # head is the commit the tree under test was taken from.
        self.head = head
        # workspace is GITHUB_WORKSPACE.
        self.workspace = os.path.join(WORK, "switchtender", "switchtender")
        # temp is RUNNER_TEMP.
        self.temp = os.path.join(WORK, "_temp")
        # env holds what earlier steps wrote to GITHUB_ENV.
        self.env = {}
        # paths holds what earlier steps wrote to GITHUB_PATH, the most recent first.
        self.paths = []
        # failed reports whether a step has failed, which if: failure() reads.
        self.failed = False
        # services are the containers started for this job's services block.
        self.services = []
        # summaries are the step summary files the steps were given.
        self.summaries = []

    def title(self):
        """title returns the job's display name with the matrix values filled in."""
        name = str(self.spec.get("name", self.job_id))
        return f"{self.workflow} {self.expand(name)}"

    def context(self):
        """context returns the values ${{ }} expressions read."""
        return {
            "matrix": {k: str(v) for k, v in self.matrix.items()},
            "github": {
                "workspace": self.workspace,
                "repository": REPOSITORY,
                "ref": "refs/heads/main",
                "ref_name": "main",
                "sha": self.head,
                "event_name": "push",
                "job": self.job_id,
                "token": "",
                "server_url": "https://github.com",
            },
            "runner": {"temp": self.temp, "os": "Linux", "arch": runner_arch()},
            "env": {**self.spec.get("env", {}), **self.env},
            "secrets": SecretReader(),
        }

    def expand(self, text):
        """expand replaces every ${{ }} expression in text with its value."""
        if not isinstance(text, str):
            return text
        ctx = self.context()
        return re.sub(r"\$\{\{\s*(.*?)\s*\}\}", lambda m: evaluate(m.group(1), ctx), text)


class SecretReader:
    """SecretReader looks secrets up in SECRETS, reading an absent one as empty, as GitHub does."""

    def get(self, name, default=""):
        """get returns the secret called name, or default when this machine was not given it."""
        path = os.path.join(SECRETS, name)
        if os.path.isfile(path):
            with open(path, encoding="utf-8") as f:
                return f.read()
        return default


def evaluate(expr, ctx):
    """evaluate returns the value of one ${{ }} expression.

    It reads dotted context paths and quoted strings, joined by ||, which is every form the
    workflows use. Anything else stops the job, because guessing at an expression's value is how a
    local run would come to test something CI does not.
    """
    for option in [part.strip() for part in expr.split("||")]:
        if len(option) >= 2 and option[0] == option[-1] == "'":
            value = option[1:-1]
        else:
            head, _, rest = option.partition(".")
            if head not in ctx or not rest:
                raise JobError(f"ci-local cannot evaluate the expression ${{{{ {expr} }}}}")
            value = ctx[head].get(rest, "")
        if value:
            return str(value)
    return ""


def condition_holds(job, cond):
    """condition_holds evaluates a step's if: condition, which defaults to success()."""
    if cond is None:
        return not job.failed
    text = str(cond).strip()
    match = re.fullmatch(r"\$\{\{\s*(.*?)\s*\}\}", text)
    if match:
        text = match.group(1)
    known = {
        "always()": True,
        "success()": not job.failed,
        "failure()": job.failed,
        "cancelled()": False,
        "!cancelled()": True,
    }
    if text not in known:
        raise JobError(f"ci-local cannot evaluate the condition if: {cond}")
    return known[text]


def runner_arch():
    """runner_arch returns RUNNER_ARCH for this machine."""
    machine = os.uname().machine
    return {"x86_64": "X64", "aarch64": "ARM64"}.get(machine, machine.upper())


def matrix_combinations(spec, wanted):
    """matrix_combinations lists the job's matrix combinations, narrowed to the wanted values.

    wanted maps a matrix key to the values to keep. A key the job's matrix does not define narrows
    nothing, so one setting such as shard=4,5 can be given to every job of a phase.
    """
    matrix = (spec.get("strategy") or {}).get("matrix") or {}
    keys = [k for k in matrix if k not in ("include", "exclude")]
    combos = [dict(zip(keys, values)) for values in itertools.product(*(matrix[k] for k in keys))]
    if not keys:
        combos = []
    combos.extend(dict(entry) for entry in matrix.get("include", []))
    for gone in matrix.get("exclude", []):
        combos = [c for c in combos if any(str(c.get(k)) != str(v) for k, v in gone.items())]
    if not combos:
        combos = [{}]
    for key, values in wanted.items():
        if any(key in c for c in combos):
            combos = [c for c in combos if str(c.get(key)) in values]
    if not combos:
        raise JobError(f"the job has no matrix combination with {wanted}")
    return combos


def read_env_file(path):
    """read_env_file parses what a step wrote to GITHUB_ENV, including NAME<<DELIMITER blocks."""
    out = {}
    if not os.path.exists(path):
        return out
    with open(path, encoding="utf-8") as f:
        lines = f.read().split("\n")
    i = 0
    while i < len(lines):
        line = lines[i]
        i += 1
        if not line.strip():
            continue
        eq, heredoc = line.find("="), line.find("<<")
        if heredoc != -1 and (eq == -1 or heredoc < eq):
            name, delimiter = line[:heredoc], line[heredoc + 2:]
            value = []
            while i < len(lines) and lines[i] != delimiter:
                value.append(lines[i])
                i += 1
            i += 1
            out[name] = "\n".join(value)
        else:
            out[line[:eq]] = line[eq + 1:]
    return out


def read_path_file(path):
    """read_path_file returns the directories a step wrote to GITHUB_PATH, in the order written."""
    if not os.path.exists(path):
        return []
    with open(path, encoding="utf-8") as f:
        return [line.strip() for line in f if line.strip()]


def oom_kills():
    """oom_kills returns how many processes of this container the kernel has killed for memory.

    It returns zero where the count is not kept, under cgroup v1, which leaves a killed test to
    report itself as killed without the cause named.
    """
    if not os.path.exists(MEMORY_EVENTS):
        return 0
    with open(MEMORY_EVENTS, encoding="utf-8") as f:
        for line in f:
            key, _, value = line.partition(" ")
            if key == "oom_kill":
                return int(value)
    return 0


def step_failure(name, code, killed):
    """step_failure says why a step failed, naming a lack of memory when that is the cause."""
    if not killed:
        return f"step \"{name}\" exited {code}"
    with open("/proc/meminfo", encoding="utf-8") as f:
        total = int(f.readline().split()[1]) / 1024 / 1024
    return (f"step \"{name}\" exited {code} after the kernel killed {killed} of its processes for "
            f"memory. Docker has {total:.1f} GiB and a hosted runner has 16 GiB to itself, so "
            "this failure is this machine's and not the code's: give Docker more memory, or stop "
            "what else is using it, and run the phase again.")


def git(*args, cwd=None):
    """git runs a read-only git command and returns its output."""
    out = subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True, text=True)
    return out.stdout.strip()


def checkout(job, step):
    """checkout gives the job the tree under test where actions/checkout would put it.

    The files are the tree under test. What git can see depends on fetch-depth, as it does in CI.
    With fetch-depth 0 the job sees the repository's whole history and every tag, through a .git
    file naming the repository's own git directory, read-only. With the default depth of one it
    sees a checkout of that depth: one commit, no history behind it, and no tags. That is built as a
    .git directory borrowing the repository's objects, so a job whose checkout lacks the tags a
    test needs fails here the way it failed in CI.
    """
    opts = step.get("with") or {}
    other = job.expand(str(opts.get("repository", REPOSITORY)))
    if other != REPOSITORY:
        raise JobError(f"the job checks out {other}, and ci-local only has this repository")
    dest = os.path.join(job.workspace, job.expand(str(opts.get("path", ""))))
    os.makedirs(dest, exist_ok=True)
    subprocess.run(["cp", "-a", SOURCE + "/.", dest], check=True)
    depth = str(opts.get("fetch-depth", "1"))
    if depth == "0":
        note(f"full history and tags, as fetch-depth 0 fetches, in {dest}")
        return
    with open(os.path.join(dest, ".git"), encoding="utf-8") as f:
        gitdir = f.read().strip().removeprefix("gitdir:").strip()
    common = git("--git-dir", gitdir, "rev-parse", "--path-format=absolute", "--git-common-dir")
    os.remove(os.path.join(dest, ".git"))
    shallow = os.path.join(dest, ".git")
    for sub in ("objects/info", "refs/heads", "refs/tags"):
        os.makedirs(os.path.join(shallow, sub))
    files = {
        "HEAD": "ref: refs/heads/main\n",
        "refs/heads/main": job.head + "\n",
        "shallow": job.head + "\n",
        "objects/info/alternates": os.path.join(common, "objects") + "\n",
        "config": "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n",
    }
    for name, text in files.items():
        with open(os.path.join(shallow, name), "w", encoding="utf-8") as f:
            f.write(text)
    note(f"one commit and no tags, as fetch-depth {depth} fetches, in {dest}")


@uses("actions/checkout")
def use_checkout(job, step):
    """use_checkout stands in for actions/checkout."""
    checkout(job, step)


@uses("actions/setup-go")
def use_setup_go(job, step):
    """use_setup_go checks that the image's Go is the version setup-go would install."""
    opts = step.get("with") or {}
    want = str(opts.get("go-version", ""))
    if not want:
        with open(os.path.join(job.workspace, opts["go-version-file"]), encoding="utf-8") as f:
            text = f.read()
        found = re.search(r"(?m)^toolchain go(\S+)", text) or re.search(r"(?m)^go (\S+)", text)
        want = found.group(1)
    have = subprocess.run(["go", "env", "GOVERSION"], check=True, capture_output=True,
                          text=True).stdout.strip().removeprefix("go")
    if have != want:
        raise JobError(f"setup-go would install Go {want} and the image has {have}. ci-local.sh "
                       "rebuilds the image when go.mod changes, so run it again.")
    note(f"Go {have}")


@uses("actions/setup-node")
def use_setup_node(job, step):
    """use_setup_node checks that the image's Node is on the major line setup-node is given."""
    want = str((step.get("with") or {}).get("node-version", ""))
    have = subprocess.run(["node", "--version"], check=True, capture_output=True,
                          text=True).stdout.strip().removeprefix("v")
    if have.split(".")[0] != want.split(".")[0]:
        raise JobError(f"setup-node would install Node {want} and the image has {have}")
    note(f"Node {have}")


@uses("golangci/golangci-lint-action")
def use_golangci_lint(job, step):
    """use_golangci_lint runs the linter release the action is told to download."""
    opts = step.get("with") or {}
    want = str(opts.get("version", "")).removeprefix("v")
    version = subprocess.run(["golangci-lint", "version"], check=True, capture_output=True,
                             text=True).stdout
    if f"version {want} " not in version:
        raise JobError(f"the lint job runs golangci-lint {want} and the image has: {version}")
    args = shlex.split(job.expand(str(opts.get("args", ""))))
    workdir = os.path.join(job.workspace, job.expand(str(opts.get("working-directory", ""))))
    killed = oom_kills()
    code = run_script(job, step, "golangci-lint run " + shlex.join(args) + "\n", workdir, {})
    if code != 0:
        raise JobError(step_failure("golangci-lint run", code, oom_kills() - killed))


@uses("actions/upload-artifact")
def use_upload_artifact(job, step):
    """use_upload_artifact copies what the step would upload into the run's artifact directory."""
    opts = step.get("with") or {}
    name = job.expand(str(opts.get("name", "artifact")))
    target = os.path.join(ARTIFACTS, re.sub(r"[^A-Za-z0-9._-]+", "-", name))
    found = 0
    for pattern in job.expand(str(opts.get("path", ""))).splitlines():
        pattern = pattern.strip()
        if not pattern:
            continue
        for match in glob.glob(os.path.join(job.workspace, pattern), recursive=True):
            rel = os.path.relpath(match, job.workspace)
            if rel.startswith(".."):
                rel = os.path.basename(match.rstrip("/"))
            copy_into(match, os.path.join(target, rel))
            found += 1
    if found:
        note(f"kept {name} in the run's artifacts directory on the host")
    elif str(opts.get("if-no-files-found", "warn")) == "error":
        raise JobError(f"upload-artifact {name} found no files")


@uses("cloudflare/wrangler-action")
def use_wrangler(job, step):
    """use_wrangler declines to deploy, which only the real workflow may do."""
    note("not run: this deploys the site to Cloudflare with the repository's token")


def copy_into(src, dst):
    """copy_into copies a file or a directory tree to dst, making its parents."""
    os.makedirs(os.path.dirname(dst), exist_ok=True)
    if os.path.isdir(src):
        shutil.copytree(src, dst, symlinks=True, dirs_exist_ok=True)
    else:
        shutil.copy2(src, dst)


def base_env(job):
    """base_env returns the environment a hosted runner gives every step of the job."""
    env = dict(os.environ)
    for name in [n for n in env if n.startswith("CI_LOCAL_")]:
        del env[name]
    env.update({
        "HOME": "/home/runner",
        "USER": "runner",
        "LOGNAME": "runner",
        "CI": "true",
        "GITHUB_ACTIONS": "true",
        "GITHUB_WORKSPACE": job.workspace,
        "GITHUB_REPOSITORY": REPOSITORY,
        "GITHUB_REPOSITORY_OWNER": REPOSITORY.split("/")[0],
        "GITHUB_SERVER_URL": "https://github.com",
        "GITHUB_REF": "refs/heads/main",
        "GITHUB_REF_NAME": "main",
        "GITHUB_REF_TYPE": "branch",
        "GITHUB_SHA": job.head,
        "GITHUB_EVENT_NAME": "push",
        "GITHUB_JOB": job.job_id,
        "GITHUB_RUN_ID": "0",
        "GITHUB_RUN_NUMBER": "0",
        "RUNNER_OS": "Linux",
        "RUNNER_ARCH": runner_arch(),
        "RUNNER_TEMP": job.temp,
        "RUNNER_TOOL_CACHE": "/opt/hostedtoolcache",
        "ImageOS": "ubuntu24",
    })
    return env


def run_script(job, step, script, workdir, step_env):
    """run_script runs one step script the way the runner does, and applies what it exported."""
    stamp = f"{len(job.summaries):03d}"
    files = {name: os.path.join(job.temp, "_runner_file_commands", f"{name}_{stamp}")
             for name in ("env", "path", "output", "state", "summary")}
    os.makedirs(os.path.dirname(files["env"]), exist_ok=True)
    for path in files.values():
        open(path, "w", encoding="utf-8").close()
    job.summaries.append(files["summary"])
    env = base_env(job)
    env.update({k: job.expand(str(v)) for k, v in (job.spec.get("env") or {}).items()})
    env.update(job.env)
    env.update({k: job.expand(str(v)) for k, v in step_env.items()})
    env["PATH"] = os.pathsep.join(job.paths + [env["PATH"]])
    env.update({
        "GITHUB_ENV": files["env"],
        "GITHUB_PATH": files["path"],
        "GITHUB_OUTPUT": files["output"],
        "GITHUB_STATE": files["state"],
        "GITHUB_STEP_SUMMARY": files["summary"],
    })
    if not os.path.isdir(workdir):
        raise JobError(f"the working directory {workdir} does not exist")
    script_path = os.path.join(job.temp, f"step_{stamp}.sh")
    with open(script_path, "w", encoding="utf-8") as f:
        f.write(script)
    shell = step.get("shell")
    if shell in (None, ""):
        argv = ["bash", "-e", script_path]
    elif shell == "bash":
        argv = ["bash", "--noprofile", "--norc", "-eo", "pipefail", script_path]
    elif shell == "sh":
        argv = ["sh", "-e", script_path]
    else:
        raise JobError(f"ci-local runs bash and sh steps, and this step asks for {shell}")
    code = subprocess.run(argv, cwd=workdir, env=env).returncode
    job.env.update(read_env_file(files["env"]))
    for directory in read_path_file(files["path"]):
        job.paths.insert(0, directory)
    return code


def start_services(job):
    """start_services starts the job's service containers where the job reaches them as CI does.

    On a hosted runner the job runs on the host, and a service's published port answers on
    localhost. Here each service joins this container's network namespace instead, so localhost
    reaches it on the port it listens on, with nothing published on the host to collide with.
    """
    for name, spec in (job.spec.get("services") or {}).items():
        container = f"{os.environ['CI_LOCAL_CONTAINER']}-{name}"
        argv = ["docker", "run", "-d", "--name", container,
                "--label", f"{RUN_LABEL}={os.environ.get('CI_LOCAL_RUN_ID', '')}",
                "--network", f"container:{os.environ['CI_LOCAL_CONTAINER']}"]
        for key, value in (spec.get("env") or {}).items():
            argv += ["-e", f"{key}={job.expand(str(value))}"]
        argv += shlex.split(job.expand(str(spec.get("options", ""))))
        argv.append(job.expand(str(spec["image"])))
        subprocess.run(argv, check=True, stdout=subprocess.DEVNULL)
        job.services.append(container)
        note(f"service {name}: {spec['image']}, reachable on localhost as on a hosted runner")
        wait_healthy(container)


def wait_healthy(container):
    """wait_healthy waits for a service's health check to pass, as the runner does before step 1."""
    fmt = "{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}"
    deadline = time.monotonic() + 300
    while time.monotonic() < deadline:
        state = subprocess.run(["docker", "inspect", "--format", fmt, container],
                               capture_output=True, text=True).stdout.strip()
        if state in ("healthy", "running"):
            return
        if state in ("unhealthy", "exited", "dead"):
            break
        time.sleep(1)
    raise JobError(f"service container {container} did not become healthy")


def stop_services(job):
    """stop_services removes the job's service containers and the anonymous volumes their images
    declare, such as PostgreSQL's data directory, which would otherwise outlive every run."""
    for container in job.services:
        subprocess.run(["docker", "rm", "-f", "-v", container], capture_output=True)
    job.services = []


def step_name(job, step):
    """step_name returns how GitHub names a step in its log."""
    if "name" in step:
        return job.expand(str(step["name"]))
    if "uses" in step:
        return "Run " + step["uses"].split("@")[0]
    return "Run " + str(step.get("run", "")).strip().splitlines()[0]


def run_job(job, selected):
    """run_job runs every step of one job and raises JobError at the first that fails."""
    say(job.title())
    started = time.monotonic()
    shutil.rmtree(job.workspace, ignore_errors=True)
    shutil.rmtree(job.temp, ignore_errors=True)
    os.makedirs(job.workspace)
    os.makedirs(job.temp)
    failure = None
    try:
        start_services(job)
        for step in job.spec.get("steps", []):
            raw_name = str(step.get("name", ""))
            name = step_name(job, step)
            if selected and "uses" not in step and raw_name not in selected:
                continue
            if not condition_holds(job, step.get("if")):
                continue
            result = run_step(job, step, raw_name, name)
            failure = failure or result
    finally:
        stop_services(job)
        keep_summaries(job)
    elapsed = time.monotonic() - started
    limit = job.spec.get("timeout-minutes")
    if limit and elapsed > float(limit) * 60:
        note(f"\033[33mtook {elapsed / 60:.1f} minutes, over the {limit} CI allows this job\033[0m")
    if failure:
        raise JobError(failure)
    note(f"\033[32mok\033[0m {job.title()} in {elapsed:.0f}s")


def run_step(job, step, raw_name, name):
    """run_step runs one step and returns why it failed, or None when it passed."""
    print(f"\n-- {name}", flush=True)
    key = (job.workflow, job.job_id, raw_name)
    try:
        if "uses" in step:
            action = step["uses"].split("@")[0]
            if action not in USES:
                raise JobError(f"the step uses {action}, which has no local equivalent. Add one "
                               "to USES in scripts/ci-local-runner.py.")
            USES[action](job, step)
            return None
        script = str(step["run"])
        workdir = job.expand(str(step.get("working-directory") or
                                 ((job.spec.get("defaults") or {}).get("run") or {})
                                 .get("working-directory") or ""))
        workdir = os.path.join(job.workspace, workdir)
        local = LOCAL_STEPS.get(key)
        if os.environ.get("CI_LOCAL_SUPERTEST_TIER") == "community":
            local = local or COMMUNITY_STEPS.get(key)
        if local:
            if digest(script) != local["ci"]:
                raise JobError(f"{job.workflow} changed the step \"{raw_name}\" in job "
                               f"{job.job_id} (digest {digest(script)}), and ci-local runs a local "
                               "equivalent written for the old text. Update its LOCAL_STEPS entry "
                               "in scripts/ci-local-runner.py to match.")
            note("local equivalent: " + local["why"])
            script = local["script"]
        killed = oom_kills()
        code = run_script(job, step, job.expand(script), workdir, step.get("env") or {})
        if code != 0:
            raise JobError(step_failure(name, code, oom_kills() - killed))
        extra = LOCAL_EXTRA_STEPS.get(key)
        if extra:
            print(f"\n-- {extra['name']} (local)", flush=True)
            code = run_script(job, step, extra["script"], workdir, step.get("env") or {})
            if code != 0:
                raise JobError(step_failure(extra["name"], code, oom_kills() - killed))
        return None
    except JobError as err:
        if step.get("continue-on-error"):
            note(f"continuing past a failure the workflow allows: {err}")
            return None
        job.failed = True
        print(f"\033[31m   failed: {err}\033[0m", flush=True)
        return f"{job.title()}: {err}"


def keep_summaries(job):
    """keep_summaries copies the step summaries into the run's artifact directory."""
    text = ""
    for path in job.summaries:
        if os.path.exists(path):
            with open(path, encoding="utf-8") as f:
                text += f.read()
    if text.strip():
        name = re.sub(r"[^A-Za-z0-9._-]+", "-", job.title()) + "-summary.md"
        with open(os.path.join(ARTIFACTS, name), "w", encoding="utf-8") as f:
            f.write(text)


def serve_download(argv):
    """serve_download is the curl CI's steps call: it serves their downloads from the image.

    A step that downloads a pinned tool names the build for GitHub's x86-64 runner. The image
    fetched each one at build time, for this machine, under the URL the step names, so the step
    runs as written, offline, and gets a binary that runs here. Any other request goes to the real
    curl untouched.
    """
    with open(os.path.join(DOWNLOADS, "index.json"), encoding="utf-8") as f:
        index = json.load(f)
    url = next((a for a in argv if a in index), None)
    if url is None:
        os.execv(REAL_CURL, [REAL_CURL, *argv])
    output = None
    for i, arg in enumerate(argv):
        if arg in ("-o", "--output") and i + 1 < len(argv):
            output = argv[i + 1]
        elif arg.startswith("--output="):
            output = arg.split("=", 1)[1]
        elif re.fullmatch(r"-[A-Za-z]*o", arg) and i + 1 < len(argv):
            output = argv[i + 1]
        elif re.fullmatch(r"-[A-Za-z]*o.+", arg) and not arg.startswith("--"):
            output = arg[arg.index("o") + 1:]
    with open(os.path.join(DOWNLOADS, index[url]), "rb") as src:
        data = src.read()
    if output is None:
        sys.stdout.buffer.write(data)
        return 0
    with open(output, "wb") as dst:
        dst.write(data)
    return 0


def prepare_as_root():
    """prepare_as_root sets up what the runner account needs and then becomes that account.

    A hosted runner's account reaches Docker through the docker group. The host's socket here is
    root's alone, and changing it would change it for every container on the machine, so a proxy
    owned by the docker group takes its place, at the path the runner's own socket has.
    """
    if os.getuid() != 0:
        return
    if os.path.exists(HOST_DOCKER):
        # socat ends a connection half a second after either side closes its half, and docker run
        # closes its half as soon as it has no input to send, so every line a container printed
        # after that half second was lost. -t waits for the daemon to finish instead. A client that
        # hangs up mid-reply makes socat log a broken pipe, and the client reports any failure that
        # matters itself, so socat's own log is discarded.
        subprocess.Popen(["socat", "-t", "86400",
                          f"UNIX-LISTEN:{DOCKER_SOCKET},fork,unlink-early,user=runner,"
                          "group=docker,mode=660",
                          f"UNIX-CONNECT:{HOST_DOCKER}"], stderr=subprocess.DEVNULL)
        deadline = time.monotonic() + 10
        while not os.path.exists(DOCKER_SOCKET) and time.monotonic() < deadline:
            time.sleep(0.05)
    with open(CURL_SHIM, "w", encoding="utf-8") as f:
        f.write(f"#!/bin/sh\nexec python3 {os.path.abspath(__file__)} curl \"$@\"\n")
    os.chmod(CURL_SHIM, 0o755)
    os.makedirs(ARTIFACTS, exist_ok=True)
    runner = pwd.getpwnam("runner")
    os.initgroups("runner", runner.pw_gid)
    os.setgid(runner.pw_gid)
    os.setuid(runner.pw_uid)
    os.environ.update({"HOME": runner.pw_dir, "USER": "runner", "LOGNAME": "runner"})


def head_commit():
    """head_commit returns the commit the tree under test was taken from."""
    with open(os.path.join(SOURCE, ".git"), encoding="utf-8") as f:
        gitdir = f.read().strip().removeprefix("gitdir:").strip()
    return git("--git-dir", gitdir, "rev-parse", "HEAD")


def runs_for_main(workflow):
    """runs_for_main reports whether a workflow runs on a pull request or on a push to main."""
    on = workflow.get("on", workflow.get(True))
    if isinstance(on, str):
        on = {on: None}
    elif isinstance(on, list):
        on = dict.fromkeys(on)
    for event in ("push", "pull_request"):
        if event not in (on or {}):
            continue
        filters = on[event] or {}
        branches = filters.get("branches")
        if event == "push" and branches is None and ("tags" in filters or
                                                     "tags-ignore" in filters):
            continue
        if branches is None or "main" in branches:
            return True
    return False


def check_coverage(mapping):
    """check_coverage holds ci-local.sh's table of jobs to every workflow that gates main.

    mapping holds the table's rows: workflow, job, and the step a row runs when a job is split
    across phases. Every job of every workflow that runs on a pull request or a push to main must
    be in it, and a split job must have every run step in it, so a job or step added to a workflow
    stops ci-local until it is mirrored, rather than passing here while CI runs something new.
    """
    rows = {}
    for line in mapping.splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        cells = [cell.strip() for cell in line.split("|")]
        workflow, job_id, step = cells[1], cells[2], cells[3] if len(cells) > 3 else ""
        rows.setdefault((workflow, job_id), set()).add(step)
    problems = []
    directory = os.path.join(SOURCE, ".github", "workflows")
    for name in sorted(os.listdir(directory)):
        if not name.endswith((".yml", ".yaml")):
            continue
        with open(os.path.join(directory, name), encoding="utf-8") as f:
            workflow = yaml.safe_load(f)
        if not runs_for_main(workflow):
            continue
        for job_id, spec in (workflow.get("jobs") or {}).items():
            steps = rows.pop((name, job_id), None)
            if steps is None:
                problems.append(f"{name} job {job_id} runs on main and ci-local does not run it")
                continue
            if "" in steps:
                continue
            for step in spec.get("steps", []):
                if "run" in step and str(step.get("name", "")) not in steps:
                    problems.append(f"{name} job {job_id} step \"{step.get('name', step['run'])}\" "
                                    "runs on main and no ci-local phase runs it")
    for workflow, job_id in rows:
        problems.append(f"ci-local runs {workflow} job {job_id}, which no longer exists")
    if problems:
        raise JobError("the workflows and ci-local.sh's job table disagree:\n  " +
                       "\n  ".join(problems) +
                       "\nMap each one in LINUX_JOBS in scripts/ci-local.sh.")
    note("every job and step that gates main has a ci-local phase")
    return 0


def main():
    """main runs the job named on the command line, or serves a download as curl."""
    if len(sys.argv) > 1 and sys.argv[1] == "curl":
        return serve_download(sys.argv[2:])
    if len(sys.argv) > 1 and sys.argv[1] == "coverage":
        return check_coverage(sys.stdin.read())
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--workflow", required=True, help="Workflow file, such as ci.yml.")
    parser.add_argument("--job", required=True, help="Job id in the workflow.")
    parser.add_argument("--step", action="append", default=[],
                        help="Run only this named run step, with the job's uses steps. Repeatable.")
    parser.add_argument("--matrix", action="append", default=[],
                        help="KEY=VALUE[,VALUE]: run only the matrix combinations with these "
                             "values.")
    args = parser.parse_args()
    prepare_as_root()
    path = os.path.join(SOURCE, ".github", "workflows", args.workflow)
    with open(path, encoding="utf-8") as f:
        workflow = yaml.safe_load(f)
    spec = (workflow.get("jobs") or {}).get(args.job)
    if spec is None:
        raise JobError(f"{args.workflow} has no job {args.job}")
    if not str(spec.get("runs-on", "")).startswith("ubuntu"):
        raise JobError(f"{args.job} runs on {spec.get('runs-on')}, and ci-local mirrors Ubuntu")
    wanted = {}
    for item in args.matrix:
        key, _, values = item.partition("=")
        wanted.setdefault(key, set()).update(values.split(","))
    head = head_commit()
    for combo in matrix_combinations(spec, wanted):
        run_job(Job(args.workflow, args.job, spec, combo, head), set(args.step))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except JobError as err:
        print(f"\n\033[31mci-local: {err}\033[0m", file=sys.stderr, flush=True)
        sys.exit(1)
