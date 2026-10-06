#!/usr/bin/env python3
"""Regenerate the requirements locks the managed Ansible runtime installs from.

`switchtender ansible install` creates a Python virtual environment and installs one pinned
ansible-core release into it with pip in hash-checking mode, from a lock that ships in the
repository under internal/ansibleruntime/locks. This script writes those locks, one per supported
release. The releases are the ansible-core matrix in .github/workflows/ci.yml, the same list
inventory.TestedAnsibleCoreReleases holds, and a test fails when the locks and that list drift.

Each lock pins ansible-core and every package it depends on to an exact version, with the SHA-256
of every wheel PyPI publishes for that version, so the one lock installs on Linux and macOS, on x86
and Arm, on every Python version the release supports. The header records the release and the
controller Python range it supports, read from the release's own metadata on PyPI: its
Requires-Python is the floor, and its newest Python classifier is the ceiling.

Run it from the repository root with uv on PATH (or named by UV) and network access to PyPI:

    python3 scripts/ansible-runtime-locks.py

It resolves with `uv pip compile --universal --generate-hashes --only-binary :all:` against
https://pypi.org/simple only, ignoring any uv or pip configuration on the machine, and keeps uv's
cache in a temporary directory it removes afterwards. Review the diff before committing it: a
changed hash for a version that did not change means PyPI served different bytes, and that is a
reason to stop, not to regenerate.
"""

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.request

LOCK_DIR = os.path.join("internal", "ansibleruntime", "locks")
CI_WORKFLOW = os.path.join(".github", "workflows", "ci.yml")
INDEX_URL = "https://pypi.org/simple"
COMMAND = "scripts/ansible-runtime-locks.py"


def releases():
    """Return the ansible-core releases the CI conformance matrix pins."""
    with open(CI_WORKFLOW, encoding="utf-8") as f:
        for line in f:
            if re.match(r"^\s*ansible-core: \[", line):
                found = re.findall(r"[0-9]+\.[0-9]+\.[0-9]+", line)
                if found:
                    return found
    sys.exit(f"no ansible-core matrix found in {CI_WORKFLOW}")


def python_range(release):
    """Return the oldest and newest controller Python the release supports, from PyPI."""
    url = f"https://pypi.org/pypi/ansible-core/{release}/json"
    with urllib.request.urlopen(url, timeout=60) as resp:
        info = json.load(resp)["info"]
    floor = re.fullmatch(r">=\s*(3\.[0-9]+)", info.get("requires_python") or "")
    if floor is None:
        sys.exit(f"ansible-core {release}: unexpected Requires-Python {info.get('requires_python')!r}")
    minors = sorted(
        int(m.group(1))
        for c in info.get("classifiers", [])
        if (m := re.fullmatch(r"Programming Language :: Python :: 3\.([0-9]+)", c))
    )
    if not minors:
        sys.exit(f"ansible-core {release}: no Python classifiers to read the newest Python from")
    return floor.group(1), f"3.{minors[-1]}"


def compile_lock(uv, release, oldest, cache):
    """Return uv's hash-locked universal resolution of ansible-core==release."""
    env = dict(os.environ, UV_CACHE_DIR=cache, UV_PYTHON_DOWNLOADS="never", UV_NO_CONFIG="1")
    out = subprocess.run(
        [uv, "pip", "compile", "-", "--universal", "--generate-hashes", "--only-binary", ":all:",
         "--python-version", oldest, "--index-url", INDEX_URL, "--no-progress",
         "--custom-compile-command", COMMAND],
        input=f"ansible-core=={release}\n", capture_output=True, text=True, env=env, check=False)
    if out.returncode != 0:
        sys.exit(f"uv pip compile ansible-core=={release} failed:\n{out.stderr}")
    return out.stdout


def main():
    """Write one lock per supported ansible-core release."""
    uv = os.environ.get("UV") or shutil.which("uv")
    if not uv:
        sys.exit("uv is not on PATH. Install it, or name it with UV=/path/to/uv")
    os.makedirs(LOCK_DIR, exist_ok=True)
    wanted = releases()
    for name in os.listdir(LOCK_DIR):
        if name.endswith(".txt") and name[len("ansible-core-"):-len(".txt")] not in wanted:
            os.remove(os.path.join(LOCK_DIR, name))
            print(f"{os.path.join(LOCK_DIR, name)}: removed, no longer in the CI matrix")
    cache = tempfile.mkdtemp(prefix="ansible-runtime-locks-")
    try:
        for release in wanted:
            oldest, newest = python_range(release)
            body = compile_lock(uv, release, oldest, cache)
            header = (
                f"# The managed Ansible runtime's lock for ansible-core {release}.\n"
                f"# Written by {COMMAND}. Do not edit it by hand.\n"
                f"# ansible-core: {release}\n"
                f"# python: {oldest}-{newest}\n"
                "#\n"
            )
            path = os.path.join(LOCK_DIR, f"ansible-core-{release}.txt")
            with open(path, "w", encoding="utf-8") as f:
                f.write(header + body)
            print(f"{path}: Python {oldest} to {newest}")
    finally:
        shutil.rmtree(cache, ignore_errors=True)


if __name__ == "__main__":
    main()
