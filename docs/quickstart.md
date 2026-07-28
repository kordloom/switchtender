<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Quickstart

No install needed to look around: the [live demo](https://demo.switchtender.com) is a seeded, read-only instance of exactly what you get.

## Requirements

Ansible on the PATH: `ansible-playbook` and `ansible-inventory`. Nothing else for the default
SQLite setup. Go 1.26 to build from source, or Docker Compose, where `docker compose up --build`
builds the image from this repository.

## Run the server

    go build -o switchtender .
    SWITCHTENDER_ENCRYPTION_KEY=change-me SWITCHTENDER_ENCRYPTION_SALT=change-me-too \
      ./switchtender serve --addr :8080 --db switchtender.db

The key and salt together seal stored credentials at rest with argon2id and AES-256-GCM. Without
both the server still runs, but credential features stay off. Keep the salt stable across restarts
or existing credentials cannot be decrypted.

Open http://localhost:8080 for the web UI, or use the API directly.

On your own machine, `./switchtender desktop` does all of this in one command: it picks a stable
loopback port, keeps its data in a per-user directory, and opens the UI. The
[desktop guide](desktop.md) covers it, including packaging.

## Submit a run

    curl -X POST localhost:8080/v1/runs \
      -d '{"playbook": "site.yml", "inventory": "hosts.ini"}'

The response carries a run id. Fetch its status, its structured events, or its log:

    curl localhost:8080/v1/runs/<id>
    curl localhost:8080/v1/runs/<id>/events
    curl localhost:8080/v1/runs/<id>/logs

Add `"shards": 4` to the body to split the run across four slices of the inventory, balanced by
each host's measured duration in recent runs.

## Add a worker

Point a worker at the same database and it competes for queued runs:

    SWITCHTENDER_ENCRYPTION_KEY=change-me SWITCHTENDER_ENCRYPTION_SALT=change-me-too \
      ./switchtender worker --db switchtender.db --name laptop

For more than one machine, use a PostgreSQL DSN as the `--db` value on every process.

## Lock down the API

Creating the first token turns on authentication. Until then the API is open so a fresh install
works immediately.

    ./switchtender token new --db switchtender.db --name ci

Create user accounts with roles for sign-in:

    SWITCHTENDER_PASSWORD=secret ./switchtender user new operator-jane --role operator --db switchtender.db

## Run with Docker

    export SWITCHTENDER_ENCRYPTION_KEY=change-me
    export SWITCHTENDER_ENCRYPTION_SALT=change-me-too
    docker compose up --build

This starts a server, a PostgreSQL database, and a worker. The server listens on port 8080. Set
`SWITCHTENDER_PORT` to change the host port.

## Set up a production server

For a real install, `init` generates the encryption key and salt, creates the first admin account, and
writes a config file in one step. It can also write a systemd unit:

    ./switchtender init --db switchtender.db --config switchtender.env --systemd switchtender.service

It prints the admin password once, so save it. Move the unit into place and start it:

    sudo cp switchtender.service /etc/systemd/system/switchtender.service
    sudo systemctl enable --now switchtender

Serve HTTPS directly, with no reverse proxy in front, by pointing the server at a certificate and key:

    ./switchtender serve --db switchtender.db --tls-cert tls.crt --tls-key tls.key

## Run on Kubernetes

SwitchTender needs no operator. A Helm chart installs the server and a worker as ordinary pods sharing a
database:

    helm install switchtender ./deploy/helm/switchtender

## Try the demo

To look around without setting anything up, run the seeded demo. It fills a fresh database with
sample projects, templates, inventories, and real runs, including a flaky host, a split, and a
pipeline, then serves it read-only so it is safe to expose:

    ./switchtender demo --addr :8080

Or with Docker: `docker compose --profile demo up --build`.
