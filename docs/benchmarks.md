# Benchmarks

Measured numbers for the questions people actually ask: how fast it starts, how much memory it holds
at idle, how big the binary is, and how big the container image is. Every figure on this page was
measured on 2026-09-08 against the v1.76.0 tag, except where a line carries its own date. Figures
that vary from run to run say how many trials they came from and how far apart those trials fell.
Nothing here is a projection, and no measured cell is arithmetic on another cell. Where the page
does divide or add published figures, for a ratio or a total, it says which figures it used.

The machine is a ten-core Apple Silicon laptop. Boot times are timed in process on macOS. The memory
and container figures come from Docker Desktop's Linux virtual machine on the same laptop, five vCPU
and 8 GiB, because Linux is where servers run. Sizes are mebibytes throughout, computed from exact
byte counts rather than from the decimal megabytes `docker image ls` prints. Your hardware will
differ; the method will not.

## Boot and memory

| Measurement | Median | Spread | Trials |
|-------------|--------|--------|--------|
| Cold boot to serving, no encryption key | 30 ms | 27-48 ms | 15 |
| Cold boot to serving, credential encryption on | 83 ms | 73-201 ms | 15 |
| Resident memory at idle, Linux, no encryption key | 35.1 MiB | 34.9-35.1 MiB | 5 |
| Resident memory at idle, Linux, credential encryption on | 34.6 MiB | 34.5-35.6 MiB | 5 |

Boot is timed in process, from launch to a served `/healthz`, so no shell or subprocess overhead is
counted. The harness runs a warm-up and five timed trials per path, and it was run three times. The
median column is the middle of those three five-trial medians, and the spread column is the lowest
and the highest single trial across all fifteen. The three medians were 29, 30 and 30 ms without
encryption, and 73, 83 and 88 ms with it.

On the encryption path the spread matters more than the midpoint. Earlier versions of this table
published one number from a smaller sample, most recently 73 ms taken from two end to end runs that
returned 74 ms and 72 ms. Three runs put the tail at 201 ms, which two runs never saw. Read the
range, not one reading.

Memory is `VmRSS` read from `/proc` inside the container, three seconds after the server began
serving, on five freshly started containers per path. Turning encryption on does not raise it,
because the argon2id arena is handed back to the operating system as soon as the key exists.

The gap between the two boot rows is credential key derivation. Timed on its own, with the
parameters the code ships (argon2id, three passes, 64 MiB memory cost, four lanes), the derivation
takes 38 ms on this laptop. Two runs of fifteen timed derivations gave medians of 37.7 and 38.5 ms,
with individual trials from 35 to 43 ms apart from one 109 ms outlier. Subtracting one boot row from
the other would instead put the gap at 53 ms, but that difference also absorbs arena allocation and
run-to-run noise, so the directly timed figure is the one published here and the subtraction is not.
The cost is CPU-bound, which is why it stays small on a laptop and grows on one slow shared vCPU. It
buys a key that is expensive to attack offline, and it is paid once per process, not per run. If you
store no credentials in SwitchTender, leave the encryption pair unset and you never pay it.

On macOS the same idle measurement reads as high as 101 MiB with encryption on, because `ps` there
keeps reclaimable pages in the resident count even after the memory has been released. The Linux
figures above are the ones to plan against.

## Binary size

A stripped release build from the v1.76.0 tag, with the flags the release uses:

    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/kordloom/switchtender/cmd.Version=1.76.0"

| Platform | Size | Bytes |
|----------|------|-------|
| linux/arm64 | 30.9 MiB | 32,374,946 |
| darwin/arm64 | 31.9 MiB | 33,450,818 |
| linux/amd64 | 32.9 MiB | 34,521,250 |

That single file is the whole control plane: server, workers, UI, importers, and the CLI. The build
is reproducible. Built twice from the tag with the same toolchain, the darwin/arm64 binary came out
byte for byte identical, SHA-256 `6975b605fb3ee42e...`.

Growth over time is the figure worth watching, and it is small. Building four tags today with one
toolchain and one set of flags, the darwin/arm64 binary measures 29.7 MiB at v1.30.0, 30.2 MiB at
v1.53.0, 30.7 MiB at v1.62.0 and 31.9 MiB at v1.76.0. That is 2.2 MiB across the eighty-seven
releases after v1.30.0, which included a security-hardening pass, custom credential types, and new
governance features.

## Reproduce it

The repository carries the harness the two boot rows came from:

    go run ./cmd/bench

It builds a release binary, runs a warm-up and five timed trials for both boot paths, and prints the
boot rows with their medians and ranges alongside the size of the binary it just built for your
platform. Its memory readings come from `ps` on the host, so on macOS they read high for the reason
given above. The Linux memory rows in the table come from `/proc` inside a container instead, and
the per-platform sizes come from the release build shown under Binary size. Run it on your own
hardware and you should get your machine's version of the boot rows rather than ours.

## Pending re-measurement

An earlier round, measured on 2026-07-29 against v1.30.0, put a $6/month cloud VM with one shared
vCPU at 189 ms cold boot without encryption and 937 ms with it, showing that argon2id dominates
startup on a slow core. Those figures are eighty-seven releases old and are not re-derived here, so
treat them as the shape of the thing rather than as current numbers. This row is due a re-run.

## Head to head

Numbers we quote from a competitor's own marketing are not a benchmark. Every measured cell in the
table below was taken here, on 2026-09-08, on the machine described at the top of the page. The
cells that carry no number say why below rather than guessing.

The version of this table that stood before mixed its dates: a freshly measured SwitchTender column
beside competitor figures from 2026-08-10. Freezing the whole table at 2026-08-10 was the other
option, and it was not available honestly, because the SwitchTender image size published for that
date was wrong on that date too and no correct reading exists for it. So every cell was re-measured
instead. Semaphore is a single container and re-measures cleanly at its current release. AWX could
not be re-timed here, because it needs Kubernetes and this machine does not have a cluster, but
`docker pull` on 2026-09-08 reports that the 24.6.1 tag still resolves to the same digest that was
weighed before, so its size is a current reading of the same artifact rather than a stale one.

Method for the sizes: each image's exact byte count from `docker image inspect`, divided by 1024
twice. The SwitchTender image is built from this repository's Dockerfile at the v1.76.0 tag. Method
for resident memory: the server process's `VmRSS`, three seconds after it began serving, on five
freshly started containers.

| Measurement | SwitchTender v1.76.0 | Semaphore v2.19.12 | AWX 24.6.1 |
|-------------|----------------------|--------------------|------------|
| Container image | 110.5 MiB | 939.2 MiB | 950.3 MiB |
| Processes to run it | one binary, or one container | one container | two container roles, plus a database and a cache |
| External services required | none; SQLite is embedded | none in this mode | Kubernetes, PostgreSQL, Redis, and a Receptor mesh |
| Cold boot to serving | not published, see below | not published, see below | not a single number, see below |
| Idle resident memory | 35.1 MiB | 43.6 MiB | not measured here |

**The container image was published on this page as 40 MiB. That was wrong, and it was wrong on the
dates it was published, not merely stale.** Built from this repository's Dockerfile, the image
measures 110.5 MiB at the v1.76.0 tag and 109.8 MiB rebuilt from the v1.62.0 tag. No release this
page has covered ever produced an image of 40 MiB. The layers say where the weight goes: 71.5 MiB is
the Alpine runtime layer carrying `ansible-core`, OpenSSH and CA certificates, 30.9 MiB is the
SwitchTender binary, and 8.1 MiB is the Alpine base. The static binary is the small part. The
Ansible runtime next to it is not.

**The container cold-boot row is withdrawn rather than restated.** By the method that row described,
`docker start` to the first successful health response, ten timings on this machine ran from 401 ms
to 3.5 seconds, and the `docker start` client call alone accounted for 355 ms to 2.6 seconds of
that, before the container's first instruction runs. The 24 ms reading that once stood here is not
reachable by that method, and the 45 ms Semaphore figure beside it was taken the same way, so
neither is republished. The in-process boot table at the top of this page comes from a harness in
this repository, reproduces on demand, and stands.

**Semaphore is a genuinely light one-binary competitor.** Measured in its single-container mode with
the embedded SQLite database its own entrypoint supports, its image measures 939.2 MiB where ours
measures 110.5 MiB, a ratio of about 8.5 to 1, and it holds a higher resident set at 43.6 MiB
against our 35.1 MiB. Size and memory are what this page can speak to, and they are not a feature
comparison: Semaphore's own image describes it as covering Terraform, OpenTofu, Terragrunt and
PowerShell alongside Ansible. What each product does and does not carry is the [comparison
page](comparison.md), including where SwitchTender is still young. We publish no boot comparison
against it, for the reason above.

**AWX is not shaped for a boot-time row, and pretending otherwise would be the dishonest thing.** It
has no single-process mode. Its own image ships two supervisor configurations: the web set runs
nginx, uwsgi, daphne and two helpers, and the task set runs the dispatcher, the websocket relay and
the callback receiver. Its default settings point the message broker, the cache and the websocket
channel layer at Redis, and its dispatcher listens on PostgreSQL `pg_notify`, so a database and a
cache are not optional extras. The same defaults schedule a receptor work-unit reaper every sixty
seconds and set a receptor service advertisement period, so a Receptor mesh stands alongside them;
the `receptor` binary is not in this image, so whatever carries it is not weighed below. The AWX
Operator reconciles that set of pods and pulls a separate execution-environment image before it
runs a playbook, so "time to serving" there is a cluster reconciling over minutes, not a process
starting in milliseconds. What compares is the standing cost. Weighed on 2026-09-08, the
application image at 950.3 MiB, `postgres:15` at 445.8 MiB and `redis:7` at 129.3 MiB come to
1,599,467,678 bytes, about 1.5 GiB of images across the four long-lived containers those three
images produce, on top of a Kubernetes cluster and before the Receptor mesh, before the first job
runs. That total is a floor, not a ceiling. Earlier versions of this page put it at 1.6 GiB, which
was the decimal-gigabyte figure labeled as gibibytes. The architecture difference is what the whole
product is built around, and this is it in units that compare.

Run these yourself and get materially different numbers, and open an issue with your hardware and
method; we will look, and we will correct the table if it is wrong.
