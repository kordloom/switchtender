# Yardmaster

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

## Quick start

    go build -o yardmaster .
    ./yardmaster serve --addr :8080 --db yardmaster.db

Open http://localhost:8080 and submit a run:

    curl -X POST localhost:8080/runs \
      -d '{"playbook": "site.yml", "inventory": "hosts.ini"}'

Add `"shards": 4` to split it. Ansible is the only runtime dependency: `ansible-playbook` and
`ansible-inventory` on PATH.

## What it does today

- Runs. Submit over HTTP, execute ansible-playbook, capture both the human log and a structured
  event stream through an embedded callback plugin.
- Live view. Server-Sent Events stream events and log chunks as the run executes; the UI paints
  the matrix in real time.
- Drill-down. Message, stdout, stderr, return code, and diff for any host and task in the run.
- Splits. `shards` on a run partitions the inventory, runs the slices through a bounded worker
  pool, rolls their status up to one parent, and merges every shard's events into one matrix.
  Placement uses each host's average duration over its recent runs, so a slow host stops
  dragging a whole shard; hosts with no history balance by count.
- Shard retry. Retry a finished split and only its failed shards run again, with their exact
  host groups, linked back to the original run.
- Pipelines. Ordered playbook steps under one parent run. Stop on failure by default, continue
  per step when asked, and each step is a full run with its own matrix and history.
- Scheduling. Cron schedules fire runs, splits, or pipelines and every fire keeps full
  structured history.
- Fleet health. Per-host outcomes persist at the end of every run, and the fleet view ranks
  hosts by failures across their recent runs. The event history makes cross-run questions
  cheap to answer.
- Recovery. Cancellation stops a run, a whole split, or a pipeline mid-flight and records it as
  canceled, not failed. A server that dies mid-run marks its orphans interrupted on the next
  start.

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
| POST   | `/schedules`            | Create a cron schedule for a run, split, or pipeline    |
| GET    | `/schedules`            | List schedules                                          |
| GET    | `/schedules/{id}`       | One schedule                                            |
| DELETE | `/schedules/{id}`       | Delete a schedule                                       |
| GET    | `/fleet`                | Hosts ranked by failures over recent runs               |
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
train out, what waits on a siding. Good name for the job this tool does. The metaphor stays under
the hood as component codenames and never leaks into the API, the UI, or anything you operate.

| Codename   | Component                                          |
|------------|----------------------------------------------------|
| Roundhouse | The runner that shells out to ansible-playbook     |
| Dispatcher | The run coordinator and cron scheduler             |
| Brakeman   | Cancellation                                       |
| Yardgoat   | The distributed worker (planned)                   |

## Roadmap

- DAG pipelines with branching, per-step retry, and outputs passed between steps
- Flaky host detection and task duration trends on top of the persisted event history
- Integration tests against real multi-node clusters via kind
- Postgres store backend for multi-instance deployments
- Distributed workers for remote execution

## Status

Pre-alpha, moving fast, APIs change without notice.

## License

Apache-2.0. See `LICENSE`.
