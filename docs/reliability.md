<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Reliability

SwitchTender treats a run as durable, attributable work. It runs many at once under a fixed bound,
coordinates failure instead of ignoring it, recovers when a worker dies, and keeps its store
consistent while several workers write to it at the same time. This page states exactly how, and it
is straight about the edges, because a claim you can check is worth more than one you cannot.

## Workers and concurrency

A worker is any process running the executor against the shared store. The `serve` process is one,
and each `worker` process adds another. Every process claims pending runs from the store and runs up
to a fixed number at once, four by default and set with `--workers`. The bound is per process, so a
fleet adds capacity by adding workers, each keeping its own limit rather than a single global pool.

A run can target a named queue, and only a worker serving that queue claims it, which places work
across a mixed fleet. A GPU job waits for a GPU worker, and a default job runs anywhere.

Claiming is exact on PostgreSQL. A worker takes the oldest pending run with `FOR UPDATE SKIP LOCKED`,
so two workers never claim the same run even when they poll at the same instant. SQLite serializes
every read and write through a single connection, which is why SQLite fits one process and PostgreSQL
backs a fleet. Run multiple workers against PostgreSQL.

## Recovery when a worker dies

A running run carries a lease that its holder renews every few seconds. If a worker crashes or loses
the network, the lease goes stale, and a janitor sweeps it after thirty seconds. Work that was still
pending is returned to the queue for another worker. Work that was mid-flight is marked interrupted
so it is not silently lost. An interrupted run does not resume from a checkpoint. It is a clean
failure a person or a schedule can run again, not a partial state left holding a lease forever.

## High availability

On PostgreSQL the control plane runs active-active. Start two or more `serve` processes against the
same database, put any load balancer in front, and every replica serves the full API and UI while
all of them execute work. There is no leader to elect and no coordinator to stand up, because every
cross-process decision already happens in the store:

- A pending run is claimed with `FOR UPDATE SKIP LOCKED`, so two replicas never take the same run.
- A due schedule is claimed with a compare-and-set on its next fire time, so two schedulers ticking
  the same cron entry fire it exactly once.
- Approve and reject are compare-and-set state transitions, so two admins on two replicas cannot
  release the same held run twice.
- The audit chain appends under a transaction-level advisory lock with a unique sequence index
  behind it, so the chain stays linear across replicas.
- Live run pages poll the shared store, so a browser on one replica watches a run executing on
  another, painted as it happens.
- Sessions and tokens live in the store, so a sign-in on one replica works on all of them.

When a replica dies, its leases go stale and any survivor's janitor requeues the work, which the
integration suite proves with two replicas on one PostgreSQL: shared claiming with no double-claim,
a single fire for a schedule two replicas race for, and a dead replica's run finished by the
survivor. Kill a replica mid-run and the run fails clean and requeues, it does not vanish.

To run it: point every replica at the same PostgreSQL with `--db`, share the same
`SWITCHTENDER_ENCRYPTION_KEY` and `SWITCHTENDER_ENCRYPTION_SALT` so sealed credentials decrypt everywhere, and
health-check `/healthz` at the balancer. SQLite has no server to share, so it stays a single-node
deployment by design; PostgreSQL is the HA backend.

## Splits balance real work

A split shards one inventory across parallel slices of the same playbook, each limited to its hosts,
with the parent rolling the slices into one host matrix. Hosts are packed by their measured average
duration over recent runs, heaviest first into the least-loaded shard, so each shard carries a
similar amount of wall-clock work. The packing is deterministic, and a host with no history is given
the average weight so a new host is neither favored nor stranded.

Balancing by measured duration is the point. Round-robin slicing hands each shard the same count of
hosts, so a handful of slow hosts make one shard the long pole and the run waits on it. Packing by
time evens the finish line.

Splitting applies to Ansible, and only when there are at least two shards and at least two hosts to
divide. Below that it runs as one. The parent holds a lease while it waits for every shard to reach a
terminal state, then rolls up the outcome: every shard succeeds and the split succeeds, any shard
fails and the split fails, any cancel and it cancels. Retrying a split re-runs only the shards that
failed, under a new parent that records what it retried, so a hundred-host run that failed on three
hosts costs three hosts to fix, not a hundred.

## Pipelines coordinate failure

A pipeline runs steps in order, or as a dependency graph when steps declare what they depend on.
Each step is itself a run with a full matrix, events, and history.

In order, a failed step stops the pipeline, unless that step is marked to continue on failure, in
which case the next step still runs. In a graph, a step runs once all its dependencies finish and
each of them either succeeded or is allowed to continue on failure. A step blocked by a failed
dependency is skipped, and the skip carries down to everything that depended on it. A skipped step
starts no run, so the history shows what ran and what never got the chance.

A step can retry a set number of times. Each attempt is a fresh child run, so every try keeps its own
log and matrix and none overwrite each other. Retries run back to back with no built-in delay, so a
step that should pause between attempts sets that in its own logic. Values a step publishes with
`set_stats` flow to the steps that depend on it, merged in dependency order, so a later step reads
what an earlier one produced.

## Cancellation reaches the whole tree

Canceling a run signals its process group, not only the top process. On Unix the entire tree the
tool spawned, the ssh connections, plugin processes, and child commands, receives the kill, with a
grace window before the pipes are closed. On Windows the direct process is signaled. Prefer Unix
workers where a clean stop of the whole tree matters.

Cancellation crosses processes. The request is written to the store, and whichever worker holds the
run acts on it within a few seconds through the same watch loop that renews the lease. A pending run
that no worker has claimed is canceled in place. Canceling a split cancels its shards, and canceling
a pipeline stops the running steps and skips the rest.

## Durability and consistency

Writes are atomic. A run is saved as a single upsert. A batch of events, and the per-host and
per-task summaries, are each written inside one transaction, so a reader never sees half a set. When
a run reaches a terminal state, the final save is retried a few times so a brief database contention
does not lose the outcome.

The schema is applied idempotently on both stores, guarded by `IF NOT EXISTS`, so starting a newer
binary against an existing database is safe to repeat.

The audit trail is a SHA-256 hash chain. Every recorded mutation carries the previous entry's hash
and its own hash over its content, so altering, reordering, or dropping an entry breaks the chain,
which `GET /audit/verify` detects. The chain is tamper-evident with no key configured. When a signing
key is set, `GET /audit/export` seals the chain head with an ed25519 signature, and `switchtender audit
verify` confirms the trail offline, without trusting the server that produced it. The append is
serialized by an in-process lock on SQLite and a transaction-level advisory lock on PostgreSQL, with
a unique index on the sequence number as a cross-process backstop, so the chain stays linear even
under concurrent writers.

## Idempotency and delivery

On PostgreSQL each pending run is claimed exactly once across the fleet. The scheduler uses that same
claim, so two servers running the same cron entry never double-fire.

Submitting a run is not idempotent. Each call creates a new run with a fresh identifier, and there is
no request-deduplication key, so a client that retries a submit it already sent creates a second run.
An approval is a compare-and-set state transition, so two concurrent approvals release a held run
exactly once and the loser gets a clear conflict. Treat a submit as create-once on the client side.

Stored events are ordered and written once per batch. A batch replayed after a transient error
appends rather than deduplicates, so a consumer keys on the event sequence number.

Notifications by webhook, email, and Slack are best effort. Each is attempted at least once with one
retry, then logged and dropped, and the run's extra vars are stripped from the payload so survey and
template values never leave the system. Notifications do not block a run from finishing, and shutdown
waits for the deliveries already in flight. Treat a notification as a signal, and the store as the
source of truth.

## Next to AWX and Semaphore

SwitchTender coordinates a fleet with database leasing, so the same one binary adds capacity without a
separate mesh to stand up. It splits by measured duration rather than round-robin, so a run finishes
on the slowest shard and not the unluckiest one. It retries only the shards that failed. It reads a
run as a host-by-task matrix instead of a log stream. And it proves its own audit trail offline. The
[comparison](comparison.md) lays the three tools side by side, ahead, even, and behind.
