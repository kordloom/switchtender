<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Railwarden" width="140">
  </picture>
</p>

# Quickstart

## Requirements

Ansible on the PATH: `ansible-playbook` and `ansible-inventory`. Nothing else for the default
SQLite setup. Go 1.26 to build from source, or use the container image.

## Run the server

    go build -o railwarden .
    RAILWARDEN_ENCRYPTION_KEY=change-me RAILWARDEN_ENCRYPTION_SALT=change-me-too \
      ./railwarden serve --addr :8080 --db railwarden.db

The key and salt together seal stored credentials at rest with argon2id and AES-256-GCM. Without
both the server still runs, but credential features stay off. Keep the salt stable across restarts
or existing credentials cannot be decrypted.

Open http://localhost:8080 for the web UI, or use the API directly.

On your own machine, `./railwarden desktop` does all of this in one command: it picks a stable
loopback port, keeps its data in a per-user directory, and opens the UI. The
[desktop guide](desktop.md) covers it, including packaging.

## Submit a run

    curl -X POST localhost:8080/runs \
      -d '{"playbook": "site.yml", "inventory": "hosts.ini"}'

The response carries a run id. Fetch its status, its structured events, or its log:

    curl localhost:8080/runs/<id>
    curl localhost:8080/runs/<id>/events
    curl localhost:8080/runs/<id>/logs

Add `"shards": 4` to the body to split the run across four slices of the inventory, balanced by
each host's measured duration in recent runs.

## Add a worker

Point a worker at the same database and it competes for queued runs:

    RAILWARDEN_ENCRYPTION_KEY=change-me RAILWARDEN_ENCRYPTION_SALT=change-me-too \
      ./railwarden worker --db railwarden.db --name laptop

For more than one machine, use a PostgreSQL DSN as the `--db` value on every process.

## Lock down the API

Creating the first token turns on authentication. Until then the API is open so a fresh install
works immediately.

    ./railwarden token new --db railwarden.db --name ci

Create user accounts with roles for sign-in:

    RAILWARDEN_PASSWORD=secret ./railwarden user new operator-jane --role operator --db railwarden.db

## Run with Docker

    export RAILWARDEN_ENCRYPTION_KEY=change-me
    export RAILWARDEN_ENCRYPTION_SALT=change-me-too
    docker compose up --build

This starts a server, a PostgreSQL database, and a worker. The server listens on port 8080. Set
`RAILWARDEN_PORT` to change the host port.

## Set up a production server

For a real install, `init` generates the encryption key and salt, creates the first admin account, and
writes a config file in one step. It can also write a systemd unit:

    ./railwarden init --db railwarden.db --config railwarden.env --systemd railwarden.service

It prints the admin password once, so save it. Move the unit into place and start it:

    sudo cp railwarden.service /etc/systemd/system/railwarden.service
    sudo systemctl enable --now railwarden

Serve HTTPS directly, with no reverse proxy in front, by pointing the server at a certificate and key:

    ./railwarden serve --db railwarden.db --tls-cert tls.crt --tls-key tls.key

## Run on Kubernetes

Railwarden needs no operator. A Helm chart installs the server and a worker as ordinary pods sharing a
database:

    helm install railwarden ./deploy/helm/railwarden

## Try the demo

To look around without setting anything up, run the seeded demo. It fills a fresh database with
sample projects, templates, inventories, and real runs, including a flaky host, a split, and a
pipeline, then serves it read-only so it is safe to expose:

    ./railwarden demo --addr :8080

Or with Docker: `docker compose --profile demo up --build`.
