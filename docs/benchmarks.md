# Benchmarks

Measured numbers for the questions people actually ask: how fast does it start, how much memory does
it hold at idle, and how big is the binary. Every number below was measured on a release-flag build,
the method is shown so you can reproduce it on your own hardware, and nothing here is a projection.

Measured on 2026-07-27 against v1.24.2 on an Apple Silicon laptop, natively and inside a Debian
container on the same machine. Your hardware will differ; the method will not.

## The numbers

Measured on 2026-07-29 against v1.30.0. Two very different machines, because the answer depends
almost entirely on the processor you give it.

| Measurement | Apple Silicon laptop | $6/month cloud VM, 1 vCPU |
|-------------|----------------------|---------------------------|
| Cold boot to serving, no encryption key | 43 ms | 189 ms |
| Cold boot to serving, credential encryption on | 87 ms | 937 ms |
| Resident memory at idle | 39 MB | 38 MB |
| Stripped release binary | 30 MB | 31 MB |

Boot times are the median of five trials after a warm-up run, timed from process start to a served
`/healthz`. Memory is resident size three seconds after serving begins.

The gap between the two boot rows is credential key derivation: argon2id at 64 MiB memory cost, a
deliberate security parameter, paid once at startup. It is CPU-bound, which is why it costs 44 ms on
a laptop and closer to a second on one slow shared vCPU. That cost buys a key that is expensive to
attack offline, and it is paid once per process, not per run. If you do not store credentials in
SwitchTender, do not set the encryption pair and you do not pay it at all.

Idle memory is flat across both machines because the derivation arena is handed back to the
operating system as soon as the key exists.

## Reproduce it

The repository carries the harness this table came from:

    go run ./cmd/bench

It builds a release binary, runs the warm-up and five timed trials for both boot paths, and prints
the same three rows. Run it on your own hardware and you should get your machine's version of the
table rather than ours.

One platform note: on macOS, `ps` keeps reclaimable pages in its resident count, so the
with-encryption idle number can read near 100 MB there even though the memory has been released and
the OS takes it back under any pressure. On Linux, where servers actually run, the resident count
drops immediately and idles in the same 32 to 38 MB band as the no-encryption path.

## What this buys you in practice

- Restarts are free. A control plane that boots in well under a second restarts on upgrade or crash
  before a health check notices. There is no cluster to warm and no operator to reconcile.
- It fits next to your workload. At under 40 MB idle, SwitchTender runs on the same small VM as the
  things it automates, a spare Raspberry Pi, or the corner of a laptop.
- One artifact to ship. The 29 MB binary is the whole control plane: server, workers, UI, importers,
  and the CLI. Copy it, run it.

## About comparisons

We publish only numbers we measured ourselves, so there are no AWX or Semaphore columns above. What
can be said factually: AWX requires a Kubernetes cluster, its operator, PostgreSQL, Redis, and
Receptor before it runs a playbook, so a boot-time comparison is not even shaped the same, and that
is the point. The [comparison page](comparison.md) covers the feature-by-feature picture, including
where SwitchTender is still young. If you run these benchmarks and get materially different numbers,
open an issue with your hardware and method and we will look.
