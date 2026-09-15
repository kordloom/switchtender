<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Quickstart

No install needed to look around. The [live demo](https://demo.switchtender.com) is a seeded, read-only instance of exactly what you get.

## Requirements

Ansible on the PATH: `ansible-playbook` and `ansible-inventory`. Nothing else for the default
SQLite setup. Building from source instead of installing the release binary needs Go 1.26, and
Docker Compose is an alternative, where `docker compose --profile stack up --build` builds the image from this
repository.

## Install

    curl -fsSL https://switchtender.com/install.sh | sh

The script downloads the release binary for your platform, checks it against the published
checksums, and installs it. It writes to `/usr/local/bin` when it can and to `~/.local/bin`
otherwise, which is what happens on a stock Mac and on any Linux install without root. That second
directory is often not on PATH, so the script says so and prints its closing commands with the full
path; add the line it gives you to run `switchtender` by name. Running in about a minute. Prefer to build it
yourself, or want to hack on it? `go build -o switchtender .` from a clone produces the same
binary; the commands below assume it is on your PATH, so prefix a locally built binary with `./`.

## Run the server

    SWITCHTENDER_ENCRYPTION_KEY=change-me SWITCHTENDER_ENCRYPTION_SALT=change-me-too \
      switchtender serve --addr :8080 --db switchtender.db

The key and salt together seal stored credentials at rest with argon2id and AES-256-GCM. Without
both the server still runs, but credential features stay off. Keep the salt stable across restarts
or existing credentials cannot be decrypted.

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

Distributed execution is a Team feature, so every `switchtender worker` needs a licence and refuses
to start without one. Community runs everything on the server itself, which is the default and needs
no extra process: this section is for when one machine is no longer enough.

Point a worker at the same database and it competes for queued runs:

    SWITCHTENDER_ENCRYPTION_KEY=change-me SWITCHTENDER_ENCRYPTION_SALT=change-me-too \
      switchtender worker --db switchtender.db --name laptop

For more than one machine, use a PostgreSQL DSN as the `--db` value on every process.

Queues are part of the same feature: naming one on a run, a template, or an inventory source routes
it to a worker serving that name, so it is refused on Community rather than accepted and left with
nothing able to claim it.

## Lock down the API

The initial admin token from the first start is yours to keep, but a shared install deserves named
tokens so the audit trail says who did what. Mint one per person and per CI job:

    switchtender token new --db switchtender.db --name ci

A loopback bind, or `--read-only`, serves without authentication instead, since neither exposes an
unauthenticated API to the network.

Create user accounts with roles for sign-in:

    SWITCHTENDER_PASSWORD=secret switchtender user new operator-jane --role operator --db switchtender.db

## Run with Docker

The compose file lives in the repository, so this one needs a checkout rather than the installed
binary:

    git clone https://github.com/kordloom/switchtender
    cd switchtender
    export SWITCHTENDER_ENCRYPTION_KEY=change-me
    export SWITCHTENDER_ENCRYPTION_SALT=change-me-too
    docker compose --profile stack up --build

This starts a server and a PostgreSQL database. The server listens on port 8080. Set
`SWITCHTENDER_PORT` to change the host port.

Workers are a separate profile because they are Team: `docker compose --profile stack --profile
workers up --build` adds one, and it needs a licence to start.

## Set up a production server

For a real install, `init` generates the encryption key and salt, creates the first admin account, and
writes a config file in one step. It can also write a systemd unit:

    switchtender init --db switchtender.db --config switchtender.env --systemd switchtender.service

It prints the admin password once, so save it. Move the unit into place and start it:

    sudo cp switchtender.service /etc/systemd/system/switchtender.service
    sudo systemctl enable --now switchtender

Serve HTTPS directly, with no reverse proxy in front, by pointing the server at a certificate and key:

    switchtender serve --db switchtender.db --tls-cert tls.crt --tls-key tls.key

## Run on Kubernetes

SwitchTender needs no operator. A Helm chart installs the server and a worker as ordinary pods sharing a
database:

The chart is in the repository too, so clone it first if you installed the binary alone:

    git clone https://github.com/kordloom/switchtender
    cd switchtender
    helm install switchtender ./deploy/helm/switchtender \
      --set encryptionKey=$(openssl rand -hex 32) \
      --set encryptionSalt=$(openssl rand -hex 16)

Add `--set auditKey=$(openssl rand -hex 32)` too, a 32 byte ed25519 seed as hex, or that install
cannot sign a receipt. A deployment that shares one database will not create a signing key by itself, because
every server and worker has to sign as the same install, and a key minted inside one pod would be
that pod's alone. Without it the chain still records and still verifies, but no receipt, signed
bundle or trust document can be produced, which is most of why anyone runs this. Keep the seed where
you can restore it.

Both values are required, and the salt has to stay the same across upgrades: it is what every
stored secret was sealed against, so a new salt makes the old ones unreadable. Keep them in a
secret manager and pass `--set existingSecret=<name>` instead once you have one. The chart pulls
`ghcr.io/kordloom/switchtender`.

## Try the demo

To look around without setting anything up, run the seeded demo. It fills a fresh database with
sample projects, templates, inventories, and real runs, including a flaky host, a split, and a
pipeline, then serves it read-only so it is safe to expose:

    switchtender demo --addr :8080

Or with Docker, from a checkout: `docker compose --profile demo up --build`.
