<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Quickstart

## Requirements

Ansible on the PATH: `ansible-playbook` and `ansible-inventory`. Nothing else for the default
SQLite setup. Go 1.26 to build from source, or use the container image.

## Run the server

    go build -o yardmaster .
    YARDMASTER_ENCRYPTION_KEY=change-me YARDMASTER_ENCRYPTION_SALT=change-me-too \
      ./yardmaster serve --addr :8080 --db yardmaster.db

The key and salt together seal stored credentials at rest with argon2id and AES-256-GCM. Without
both the server still runs, but credential features stay off. Keep the salt stable across restarts
or existing credentials cannot be decrypted.

Open http://localhost:8080 for the web UI, or use the API directly.

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

    YARDMASTER_ENCRYPTION_KEY=change-me YARDMASTER_ENCRYPTION_SALT=change-me-too \
      ./yardmaster worker --db yardmaster.db --name laptop

For more than one machine, use a PostgreSQL DSN as the `--db` value on every process.

## Lock down the API

Creating the first token turns on authentication. Until then the API is open so a fresh install
works immediately.

    ./yardmaster token new --db yardmaster.db --name ci

Create user accounts with roles for sign-in:

    YARDMASTER_PASSWORD=secret ./yardmaster user new operator-jane --role operator --db yardmaster.db

## Run with Docker

    export YARDMASTER_ENCRYPTION_KEY=change-me
    export YARDMASTER_ENCRYPTION_SALT=change-me-too
    docker compose up --build

This starts a server, a PostgreSQL database, and a worker. The server listens on port 8080. Set
`YARDMASTER_PORT` to change the host port.

## Try the demo

To look around without setting anything up, run the seeded demo. It fills a fresh database with
sample projects, templates, inventories, and real runs, including a flaky host, a split, and a
pipeline, then serves it read-only so it is safe to expose:

    ./yardmaster demo --addr :8080

Or with Docker: `docker compose --profile demo up --build`.
