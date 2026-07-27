# Benchmarks

Measured numbers for the questions people actually ask: how fast does it start, how much memory does
it hold at idle, and how big is the binary. Every number below was measured on a release-flag build,
the method is shown so you can reproduce it on your own hardware, and nothing here is a projection.

Measured on 2026-07-27 against v1.24.2 on an Apple Silicon laptop, natively and inside a Debian
container on the same machine. Your hardware will differ; the method will not.

## The numbers

| Measurement | Apple Silicon, native | Debian container, same machine |
|-------------|-----------------------|--------------------------------|
| Cold boot to serving, no encryption key | ~37 ms | ~110 ms |
| Cold boot to serving, credential encryption on | ~75 ms | ~140 ms |
| Resident memory at idle | 32 to 38 MB | 32 to 38 MB |
| Stripped release binary | 29 MB | 29 MB |

Boot times are the median of five trials, first request to a live `/healthz`. The spread between the
two boot rows is the credential key derivation: argon2id at 64 MiB memory cost, a deliberate security
parameter, paid once at startup. The derivation arena is handed back to the operating system as soon
as the key exists, so it does not stay in resident memory.

## Reproduce it

Build with the release flags:

    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o switchtender .
    ls -l switchtender

Time a cold boot to first served request:

    START=$(date +%s%N)
    ./switchtender serve --db bench.db --addr 127.0.0.1:8080 &
    until curl -sf http://127.0.0.1:8080/healthz >/dev/null 2>&1; do :; done
    echo "$(( ($(date +%s%N) - START) / 1000000 ))ms"

Measure idle memory after a few seconds:

    sleep 5
    ps -o rss= -p $(pgrep -f "switchtender serve") | awk '{printf "%.0f MB\n", $1/1024}'

Run each a handful of times and take the median. Set `SWITCHTENDER_ENCRYPTION_KEY` and
`SWITCHTENDER_ENCRYPTION_SALT` to measure the with-encryption boot path.

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
