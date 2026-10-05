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
every write through a single connection while reads run on a separate read-only pool under WAL, so a
long listing or log read never blocks a claim or a finalize. That is why SQLite fits one process and
PostgreSQL backs a fleet. Run multiple workers against PostgreSQL.

Every process that finishes a run commits that run's outcome to the audit chain, so a run's evidence
does not depend on which worker happened to claim it. That includes the runs nobody is left to finish:
when the sweep interrupts a run whose worker died, it records that outcome too, because a change that
was executing when its process disappeared is exactly the incident somebody asks about afterward. PostgreSQL serializes those appends with a
transaction-level advisory lock, which is what lets a fleet write one chain without forking it.

## Recovery when a worker dies

A running run carries a lease that its holder renews every few seconds. If a worker crashes or loses
the network, the lease goes stale, and a janitor sweeps it after thirty seconds. Work that was still
pending is returned to the queue for another worker. Work that was mid-flight is marked interrupted
so it is not silently lost, and announced like any other end of a run: its outcome reaches the
chain, and its channels and targets hear that it stopped, a pager included. An interrupted run does
not resume from a checkpoint. It is a clean failure a person or a schedule can run again, not a
partial state left holding a lease forever.

A worker that only stalled, a paused virtual machine or a long hang on the network, cannot start a
run the janitor took back from it. The move to running is fenced on the claim itself, the worker's
name and the capability minted when it claimed the run, so a requeued run, or one another worker has
claimed since, refuses the stale start, and the worker walks away without starting its tool.

A worker killed outright stops its tool as well. Every tool runs under a small supervisor, the same
binary started again, that leads the tool's process group and watches a pipe only the worker holds
open. The operating system closes that pipe the moment the worker dies, however it dies, and on
Linux the kernel's parent-death signal says so too. The supervisor then stops the group the way a
cancel does, a terminate signal first and a kill ten seconds later, and for a container run it also
stops and removes the container by name, since the container runs under its daemon rather than
under the worker. The play does not carry on changing hosts after the run reads as interrupted. The
limits:

- This holds on Linux, macOS, and the other Unix platforms. On Windows a tool started by a worker
  that is killed outright keeps running.
- The supervisor ends the tool's process group. A process the tool moved out of that group, such as
  one started with setsid, is outside its reach, as it is outside a cancel's.
- A supervisor killed on its own, rather than with its worker, leaves its tool running, the same as
  killing a tool's top process always did.

The keys and tokens a dead worker had staged on its host are removed by the run-files sweep every
server and worker on that host runs, and by systemd when the worker ran under the shipped unit. A
worker that is only partitioned keeps its files, because its lock still says it is alive.
[Run files](run-files.md) has the rule.

Every server and worker on a host shares one project cache. Each run executes from a private copy of
its project that its process locks for as long as the run lasts, and a process starting up removes
only the copies whose process is gone, so restarting one replica or worker never deletes the
checkout a live run on another is executing from. Updates to a project's shared checkout take a lock
file in the cache, so two processes never clone or reset the same checkout at once.

## What is stopping work

The overview's Needs attention panel answers, for every run that is not moving, what is stopping it.
It sorts the waiting work into four counts, each a list a click away:

- **Worker lost.** The worker running it stopped renewing its lease. The panel names the worker,
  says how long ago it last reported, and counts down to the automatic reclaim the sweep above makes
  once the lease expires. It alerts only when that reclaim has not happened within two lease
  periods, which means no server is sweeping.
- **No worker available.** It is queued and no connected worker serves its queue. The panel names
  the queue, shows zero eligible workers connected, and says it starts automatically when one
  appears. It alerts after 15 minutes.
- **Approval needed.** A run held by a rule, a plan gate's apply among them, or a workflow waiting
  at an approval step, with a badge saying which and, for a step, whether the workflow's other
  branches are still running or it is paused. It says who can approve, how long it has waited, and
  what runs next on approve and on deny. It never alerts unless an age alert is turned on.
- **Blocked.** It has waited past a threshold, 15 minutes by default, behind other work: every
  worker serving its queue is running as many runs as it takes, or a schedule is skipping its fires
  while a run it started is still going. It names the run holding it and alerts after 15 minutes.

A connected worker is one that has reported within the last half minute. Every server and database
worker reports on its own timer, so a worker with every slot busy is never mistaken for a missing
one, and the control node reports each relay worker it hears from, claims and lease renewals alike.

Work in several conditions at once shows the most urgent as its main blocker, in the order above,
the rest as badges, and one line on how they interact. Every timer measures the time in the current
blocker: a run approved after a long hold has waited for a worker only since the approval. A retry,
a dependency, or a run holding a slot is given as the reason on an item rather than counted, and a
schedule fire skipped because its inventory matched no hosts stays in the schedule's history rather
than here. The thresholds are organization defaults with per-queue and per-template overrides, set
with [`--attention-file`](configuration.md#attention-thresholds).

There is deliberately no button that puts a run back in the queue by hand. A worker that stopped
reporting may only be cut off from the store rather than gone, and a run requeued out from under it
could execute a second time on the same hosts. The lease sweep's reclaim is the safe version of that
move, because it waits for the lease to expire first, and the panel shows exactly when it will.

## High availability

High availability is a Team feature. On PostgreSQL the control plane runs active-active. Start two or more `serve` processes against the
same database, put any load balancer in front, and every replica serves the full API and UI while
all of them execute work. There is no leader to elect and no coordinator to stand up, because every
cross-process decision already happens in the store:

- A pending run is claimed with `FOR UPDATE SKIP LOCKED`, so two replicas never take the same run.
- A due schedule is claimed with a compare-and-set on its next fire time, so two schedulers ticking
  the same cron entry fire it exactly once.
- Approve and reject are compare-and-set state transitions, so two admins on two replicas cannot
  release the same held run twice. A decision claims its run before anything records it, so the
  one that loses, to a second admin, a cancel, or a timeout, is refused with nothing written, and
  the audit chain and every receipt, register, and decision list name only the decision that took
  effect. A decision whose process dies after its claim is recorded and applied by the next
  janitor on any replica.
- The audit chain appends under a transaction-level advisory lock with a unique sequence index
  behind it, so the chain stays linear across replicas.
- Live run pages poll the shared store, so a browser on one replica watches a run executing on
  another, painted as it happens.
- Sessions and tokens live in the store, so a sign-in on one replica works on all of them.

When a replica dies, its leases go stale and any survivor's janitor requeues the work, which the
integration suite proves with two replicas on one PostgreSQL: shared claiming with no double-claim,
a single fire for a schedule two replicas race for, and a dead replica's run finished by the
survivor. Kill a replica mid-run and the run fails clean: it is marked interrupted, a terminal state ready for an explicit rerun, never silently re-executed. Only work still leased and unstarted requeues automatically.

To run it, point every replica at the same PostgreSQL with `SWITCHTENDER_DB`, or `--db`, which puts
the password on the command line. Give them all the same `SWITCHTENDER_ENCRYPTION_KEY` and
`SWITCHTENDER_ENCRYPTION_SALT`, so sealed credentials decrypt everywhere, and the same
`SWITCHTENDER_AUDIT_KEY`, since replicas sharing a database sign as one install and none of them
mints the key. Health-check `/readyz` at the balancer: it reads the store and fails when the
database is out of reach, while `/healthz` answers as long as the process is up and would keep a
replica that cannot reach the database in rotation. SQLite has no server to share, so it stays a
single-node deployment by design, and PostgreSQL is the HA backend.

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

A workflow that reaches an approval step and has nothing else to do parks: it releases its lease and
waits in the store, so a restart while it waits loses nothing. Whichever replica records the decision
resumes it from the stored step records with one compare-and-set, so the decision is acted on
exactly once, and every replica's janitor times out an expired step and resumes a decided workflow a
crash left parked. A parked workflow is stored under a status of its own, so an earlier release
running against the same database, during a rolling update or after a rollback, neither approves it
as a run held before it started nor sweeps it, and it waits for this release. The [concepts
page](concepts.md) describes approval steps in full.

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
which `GET /v1/audit/verify` detects. The chain is tamper-evident with no key configured. `GET /v1/audit/bundle`
seals the chain into a signed LoomSeal bundle, and the open `loomseal` verifier confirms the trail
offline, without trusting the server that produced it. A chain that does not verify is refused
rather than sealed: the endpoint answers 409 naming what failed and where, so a broken chain is
never signed over. `switchtender audit report`, a Team feature, renders the
period's changes as a self-contained HTML evidence report a compliance or vendor-security reviewer
reads without any tooling, and the bundle proves the chain behind it independently. The append is serialized by an in-process
lock on SQLite and a transaction-level advisory
lock on PostgreSQL, with
a unique index on the sequence number as a cross-process backstop, so the chain stays linear even
under concurrent writers.

## Idempotency and delivery

On PostgreSQL each pending run is claimed exactly once across the fleet. The scheduler uses that same
claim, so two servers running the same cron entry never double-fire.

A submission is deduplicated when it carries a key. `POST /v1/runs` and `POST /v1/pipelines` take an
`Idempotency-Key` header, kept per organization, so a client that retries a submit it already sent
is answered with the run the first one made rather than a second run. Without the header each call
creates a new run with a fresh identifier, so a client that cannot send one should treat a submit as
create-once on its side. The server keys what it launches itself the same way: a rerun, a webhook
delivery, and a provisioning callback are deduplicated on what they carry, and each scheduled fire
carries a key derived from its schedule and occurrence, `run.ScheduleKey`, in the server's reserved
`st:` namespace, which no caller's key can spell. A fire that takes up an occurrence an interrupted
fire handed back therefore finds the run that one made instead of starting a second. An approval is
a compare-and-set state transition, so two concurrent approvals release a held run exactly once and
the loser gets a clear conflict.

Stored events are ordered and written once per batch. A batch replayed after a transient error
appends rather than deduplicates, so a consumer keys on the event sequence number.

Notifications, on all eleven server-wide channels from webhook and Slack to PagerDuty, Twilio SMS,
and email, and on any per-template targets, are best effort. Each is attempted at least once with one
retry, then logged and dropped, and the run's extra vars are stripped from the payload so survey and
template values never leave the system. Notifications do not block a run from finishing, and shutdown
waits for the deliveries already in flight. Treat a notification as a signal, and the store as the
source of truth.

## How this compares

SwitchTender coordinates a fleet with database leasing, so the same one binary adds capacity without a
separate mesh to stand up. It splits by measured duration rather than round-robin, so a run finishes
on the slowest shard and not the unluckiest one. It retries only the shards that failed. It reads a
run as a host-by-task matrix instead of a log stream. And it proves its own audit trail offline. The
[comparison](comparison.md) lays the three tools side by side, ahead, even, and behind.
