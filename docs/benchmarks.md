# Benchmarks

Measured numbers for the questions people actually ask: how fast does it start, how much memory does
it hold at idle, and how big is the binary. Every number below was measured on a release-flag build,
the method is shown so you can reproduce it on your own hardware, and nothing here is a projection.

Re-measured on 2026-08-02 at v1.53.0, on a ten-core Apple Silicon laptop and inside an Alpine Linux
container on the same machine. Memory and binary sizes are mebibytes, the unit `ps` and `/proc`
report. Your hardware will differ; the method will not.

The point of re-measuring: between v1.33.0 and v1.53.0 the product gained twenty releases, a full
security-hardening pass, and custom credential types, and its footprint did not move. That is the
number worth watching over time, not any single reading.

## Boot and memory

| Measurement | Value |
|-------------|-------|
| Cold boot to serving, no encryption key | 42 ms |
| Cold boot to serving, credential encryption on | 85 ms |
| Resident memory at idle, Linux, credential encryption on | 33 MiB |
| Stripped binary, arm64 | 29 MiB |

Boot times are the median of six trials after a warm-up run, timed in process from launch to a served
`/healthz`, so no shell or subprocess overhead is counted. Memory is resident size three seconds after serving begins, read from
`/proc` inside the container, because Linux is where servers actually run.

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
| linux/arm64 | 28.8 MiB |
| darwin/arm64 | 29.8 MiB |
| linux/amd64 | 30.7 MiB |

That single file is the whole control plane: server, workers, UI, importers, and the CLI.

## Reproduce it

The repository carries the harness the boot and memory rows came from, and the 2026-08-02 figures
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

## About comparisons

We publish only numbers we measured ourselves, so there are no competitor columns above. What can be
said factually is that SwitchTender boots from one binary and one file, while the AWX-lineage
controllers stand up a Kubernetes cluster, an operator, PostgreSQL, Redis, and a mesh before they run
a playbook. A boot-time comparison is not even shaped the same, and that is the point. The [comparison page](comparison.md) covers the feature-by-feature picture, including
where SwitchTender is still young. If you run these benchmarks and get materially different numbers,
open an issue with your hardware and method and we will look.
