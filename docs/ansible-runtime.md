# Managed Ansible runtime

SwitchTender runs Ansible as a separate program and never links it into the binary, because Ansible
is GPLv3. That used to mean installing Ansible on every server and worker before an Ansible run could
start. The managed runtime does that install for you. One command builds a Python virtual
environment under the server's data directory, installs a pinned ansible-core release into it, and
checks every file pip installs against a hash this binary carries:

    switchtender ansible install

Runs, inventory resolution, the inventory cross-check, project requirements installs, and doctor
use it from the next run on, with no restart. A smart or constructed inventory is resolved when the
run is submitted, with the Ansible in use at that moment. When the run starts, it looks up its
Ansible once more, and its cross-check, its play, and their evidence use that one, whatever an
install or a remove does while it runs. It is still a separate program: the server starts its
`ansible-playbook` and `ansible-inventory` the same way it starts the ones on PATH.

The container image already ships Ansible on PATH and does not need it.

## What it needs

A `python3` that the release supports as a control node, with its `venv` module, and access to
PyPI, a mirror named with `--index-url`, or a directory of wheels, as described under
[offline installs](#offline-installs):

| ansible-core | Python |
|--------------|--------|
| 2.16.19 | 3.10 to 3.12 |
| 2.17.14 | 3.10 to 3.12 |
| 2.18.19 | 3.11 to 3.13 |
| 2.19.13 | 3.11 to 3.13 |
| 2.20.9 | 3.12 to 3.14 |
| 2.21.4 | 3.12 to 3.14 |

The install tries `python3` on PATH, then `python3.N` for each version in the range, newest first.
`--python` names an interpreter instead. When none fits, the install stops before it builds an
environment and says which range the release needs and which interpreters it found. The default
release needs Python 3.12 or newer, so on an older Python pick an older release: `--version 2.19`
on Python 3.11, and `--version 2.17` on Python 3.10. The `python3` macOS ships is 3.9, which no
pinned release supports, so install a newer Python there first. On Debian and Ubuntu the `venv`
module is a separate package, such as `python3.12-venv`, and the install names the package when it
is missing.

ansible-core does not run on Windows as a control node, so the install refuses there.

## Install

    switchtender ansible install                  # the newest supported release, 2.21.4
    switchtender ansible install --version 2.18   # the release this build pins for 2.18

Any release in the supported range can be installed, and the newest is the default. A minor version
picks the release this build pins for it. Another patch release of a supported minor is refused with
the pinned one named, since there is no lock for it.

An install finds a supported Python, creates the environment in a directory nothing else uses,
checks the Python the environment itself runs against the release's range and records that version,
installs the release's lock with pip in hash-checking mode, runs the environment's `ansible
--version` and checks it reports the pinned release, and only then writes the marker that makes the
environment count as installed and makes it current. An install never deletes or rebuilds an
existing environment. A failed install removes only the directory it was building and leaves
`current` as it was, so the runtime in use before stays in use.

Running the install again for an installed release verifies it and makes it the one in use, with
nothing reinstalled, as long as this binary's lock for that release has not changed. Installing
another release keeps the first one and makes the new one current.

The install prints pip's progress to standard error and the result as JSON to standard output.

## Where it lives

The runtime directory is the first of these that is set:

1. `--dir` on the `ansible` commands, or `--ansible-runtime-dir` on `serve` and `worker`.
2. The `SWITCHTENDER_ANSIBLE_RUNTIME_DIR` environment variable.
3. `ansible/` in the data directory. That is the directory of the SQLite database, `switchtender.db`
   in the working directory unless `--db` or `SWITCHTENDER_DB` says otherwise. A PostgreSQL
   install has no database file, so it uses `switchtender/ansible` in the account's configuration
   directory.

Each install has its own environment, `<runtime directory>/<release>-<digest>`, named for the
release and the start of its lock's SHA-256, with a random suffix when that name is taken, and the
file `current` names the one in use. A server started with `--db
/var/lib/switchtender/switchtender.db` looks in `/var/lib/switchtender/ansible`.

An upgrade of SwitchTender can carry a new lock for a release that is already installed. Installing
it then builds a new environment beside the one in use, and runs keep using the old one until the
new one verifies.

Run the install as the account the server runs as, pointed at the same database or directory. The
install prints the directory it used. Doctor shows the directory the server looks in, and an error
that says Ansible is needed gives the install line with that directory in it.

`switchtender desktop` keeps its database in a per-user directory, `~/Library/Application
Support/SwitchTender` on macOS and `~/.config/SwitchTender` on Linux, so point the install at the
`ansible` directory inside it:

    switchtender ansible install --dir "$HOME/Library/Application Support/SwitchTender/ansible"

Each worker runs its plays with its own Ansible, so install on every worker host as the worker's
account. A worker looks in `ansible/` beside its `--db` value, and a relay worker started with
`--server` does too, although it opens no database. A worker on a shared PostgreSQL database looks
in `switchtender/ansible` in its account's configuration directory. `--ansible-runtime-dir` on
`worker` makes the location explicit.

The runtime directory, every directory above it, and everything in an environment must belong to
the account that uses it or to root, and no other account may be able to write them. The install
refuses a runtime directory that fails this, and never runs anything in an environment that fails
it. A server refuses to run from such a runtime too. So `sudo switchtender ansible install` into a
directory the server's account owns, such as the default one, is refused: a play running as that
account could have left code there for the install to run as root. A volume mounted
group-writable, as Kubernetes mounts one with `fsGroup`, fails the check as well. The container
image ships Ansible on PATH, so it needs no managed runtime.

## Which Ansible a run uses

The server picks the Ansible commands in this order:

1. The configured directory, `--ansible-bin` or `SWITCHTENDER_ANSIBLE_BIN`, which holds
   `ansible-playbook` and `ansible-inventory`. When it is set, it is used even if those commands are
   missing from it, so a mistake fails the run rather than running another Ansible nobody chose. A
   relative value is resolved once, against the server's working directory when it starts, never
   against a project's checkout. The value `system` selects the commands on PATH even when a
   managed runtime is installed.
2. The managed runtime, when `current` names one. A runtime in use that is broken, unfinished, or
   that another account can write is not skipped: every run that needs Ansible fails with the
   reason, and doctor reports it, rather than quietly running whatever is first on PATH.
3. The commands on PATH, when no managed runtime is in use.

The order is the same for playbook runs, inventory resolution, the cross-check, `ansible-galaxy`
when a project's requirements install, and doctor. A run in a container image uses the image's own
Ansible.

## Supply chain

- The locks ship in the binary, one per supported release. In the repository they are under
  `internal/ansibleruntime/locks`. Each pins ansible-core and every package it depends on to an
  exact version, with the SHA-256 of every wheel PyPI publishes for that version.
- pip runs in hash-checking mode. A file whose hash the lock does not record fails the install with
  nothing installed. A mirror or a proxy can serve the files, because only the recorded bytes pass.
- Only prebuilt wheels are installed, so nothing is compiled and no build step runs code the lock
  does not pin.
- The lock is read before pip sees it and refused if it holds anything but pinned, hashed
  requirements: no index or find-links options, no included files, no editable or URL
  requirements, and no environment variables. Because pip splits lines at more than a newline,
  the lock is also refused if it holds a carriage return, a form feed, a Unicode line separator,
  or any other character pip would read as a line break or a control character, so no line can
  hide from the check inside a comment.
- The interpreter and pip run isolated. No `PYTHON*` or `PIP_*` variable and no pip
  configuration file reaches them, so no requirement, index, or code comes from anywhere but the
  lock and the command line. What is honored is `--wheels`, `--index-url`, and the proxy and
  certificate variables `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`, `ALL_PROXY`, `SSL_CERT_FILE`,
  `SSL_CERT_DIR`, `REQUESTS_CA_BUNDLE`, and `CURL_CA_BUNDLE`.
- Once pip finishes, the install takes group and other write permission off everything in the
  environment, so a permissive umask does not leave it open to other accounts.
- `switchtender ansible lock` prints the lock a release installs from.

The hashes cover what pip installs, not what happens to the files afterwards. A host run executes as
the server's account, so a playbook can change a runtime that account owns, just as it can change an
Ansible installed with pipx in that account's home. To keep runs from changing it, install it as
another account, such as root, into a directory the server can read but not write, and point the
server at it:

    sudo switchtender ansible install --dir /opt/switchtender/ansible
    switchtender serve --ansible-runtime-dir /opt/switchtender/ansible ...

The server only reads the runtime directory. Installing and removing are the only operations that
write to it.

## Offline installs

On a machine with network access, with the server's operating system, CPU architecture, and Python
version:

    switchtender ansible lock --version 2.21 > ansible-core.lock
    python3 -m pip download --require-hashes --only-binary=:all: --dest wheels -r ansible-core.lock

pip can also fetch for another machine when told the target with `--platform` and
`--python-version`. Copy the `wheels` directory to the server and install from it alone:

    switchtender ansible install --version 2.21 --wheels ./wheels

A relative `--wheels` is taken from the directory the command runs in, and it must be an existing
directory. The hashes are checked the same way. To install from an internal mirror instead, name it
with `--index-url`, and the hashes are checked against what it serves. pip's environment variables
and configuration files are not read.

## List and remove

    switchtender ansible list
    switchtender ansible remove --version 2.18
    switchtender ansible remove

`list` prints every installed environment, the Python each one runs, and which one is current.
`remove` with a version removes every environment of that release, and when one of them was in use
the newest one left becomes current. `remove` alone removes every release, and the runtime directory
too when nothing else is in it, and runs go back to the Ansible on PATH. Only directories the
installer made are removed. A remove refuses, removing nothing, while a play or an inventory read is
running from an environment it would delete. Run it again once they finish.

## Evidence and doctor

When Ansible resolves a smart or constructed inventory, or cross-checks a natively resolved one
before a play, the run's record names the ansible-core version and where it came from: `managed`,
`configured`, `path`, or `image`. A cross-check comes from the lookup the run made when it started,
the same one its play runs from. A smart or constructed inventory's resolution comes from the
lookup made when the run was submitted, so a run that waited for approval while the runtime changed
records the release that resolved its inventory, which can differ from the one its play ran. The
run dossier reads, for example, "Ansible, ansible-core 2.21.4 from the managed runtime". The outcome
record commits to both.

Doctor, at `/ui/doctor` and `GET /v1/doctor`, reports the server's ansible-core version, where its
commands come from, their directory, and the runtime directory it looks in, and warns when the
version is outside the releases the [inventory engine](inventories.md) is tested against. A
managed runtime in use that cannot be used is a broken finding, with the reason and the line that
installs it again.

## Regenerating the locks

    python3 scripts/ansible-runtime-locks.py

It needs `uv` and network access to PyPI. It reads the ansible-core matrix from
`.github/workflows/ci.yml`, resolves each release with
`uv pip compile --universal --generate-hashes --only-binary :all:` against PyPI alone, and writes
one lock per release with the Python range read from that release's metadata on PyPI. A test fails
when the locks and the tested releases differ. Review the diff before committing it. A changed hash
for a version that did not change means PyPI served different bytes, and that is a reason to stop.
