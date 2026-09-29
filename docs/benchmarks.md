# Benchmarks

Measured numbers for the questions people actually ask: how fast does it start, how much memory does
it hold at idle, and how big is the binary. Every number below was measured on a release-flag build,
the method is shown so you can reproduce it on your own hardware, and nothing here is a projection.

The boot, memory, and binary figures here were re-measured on 2026-08-18 against v1.66.1, on a ten-core
Apple Silicon laptop and inside an Alpine Linux container on the same machine. The container head to
head further down is a 2026-08-10 snapshot; its competitor images were re-confirmed unchanged on
2026-08-18. Memory and binary sizes are mebibytes, the unit `ps` and `/proc` report. Your hardware will
differ; the method will not.

These boot and memory figures were measured on v1.66.1. They hold steady from one release to the
next, which is the point below, so they are not re-timed for every tag. The lower no-encryption boot
comes from UI asset compression moving off the startup path; a build older than that change comes up
slower.

The point of re-measuring: across dozens of releases, a full security-hardening pass, custom credential
types, and new governance features, the binary moved only from about 29 to 31 mebibytes and the idle
memory held flat. That stability is the number worth watching over time, not any single reading.

## Boot and memory

| Measurement | Value |
|-------------|-------|
| Cold boot to serving, no encryption key | 28 ms |
| Cold boot to serving, credential encryption on | 77 ms |
| Resident memory at idle, Linux | 32 MiB |
| Stripped binary, arm64 | 31.4 MiB |

Boot times are the median of five trials after a warm-up run, timed in process from launch to a
served `/healthz`, so no shell or subprocess overhead is counted. Memory is resident size three
seconds after serving begins, read from `/proc` inside the container, because Linux is where servers
actually run.

The gap between the two boot rows is credential key derivation: argon2id at 64 MiB memory cost, a
deliberate security parameter, paid once at startup. It is CPU-bound, which is why it costs about
35 ms on a laptop and substantially more on one slow shared vCPU. That cost buys a key that is
expensive to attack offline, and it is paid once per process, not per run. If you do not store
credentials in SwitchTender, do not set the encryption pair and you do not pay it at all.

Idle memory does not rise with encryption on because the derivation arena is handed back to the
operating system as soon as the key exists. On macOS the same measurement reads near 99 MiB, because
`ps` there keeps reclaimable pages in the resident count even though the memory has been released.
The Linux figures above are the ones to plan against.

## Binary size

A stripped release build, `go build -trimpath -ldflags "-s -w"`:

| Platform | Size |
|----------|------|
| linux/arm64 | 30.4 MiB |
| darwin/arm64 | 31.4 MiB |
| linux/amd64 | 32.4 MiB |

That single file is the whole control plane: server, workers, UI, importers, and the CLI.

## Reproduce it

The repository carries the harness the boot and memory rows came from, and the 2026-08-18 figures
above are its output:

    go run ./cmd/bench

It builds a release binary, runs the warm-up and five timed trials for both boot paths, and prints
the boot, memory, and binary size rows. Run it on your own hardware and you should get your machine's
version of the table rather than ours.

## Pending re-measurement

An earlier round measured a $6/month cloud VM with one shared vCPU at 189 ms cold boot without
encryption and 937 ms with it, showing that argon2id dominates startup on a slow core. Those figures
came from an earlier release and are not re-derived here, so treat them as indicative of the shape
rather than as current numbers. This row is due a re-run.

## Head to head

Numbers we quote from a competitor's own marketing are not a benchmark. These were measured, on one
machine, on 2026-08-10: SwitchTender at v1.62.0, Ansible Semaphore v2.19.7, and Ansible AWX 24.6.1,
each in its own container on the same Docker Desktop Linux VM (five vCPU, 8 GiB) on an Apple Silicon
laptop. Method for the timed rows: each server's binary is baked into its image, never bind-mounted
from the host, because a mounted binary faults across the virtual-machine file boundary on every cold
start and would flatter or punish a program for where its file sits rather than what it does. Boot is
`docker start` to the first successful health response, the median of five after a warm-up. Resident
memory is the server process's `VmRSS` three seconds after it began serving. Image size is what
`docker image ls` reports.

| Measurement | SwitchTender | Semaphore | AWX |
|-------------|--------------|-----------|-----|
| Container image | 40 MiB | 984 MiB | 996 MiB, plus PostgreSQL 467 MiB and Redis 136 MiB |
| Processes to run it | one binary, or one container | one container | four or more containers under Kubernetes |
| External services required | none; SQLite is embedded | none in this mode | Kubernetes, PostgreSQL, Redis, and a Receptor mesh |
| Cold boot to serving | 24 ms | 45 ms | not a single number; see below |
| Idle resident memory | 32 MiB | 42 MiB | the sum of web, task, database, and Redis |

The boot row is close to the first table's in-process figure: after asset compression moved off the
startup path, the process comes up fast enough that `docker start` overhead is most of what is left,
so the container figure and the in-process figure converge. Both are ours, measured on different
builds (v1.62.0 in this head to head, v1.66.1 in the boot table above) and days, so read the small
gap between 24 ms and 28 ms as build and method, not a contradiction.

**Semaphore is a genuinely light one-binary competitor, and we boot faster than it.** In its
single-container mode with an embedded SQLite database, the configuration measured here, it comes up
in 45 ms against our 24 ms. The two part company further on footprint and what comes in the box: our
image is a twenty-fifth the size, holds a lower resident set, and the single file is the whole control
plane, while a production Semaphore adds an external MySQL or PostgreSQL, and RBAC, enforced approvals,
a provable audit trail, and the extra runtimes are ours rather than theirs. The [comparison
page](comparison.md) has that feature picture, including where SwitchTender is still young.

**AWX is not shaped for a boot-time row, and pretending otherwise would be the dishonest thing.** It
has no single-process mode. The AWX Operator reconciles a set of pods, web and task from the 996 MiB
application image, PostgreSQL, and Redis, and pulls a separate execution-environment image before it
runs a playbook, so "time to serving" is a cluster reconciling over minutes, not a process starting in
milliseconds. What is comparable is the standing cost: roughly 1.6 GiB of images across four or more
long-lived containers, on top of a Kubernetes cluster, before the first job. That is the architecture
difference the whole product is built around, stated in the units that actually compare.

Run these yourself and get materially different numbers, and open an issue with your hardware and
method; we will look, and we will correct the table if it is wrong.
