<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Quickstart

No install needed to look around. The [live demo](https://demo.switchtender.com/ui/runs?status=pending_approval) is a seeded, read-only instance of exactly what you get.

## Requirements

An Ansible run needs ansible-core, which the binary does not carry. Use the `ansible-playbook` and
`ansible-inventory` already on your PATH, or, once the binary is installed, let
`switchtender ansible install` put a pinned, hash-checked ansible-core beside the database, as
[install Ansible](#install-ansible) shows. That install needs a `python3` from 3.10 to 3.14 with its
`venv` module, and it does not run on Windows.

The other tools come from the PATH: `terraform`, `tofu`, `python3`, `pwsh` (PowerShell 7), or `go`.
Bash runs use the system shell. Nothing else for the default SQLite setup. Building from source
instead of installing the release binary needs Go 1.26, and Docker Compose is an alternative, where
`docker compose --profile stack up --build` builds the image from this repository. That image
already carries Ansible, Python, Terraform, and OpenTofu.

## Install

    curl -fsSL https://switchtender.com/install.sh | sh

On a host with `wget` and no `curl`, `wget -qO- https://switchtender.com/install.sh | sh` does the
same. The script downloads the release binary for your platform, checks it against the published
checksums, and installs it. It writes to `/usr/local/bin` when it can and to `~/.local/bin`
otherwise, which is what happens on a stock Mac and on any Linux install without root. That second
directory is often not on PATH, so the script says so and prints its closing commands with the full
path; add the line it gives you to run `switchtender` by name. Running in about a minute. Prefer to build it
yourself, or want to hack on it? `go build -o switchtender .` from a clone produces the same
binary; the commands below assume it is on your PATH, so prefix a locally built binary with `./`.

## Install Ansible

Skip this when `ansible-playbook` is already on your PATH, or when you only run the other tools.
Otherwise run the install in the directory you will start the server in, as the account that will
run it:

    switchtender ansible install

It downloads ansible-core 2.21.4 from PyPI, checks every file against a hash the binary carries, and
installs it into `ansible/` beside `switchtender.db`, where the server finds it with no restart.
That release needs Python 3.12 to 3.14. On Python 3.11 add `--version 2.19`, and on 3.10
`--version 2.17`. The `python3` macOS ships is 3.9, so install a newer Python there first. Debian
and Ubuntu package the `venv` module separately, such as `python3.12-venv`, and the install names
the package when it is missing. The directory and every directory above it must be writable only by
that account or root, or the install refuses it. [Managed Ansible runtime](ansible-runtime.md)
covers offline installs, workers, and the desktop app.

## Run the server

    export SWITCHTENDER_ENCRYPTION_KEY=$(openssl rand -hex 32)
    export SWITCHTENDER_ENCRYPTION_SALT=$(openssl rand -hex 16)
    switchtender serve --addr :8080 --db switchtender.db

The key and salt together seal stored credentials at rest with argon2id and AES-256-GCM. Keep both
with your other secrets: a server started with a different pair cannot open the credentials sealed
under this one. Without them the server still runs, but credential features stay off.

The first start on an empty database mints an initial admin token and prints it once, so the API
is authenticated from the first request. Export it for the commands below:

    export ST_TOKEN=<the token serve printed>

Open http://localhost:8080 for the web UI and sign in with that token, or use the API directly.

A fresh install opens with an empty templates list. `switchtender examples --db switchtender.db`
seeds a handful of starter templates that run with no project, inventory, or credential, so a first
launch works on the spot. It skips a template whose name is already present, so it is safe to re-run.

On your own machine, `switchtender desktop` does all of this in one command. It picks a stable
loopback port, keeps its data in a per-user directory, and opens the UI. The
[desktop guide](desktop.md) covers it, including packaging.

## Submit a run

The first one needs nothing on disk, so it succeeds on an install that is minutes old:

    curl -X POST localhost:8080/v1/runs \
      -H "Authorization: Bearer $ST_TOKEN" \
      -d '{"tool": "bash", "command": "echo hello from switchtender"}'

An Ansible run takes a playbook and an inventory. The server resolves both relative to its own
working directory, so `site.yml` and `hosts.ini` have to exist there, or the run names a
[project](concepts.md) and they are resolved inside that checkout instead. Without either the run
is submitted, accepted, and then fails with "the playbook: site.yml could not be found":

    curl -X POST localhost:8080/v1/runs \
      -H "Authorization: Bearer $ST_TOKEN" \
      -d '{"playbook": "site.yml", "inventory": "hosts.ini"}'

The response carries a run id. Fetch its status, its structured events, or its log:

    curl -H "Authorization: Bearer $ST_TOKEN" localhost:8080/v1/runs/<id>
    curl -H "Authorization: Bearer $ST_TOKEN" localhost:8080/v1/runs/<id>/events
    curl -H "Authorization: Bearer $ST_TOKEN" localhost:8080/v1/runs/<id>/logs

Add `"shards": 4` to the body to split the run across four slices of the inventory, balanced by
each host's measured duration in recent runs.

## Add a worker (Team)

Distributed execution is a Team feature, so every `switchtender worker` needs a license and refuses
to start without one. Community runs everything on the server itself, which is the default and needs
no extra process: this section is for when one machine is no longer enough.

Point a worker at the same database, with the same key and salt set as the server's, and it
competes for queued runs:

    switchtender worker --db switchtender.db --name laptop

For more than one machine, use a PostgreSQL DSN as the `--db` value on every process.

Queues are part of the same feature: naming one on a run, a template, or an inventory source routes
it to a worker serving that name, so it is refused on Community rather than accepted and left with
nothing able to claim it.

## Lock down the API

The initial admin token from the first start is yours to keep, but a shared install deserves an
account for each person and each CI job, with tokens bound to them, so the audit trail says who did
what and each token carries only its account's role. Create the accounts:

    SWITCHTENDER_PASSWORD=secret switchtender user new operator-jane --role operator --db switchtender.db
    SWITCHTENDER_PASSWORD=secret switchtender user new ci-deploy --role operator --db switchtender.db

Then mint a token for each, bound with `--user`:

    switchtender token new --db switchtender.db --user ci-deploy --name ci

A token minted without `--user` is an admin token that names no account, like the initial one, so
keep those few.

A loopback bind, or `--read-only`, starts without minting an initial token: a loopback API is
reachable only from this machine, and a read-only one changes nothing. On a loopback bind the API
asks for a token from the moment the install holds any token or account.

## Run with Docker

The compose file lives in the repository, so this one needs a checkout rather than the installed
binary:

    git clone https://github.com/kordloom/switchtender
    cd switchtender
    export SWITCHTENDER_ENCRYPTION_KEY=$(openssl rand -hex 32)
    export SWITCHTENDER_ENCRYPTION_SALT=$(openssl rand -hex 16)
    docker compose --profile stack up --build

This starts one server on SQLite, with its database and its signing key in a volume, so both
survive a restart and an upgrade. The server listens on port 8080. Set `SWITCHTENDER_PORT` to change
the host port. With no terminal to show it on, the initial admin token is written to
`/data/initial-admin-token` in the container, readable by the service user alone:

    docker compose --profile stack exec server cat /data/initial-admin-token

On a Team license, the `team` profile runs PostgreSQL, a server, and a worker instead. Put the license
at `./switchtender-license.json`, or name it with `SWITCHTENDER_LICENSE_FILE`, and give every process
the same signing key, since processes that share one database sign as one install:

    export SWITCHTENDER_AUDIT_KEY=$(openssl rand -hex 32)
    docker compose --profile team up --build

Keep that key where you can restore it, beside the encryption key and salt.

## Set up a production server

For a real install, `init` generates the encryption key and salt, creates the first admin account, and
writes a config file in one step. It can also write a systemd unit:

    sudo useradd --system --create-home --home-dir /var/lib/switchtender switchtender
    sudo -u switchtender -H switchtender init --db /var/lib/switchtender/switchtender.db \
      --config /var/lib/switchtender/switchtender.env \
      --systemd /var/lib/switchtender/switchtender.service

The unit runs the server as the account that ran `init`, and every run the server executes without
an execution image runs as that account too, which is why `init` runs here as an account made for
the job rather than as you or as root. It prints the admin password once, so save it. Move the unit
into place and start it:

    sudo cp /var/lib/switchtender/switchtender.service /etc/systemd/system/switchtender.service
    sudo systemctl enable --now switchtender

The unit listens on loopback, which suits a reverse proxy on the same machine. To serve HTTPS
directly with nothing in front, give it a certificate and key and listen on the network: add
`--addr :8443 --tls-cert /path/to/tls.crt --tls-key /path/to/tls.key` to the unit's `ExecStart`
line, then `sudo systemctl daemon-reload && sudo systemctl restart switchtender`. Run by hand, the
server reads the same key and salt from the file `init` wrote:

    set -a; . /var/lib/switchtender/switchtender.env; set +a
    switchtender serve --db /var/lib/switchtender/switchtender.db --addr :8443 \
      --tls-cert /path/to/tls.crt --tls-key /path/to/tls.key

## Run on Kubernetes

The Helm chart runs the server as an ordinary Deployment, with no operator. By default it is the
Community shape: one server on SQLite, with the whole install on a persistent volume.

The chart is in the repository, so clone it first if you installed the binary alone. Write the keys
to a file before you install, so you keep them. The salt has to stay the same for the life of the
install, because every stored credential is sealed against it, and a key or a salt generated afresh
on an upgrade makes every stored secret unreadable:

    git clone https://github.com/kordloom/switchtender
    cd switchtender
    printf 'encryptionKey: "%s"\nencryptionSalt: "%s"\nauditKey: "%s"\n' \
      "$(openssl rand -hex 32)" "$(openssl rand -hex 16)" "$(openssl rand -hex 32)" \
      > switchtender-secrets.yaml
    chmod 600 switchtender-secrets.yaml
    helm install switchtender ./deploy/helm/switchtender -f switchtender-secrets.yaml \
      --set fullnameOverride=switchtender

Keep that file in a secret manager, and pass the same `-f switchtender-secrets.yaml` to every
`helm upgrade`. Or put the three values in a Secret of your own under `SWITCHTENDER_ENCRYPTION_KEY`,
`SWITCHTENDER_ENCRYPTION_SALT`, and `SWITCHTENDER_AUDIT_KEY` and pass `--set existingSecret=<name>`.
Copy the values across before you switch an install over: the chart keeps its own Secret rather than
deleting it, but the pods read only the one you name.

`auditKey` is the install's signing seed. On SQLite the server mints one itself when it is empty,
and setting it means you hold the copy to restore. On PostgreSQL it is required, since every server
and worker has to sign as the same install and none of them mints the key.

`fullnameOverride` names the resources `switchtender` and `switchtender-server` rather than repeating
the name. Set it on a new install only: renaming an existing install's resources orphans its data
volume and its keys.

The install prints how to reach the interface. With the default ClusterIP service:

    kubectl port-forward svc/switchtender 8080:8080

The first server to start writes an initial admin token to `/data` in its pod, readable by the
service user alone:

    kubectl exec deploy/switchtender-server -- cat /data/initial-admin-token

Sign in with it, create named accounts and tokens, and delete it.

On a Team license, point `database.dsn` at PostgreSQL to run more than one server, and set
`worker.enabled=true` for dedicated workers. Pass the license with
`--set-file license=switchtender-license.json`: `--set` parses commas and braces and changes it. The
DSN reaches the pods from a Secret rather than as an argument, so the password is not in the pod
spec.

`helm uninstall` leaves the data volume and the Secret holding the keys in place. Delete them
yourself once you are sure. `extraEnv`, `extraEnvFrom`, `extraVolumes`, and `extraVolumeMounts`
carry anything the chart has no key for, such as a secret source's token or a CA bundle.

## Try the demo

To look around without setting anything up, run the seeded demo. It fills a fresh database with
sample projects, templates, inventories, and real runs, including a flaky host, a split, and a
pipeline, then serves it read-only so it is safe to expose:

    switchtender demo --addr :8080

Or with Docker, from a checkout: `docker compose --profile demo up --build`, which serves on port
8081 so it can run beside the stack. `SWITCHTENDER_DEMO_PORT` moves it.
