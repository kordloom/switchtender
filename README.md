<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-train-tracks-dark.png">
    <img src="assets/logo-train-tracks.png" alt="Yardmaster" width="240">
  </picture>
</p>

<h1 align="center">Yardmaster</h1>

<p align="center">
  <a href="https://github.com/dcadolph/yardmaster/actions/workflows/ci.yml"><img
    src="https://github.com/dcadolph/yardmaster/actions/workflows/ci.yml/badge.svg?branch=main"
    alt="CI status"></a>
  <a href="https://github.com/dcadolph/yardmaster/releases"><img
    src="https://img.shields.io/github/v/release/dcadolph/yardmaster"
    alt="Latest release"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/dcadolph/yardmaster"
    alt="Go version">
  <a href="LICENSE"><img
    src="https://img.shields.io/github/license/dcadolph/yardmaster"
    alt="License"></a>
</p>

Playbook execution and fleet orchestration in one binary. Run Ansible against a fleet, watch every
host and task as a live status matrix instead of a text scroll, split big jobs across parallel
shards, and read the results back as one merged view. No Kubernetes operator, no Postgres, no
Redis, no message bus. One process, one SQLite file.

## Why

AWX wants a Kubernetes operator, Postgres, Redis, and Receptor before it runs a single playbook.
Semaphore is far lighter but still treats a run as a text log. Yardmaster is built on a different
bet: a run is structured data, so store it and show it that way.

- Deploy in seconds. `yardmaster serve` is the API, the executor, the scheduler, and the web UI.
- See structure, not scrollback. Every run is a stream of play, task, and host events rendered as
  a host-by-task matrix with a timeline and drill-down into real per-task output, live while it
  executes.
- Split jobs that work. Shard an inventory, run the shards in parallel, and get one merged matrix
  back. Shards are packed by each host's measured duration in past runs, and a failed split
  retries only the shards that failed.

## See it

A two-shard split executing live: the merged host matrix fills in as hosts report, one shard
fails on the broken database host while the other lands clean, and the timeline draws itself:

<img src="assets/demo-run.gif" alt="A split run executing live in the host matrix" width="100%">

Fleet health after a few runs: failure counts, flaky-host detection, and outcome sparklines per
host, remembered across every run:

<img src="assets/screenshot-fleet.png" alt="Fleet health with flaky detection and sparklines" width="100%">

## Quick start

    go build -o yardmaster .
    ./yardmaster serve --addr :8080 --db yardmaster.db

Open http://localhost:8080 and submit a run:

    curl -X POST localhost:8080/runs \
      -d '{"playbook": "site.yml", "inventory": "hosts.ini"}'

Add `"shards": 4` to split it. Ansible is the only runtime dependency: `ansible-playbook` and
`ansible-inventory` on PATH.

Migrating from AWX or Semaphore is one command. Point it at an export to see what it would create,
then apply:

    ./yardmaster import awx awx-export.json           # dry-run report
    ./yardmaster import awx awx-export.json --apply    # create the objects

Credentials come across as shells; re-enter their secrets, since exports omit them by design.

Full documentation is in [docs/](docs/): quickstart, a configuration reference for every flag and
environment variable, the concepts, a migration guide, and a comparison with AWX and Semaphore. A
`docker-compose.yml` and a Helm chart under `deploy/helm` cover deployment.

## What it does today

| Capability   | What you get                                                                    |
|--------------|---------------------------------------------------------------------------------|
| Runs         | Submit over HTTP, real ansible-playbook underneath, human log plus a structured event stream, per-task drill-down with stdout, stderr, rc, and diff |
| Live view    | Server-Sent Events paint the host matrix and log as the run executes            |
| Splits       | Shard an inventory across parallel slices, merged back into one matrix; hosts packed by measured duration from past runs |
| Shard retry  | Retry a finished split and only its failed shards run again, lineage recorded   |
| Pipelines    | Ordered steps or a dependency graph with parallel branches; failures skip exactly their dependents, per-step retry budgets, set_stats outputs flow to dependent steps as extra vars |
| Workers      | Point `yardmaster worker` at the same database and it competes for queued runs; leases, heartbeats, and a janitor make dead workers safe |
| Scheduling   | Cron schedules fire runs, splits, or pipelines with full history per fire       |
| Fleet memory | Failure rankings, flaky-host detection, outcome sparklines, per-host history, task duration trends, all from persisted structured events |
| Recovery     | Cancellation across processes recorded as canceled, orphaned runs interrupted by lease expiry, terminal saves retried |
| Storage      | SQLite out of the box; the same flag takes a PostgreSQL DSN for multi-instance  |
| Projects     | Playbooks sourced from git with clone-or-fetch sync; every run records the exact commit it executed |
| Templates    | Saved launch presets bundling project, playbook, credentials, shards, and extra vars; one click or one POST launches |
| Auth         | User accounts with admin, operator, and viewer roles enforced per route; bearer tokens hashed at rest; the API locks down the moment the first token exists |
| Observability| A Prometheus metrics endpoint, webhook notifications when runs finish, and an audit trail of every mutation |
| Inventories  | Stored inventories referenced by id, materialized on whichever executor runs the play |
| Dynamic sources | Inventory plugins and scripts refreshed into stored inventories, with cloud auth from an env credential |
| Credentials  | SSH keys, vault passwords, env bundles for cloud SDKs, become passwords, and registry logins, all encrypted at rest |
| High availability | Two servers on one database share the schedule without double-firing; tokens can carry a lifetime |
| Git triggers | A webhook URL launches a template on push; the project syncs fresh, so it deploys the commit just pushed |
| Surveys      | Templates declare typed launch prompts, validated and injected as extra vars |
| Worker queues | Target a run at a named queue; a worker serving that queue runs it and default workers leave it alone |
| Dependency sync | A project's requirements.yml roles and collections install on each sync, so playbooks that need them just run |
| Execution environments | A project can pin a container image; its runs execute inside it with their own ansible and system dependencies |
| Teams and grants | Group users into teams and grant use or manage on a specific project, template, inventory, or credential; grants layer on the global role and default open |
| Retention | A sweeper drops old run events and deletes terminal runs past a configurable age, keeping the summaries the cross-run views need |
| Email | An SMTP notification on every finished run or on failures only, alongside the finish webhooks |
| Migration | `yardmaster import awx` and `import semaphore` read an export and create the equivalent projects, inventories, templates, surveys, schedules, and credential shells, with a dry-run report first |

## HTTP API

| Method | Path                    | What                                                    |
|--------|-------------------------|---------------------------------------------------------|
| POST   | `/runs`                 | Submit a run; `shards` of two or more splits it         |
| GET    | `/runs`                 | Run history, newest first                               |
| GET    | `/runs/{id}`            | One run                                                 |
| POST   | `/runs/{id}/cancel`     | Cancel a pending or running run                         |
| POST   | `/runs/{id}/retry`      | New split from only the failed shards of a finished one |
| GET    | `/runs/{id}/shards`     | Shard runs of a split                                   |
| GET    | `/runs/{id}/steps`      | Step runs of a pipeline                                 |
| GET    | `/runs/{id}/logs`       | Captured output as plain text                           |
| GET    | `/runs/{id}/events`     | Structured events as JSON                               |
| GET    | `/runs/{id}/stream`     | Live events and log over Server-Sent Events             |
| POST   | `/pipelines`            | Submit ordered playbook steps as one pipeline           |
| POST   | `/schedules`            | Cron schedule for a run, split, pipeline, or template   |
| GET    | `/schedules`            | List schedules                                          |
| GET    | `/schedules/{id}`       | One schedule                                            |
| DELETE | `/schedules/{id}`       | Delete a schedule                                       |
| GET    | `/fleet`                | Hosts ranked by failures over recent runs, flaky flags  |
| GET    | `/hosts/{host}/runs`    | One host's recent per-run outcomes                      |
| GET    | `/tasks`                | Per-task duration trends over recent runs               |
| POST   | `/projects`             | Register a git project; runs record their commit       |
| GET    | `/projects`             | List projects                                           |
| DELETE | `/projects/{id}`        | Delete a project                                        |
| POST   | `/templates`            | Save a launch preset                                    |
| GET    | `/templates`            | List templates                                          |
| POST   | `/templates/{id}/launch`| Launch a template, answering its survey if it has one   |
| POST   | `/triggers`             | Create a webhook trigger for a template                 |
| GET    | `/triggers`             | List webhook triggers                                   |
| DELETE | `/triggers/{id}`        | Delete a trigger, revoking its webhook                  |
| POST   | `/hooks/{token}`        | Fire a trigger from a git push, no auth header needed   |
| DELETE | `/templates/{id}`       | Delete a template                                       |
| POST   | `/credentials`          | Store a credential (ssh_key, vault_password, env, become_password, registry), encrypted at rest |
| GET    | `/credentials`          | List credentials, secrets never included                |
| DELETE | `/credentials/{id}`     | Delete a credential                                     |
| POST   | `/auth/login`           | Sign in with username and password, returns a token     |
| POST   | `/auth/check`           | Verify an API token                                     |
| POST   | `/users`                | Create an account with a role                           |
| GET    | `/users`                | List accounts                                           |
| DELETE | `/users/{id}`           | Delete an account; its tokens stop working              |
| POST   | `/teams`                | Create a team of users                                  |
| GET    | `/teams`                | List teams                                              |
| DELETE | `/teams/{id}`           | Delete a team and its memberships                       |
| POST   | `/teams/{id}/members`   | Add a user to a team                                    |
| GET    | `/teams/{id}/members`   | List a team's members                                   |
| DELETE | `/teams/{id}/members/{userID}` | Remove a user from a team                        |
| POST   | `/grants`               | Grant a user or team use or manage on an object         |
| GET    | `/grants`               | List access grants                                      |
| DELETE | `/grants/{id}`          | Delete an access grant                                  |
| GET    | `/workers`              | The executor fleet with lease freshness                 |
| POST   | `/inventory-sources`    | Register a dynamic inventory source                     |
| GET    | `/inventory-sources`    | List inventory sources                                  |
| POST   | `/inventory-sources/{id}/refresh` | Refresh a source into its inventory now       |
| DELETE | `/inventory-sources/{id}` | Delete an inventory source                            |
| POST   | `/inventories`          | Store an inventory; runs reference it by id anywhere    |
| GET    | `/inventories`          | List stored inventories                                 |
| DELETE | `/inventories/{id}`     | Delete a stored inventory                               |
| GET    | `/audit`                | The mutation trail, admin only                          |
| GET    | `/metrics`              | Prometheus gauges for runs and fleet health             |
| GET    | `/healthz`              | Liveness                                                |

The web UI lives at `/ui/` and the root redirects to it.

## Design

A single-binary monolith on purpose. The serve command hosts the HTTP API, an in-process executor
with a bounded worker pool, the cron scheduler, and the embedded UI.

- Storage is SQLite through a pure Go driver in WAL mode. No cgo, one file to back up, and the
  store sits behind an interface a Postgres backend can satisfy later for multi-instance
  deployments.
- Structured events come from an embedded Ansible callback plugin that writes one JSON object per
  event to a sidecar file. The dispatcher tails the sidecar as the run executes, storing and
  publishing events without touching the human-readable log.
- Every run executes under its own cancel context. Canceling a split parent stops all of its
  shards; canceling a pipeline stops the current step and halts the sequence.

## The name

A yardmaster runs a rail yard: which engine takes which track, what gets coupled into the next
train out, what waits on a siding. Good name for the job this tool does. A few internal packages
carry yard codenames, the roundhouse runs the playbooks and the dispatcher coordinates them, but
the API, the UI, and everything you operate speak plain Ansible. No glossary required.

## Roadmap

- Integration tests against real multi-node clusters via kind
- Postgres store backend for multi-instance deployments
- Distributed workers for remote execution

## Status

Pre-alpha, moving fast, APIs change without notice.

## License

Apache-2.0. See `LICENSE`.
