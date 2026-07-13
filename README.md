<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-train-tracks-dark.png">
    <img src="assets/logo-train-tracks.png" alt="Yardmaster" width="240">
  </picture>
</p>

<h1 align="center">Yardmaster</h1>

<p align="center">
  <a href="https://yardmaster.dev"><img
    src="https://img.shields.io/badge/website-yardmaster.dev-0969da"
    alt="Website"></a>
  <a href="https://github.com/dcadolph/yardmaster/actions/workflows/ci.yml"><img
    src="https://github.com/dcadolph/yardmaster/actions/workflows/ci.yml/badge.svg?branch=main"
    alt="CI status"></a>
  <a href="https://github.com/dcadolph/yardmaster/releases"><img
    src="https://img.shields.io/github/v/release/dcadolph/yardmaster"
    alt="Latest release"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/dcadolph/yardmaster"
    alt="Go version">
  <a href="LICENSE"><img
    src="https://img.shields.io/badge/license-BSL%201.1-blue"
    alt="License"></a>
</p>

Run everything. Watch every host. Prove every change. One Go binary runs Ansible, Terraform, OpenTofu,
Bash, PowerShell, Python, and Go across your fleet, paints every run live as a host-by-task matrix instead of a text
scroll, splits big jobs across parallel shards, and proves exactly what ran and who approved it.
No Kubernetes operator, no Postgres, no Redis, no message bus. One process, one SQLite file.

## Contents

- [Why](#why)
- [See it](#see-it)
- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Documentation](#documentation)
- [Design](#design)
- [The name](#the-name)
- [Roadmap](#roadmap)
- [Status](#status)
- [License](#license)

## Why

AWX makes you stand up Kubernetes, Postgres, Redis, and Receptor before it runs a single playbook,
and it has not shipped a release in over a year. Semaphore is lighter, but a run is still a text
log. Yardmaster runs the same playbooks from one binary and treats every run as structured data you
can read, split, and remember.

|                                      | Yardmaster                                                                        | AWX                                             | Semaphore                |
|--------------------------------------|-----------------------------------------------------------------------------------|-------------------------------------------------|--------------------------|
| Deploy                               | One binary and one SQLite file, running in seconds.                               | A Kubernetes operator, Postgres, Redis, and Receptor first. | One binary.              |
| Every&nbsp;run                       | A live host-by-task matrix you read like a dashboard, with per-task drill-down.   | A text log you scroll.                          | A text log you scroll.   |
| Big&nbsp;jobs                        | Sharded across hosts, balanced by their measured duration, only failed shards retried. | Sliced round-robin, with no balancing.     | No splitting at all.     |
| Memory&nbsp;across&nbsp;runs         | Flaky hosts flagged, durations trended, every host's history kept.               | Forgotten the moment a run ends.                | Forgotten the moment a run ends. |
| Pipelines                            | A dependency graph with a drag-and-drop editor, passing typed outputs from one step to the next. | A visual workflow builder.       | Basic chaining.          |
| Leaving&nbsp;your&nbsp;old&nbsp;tool | One command imports your AWX or Semaphore projects, inventories, templates, surveys, and schedules. | Not applicable.                     | Not applicable.          |

The full head-to-head, including where Yardmaster is behind, is in the
[comparison](docs/comparison.md).

## See it

A two-shard split executing live: the merged host matrix fills in as hosts report, one shard
fails on the broken database host while the other lands clean, and the timeline draws itself:

<img src="assets/demo-run.gif" alt="A split run executing live in the host matrix" width="100%">

Fleet health after a few runs: failure counts, flaky-host detection, and outcome sparklines per
host, remembered across every run:

<img src="assets/screenshot-fleet.png" alt="Fleet health with flaky detection and sparklines" width="100%">

## Requirements

- Ansible on the PATH: `ansible-playbook` and `ansible-inventory`.
- Go 1.26 to build from source, or the container image and Helm chart to deploy.
- Nothing else for the default SQLite setup. PostgreSQL is optional, for running more than one
  instance.

## Quick start

Grab a build for your platform from the [releases page](https://github.com/dcadolph/yardmaster/releases):
a `Yardmaster.dmg` for macOS, a `windows_amd64.zip` for Windows, or a `tar.gz` of the binary for
macOS and Linux. Or build from source:

    go build -o yardmaster .
    ./yardmaster serve --addr :8080 --db yardmaster.db

Or run it as a local desktop app. On macOS open `Yardmaster.app`; otherwise one command picks a
stable loopback port, keeps its data in a per-user directory, and opens the UI in your browser:

    ./yardmaster desktop

Open http://localhost:8080 and submit a run:

    curl -X POST localhost:8080/runs \
      -d '{"playbook": "site.yml", "inventory": "hosts.ini"}'

Add `"shards": 4` to split it across four slices of the inventory.

Migrating from AWX or Semaphore is one command. Point it at an export to see what it would create,
then apply:

    ./yardmaster import awx awx-export.json           # dry-run report
    ./yardmaster import awx awx-export.json --apply    # create the objects

Credentials come across as shells. Re-enter their secrets, since exports omit them by design. The
[switching-from-AWX guide](docs/switching-from-awx.md) walks the whole move.

To see it without any setup, run the seeded read-only demo. It fills a fresh database with sample
projects, templates, and real runs, then serves it with every change blocked:

    ./yardmaster demo --addr :8080         # or: docker compose --profile demo up --build

## Documentation

The docs live in [docs/](docs/) and also render inside the app at `/ui/docs`.

| Guide | What |
|-------|------|
| [Quickstart](docs/quickstart.md) | Zero to a first run in a few minutes |
| [Switching from AWX](docs/switching-from-awx.md) | Import what you have, or set up from scratch |
| [Concepts](docs/concepts.md) | Runs, splits, pipelines, projects, templates, and the rest |
| [Configuration](docs/configuration.md) | Every command, flag, and environment variable |
| [Desktop](docs/desktop.md) | Run Yardmaster as a local desktop app |
| [Features](docs/features.md) | The full capability list |
| [HTTP API](docs/api.md) | Every endpoint the server exposes |
| [Migration](docs/migration.md) | Moving off AWX or Semaphore in detail |
| [Comparison](docs/comparison.md) | How Yardmaster compares to AWX and Semaphore |

Deploy with the `docker-compose.yml` at the root, which brings up a server, a database, and a
worker, or the Helm chart under [deploy/helm](deploy/helm).

## Design

A single-binary monolith on purpose. The serve command hosts the HTTP API, an in-process executor
with a bounded worker pool, the cron scheduler, and the embedded UI.

- Storage is SQLite through a pure Go driver in WAL mode. No cgo, one file to back up, and the
  store sits behind an interface a Postgres backend can satisfy for multi-instance deployments.
- Structured events come from an embedded Ansible callback plugin that writes one JSON object per
  event to a sidecar file. The dispatcher tails the sidecar as the run executes, storing and
  publishing events without touching the human-readable log.
- Every run executes under its own cancel context. Canceling a split parent stops all of its
  shards. Canceling a pipeline stops the current step and halts the sequence.

## The name

A yardmaster runs a rail yard: which engine takes which track, what gets coupled into the next
train out, what waits on a siding. Good name for the job this tool does. A few internal packages
carry yard codenames, the roundhouse runs the playbooks and the dispatcher coordinates them, but
the API, the UI, and everything you operate speak plain Ansible. No glossary required.

## Roadmap

- A hosted option.
- Signed desktop packages for macOS and Windows.
- Broader notification channels beyond webhook, email, and Slack.

## Status

Version 1.0. Source-available under the Business Source License 1.1. The execution engine, the
control plane, and the one-command AWX and Semaphore migration are complete. The HTTP API is served
under a stable `/v1` base path and follows semantic versioning, so no breaking change lands within
the 1.x line.

## License

Business Source License 1.1. You can read the source, run it, and use it in production. You may not
offer it to others as a hosted or managed service that competes with the maintainers. Each version
converts to Apache-2.0 four years after its release. See `LICENSE` for the exact terms, and open an
issue for commercial licensing.
