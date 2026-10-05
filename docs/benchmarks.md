# Benchmarks

Measured numbers for the questions people actually ask: how fast it starts, how much memory it holds
at idle, how big the binary is, and how big the container image is. Every figure on this page was
measured on 2026-10-05 against commit `5bd9bb5` on main, except where a line carries its own date or
names another version. The release this page ships with adds code on top of that commit, among it
the managed Ansible runtime and the pull request commands, and that build has not been measured
here. Figures that vary from run to run say how many trials they came from and how
far apart those trials fell. Nothing here is a projection, and no measured cell is arithmetic on
another cell. Where the page does divide or add published figures, for a ratio or a total, it says
which figures it used.

The machine is a ten-core Apple Silicon laptop. Boot times are timed in process on macOS. The memory
and container figures come from Docker Desktop's Linux virtual machine on the same laptop, five vCPU
and 8 GiB, because Linux is where servers run. Sizes are mebibytes throughout, computed from exact
byte counts rather than from the decimal megabytes `docker image ls` prints. Your hardware will
differ. The method will not.

## Boot and memory

| Measurement | Median | Spread | Trials |
|-------------|--------|--------|--------|
| Cold boot to serving, no encryption key | 42 ms | 37-139 ms | 15 |
| Cold boot to serving, credential encryption on | 87 ms | 80-112 ms | 15 |
| Resident memory at idle, Linux, no encryption key | 46.6 MiB | 45.7-47.3 MiB | 5 |
| Resident memory at idle, Linux, credential encryption on | 48.7 MiB | 48.0-49.5 MiB | 5 |

Boot is timed by the harness from the moment it starts the server process to a served `/healthz`, so
the server's own start is counted and no shell or helper such as curl is. The harness runs a warm-up
and five timed trials per path, and it was run three times. The median column is the middle of those
three five-trial medians, and the spread column is the lowest and the highest single trial across
all fifteen. The three medians were 48, 42 and 39 ms without encryption, and 87, 82 and 90 ms with
it.

The laptop was not idle that day. Other build and test work shared it, so each of the three runs
began only once the CPU was at least 80 percent idle. That work could still start partway through a
run, and the spread shows where it did. Two sets of three runs taken earlier the same day without
that check, while other work was running, gave medians from 38 to 68 ms without encryption and from
84 to 140 ms with it, and single trials up to 261 ms. They are not in the table. The previous round,
on 2026-09-08 against v1.76.0, put the medians at 30 and 83 ms, with the encryption tail at 201 ms.
Read the range, not one reading.

Memory is `VmRSS` read from `/proc` inside the container, three seconds after the server began
serving, on five freshly started containers per path. The process is this commit's binary, built
with the Dockerfile's flags, in the runtime image weighed under [Head to head](#head-to-head).
Turning encryption on raises the median by 2.1 MiB, 48.7 against 46.6, rather than by the 64 MiB the
key derivation allocates, because the argon2id arena is handed back to the operating system as soon
as the key exists.

The gap between the two boot rows is credential key derivation. Timed on its own, with the
parameters the code ships (argon2id, three passes, 64 MiB memory cost, four lanes), the derivation
takes about 37 ms on this laptop. Two runs of fifteen timed derivations, each begun with the CPU
at least 80 percent idle, gave medians of 36.1 and 37.0 ms, with individual trials from 34.3
to 41.0 ms. Subtracting one boot row from the other would instead put the gap at 45 ms, but
that difference also absorbs arena allocation and run-to-run noise, so the directly timed figure is
the one published here and the subtraction is not. The cost is CPU-bound, which is why it stays
small on a laptop and grows on one slow shared vCPU. It buys a key that is expensive to attack
offline, and it is paid once per process, not per run. If you store no credentials in SwitchTender,
leave the encryption pair unset and you never pay it.

On macOS the same idle measurement reads as high as 113 MiB with encryption on, because `ps`
there keeps reclaimable pages in the resident count even after the memory has been released. The
Linux figures above are the ones to plan against.

## Binary size

A stripped release build of commit `5bd9bb5`, with Go 1.26.6, the version `go.mod` names, and the
flags the release uses:

    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/kordloom/switchtender/cmd.Version=5bd9bb5"

| Platform | Size | Bytes |
|----------|------|-------|
| linux/arm64 | 51.2 MiB | 53,674,146 |
| darwin/arm64 | 52.7 MiB | 55,311,890 |
| linux/amd64 | 54.7 MiB | 57,405,602 |
| windows/amd64 | 56.0 MiB | 58,671,104 |
| darwin/amd64 | 56.0 MiB | 58,676,480 |

The macOS download is one universal file holding both darwin builds. Joined with `lipo`, the two
come to 108.7 MiB, 113,999,378 bytes.

That single file is the whole control plane: server, workers, UI, importers, and the CLI. The build
is reproducible. Built twice from this commit with the same toolchain, the second time rebuilding
every package, the darwin/arm64 binary came out byte for byte identical, SHA-256
`5fa87743299b3373...`. The container build reproduces too. The binary inside the image weighed under
[Head to head](#head-to-head) matches, byte for byte, a build of its commit made on the laptop with
the Dockerfile's flags and Go 1.26.8, the release its build image carries.

Growth over time is the figure worth watching, and this round it is not small. Building four
versions today with one toolchain and one set of flags, the darwin/arm64 binary measures 29.72 MiB
at v1.30.0, 31.90 MiB at v1.76.0, 33.63 MiB at v1.102.0 and 52.75 MiB at `5bd9bb5`. The v1.76.0
build is 33,450,818 bytes, exactly what this page published for it on 2026-09-08. From v1.30.0 to
v1.102.0 the binary grew 3.91 MiB across 122 releases. The step after v1.102.0 is 19.12 MiB, and
most of it is one feature. Rego policies run on the Open Policy Agent engine, which is compiled into
the binary. Built from the same commit with the one file that imports it replaced by a stub, the
darwin/arm64 binary measures 38.98 MiB, so the engine and the packages only it pulls in account for
13.77 MiB of the step. The other 5.35 MiB is everything else that landed after v1.102.0, among it
the wider AWX importer, the native inventory engine, and federated cloud credentials.

## Reproduce it

The repository carries the harness the two boot rows came from:

    go run ./cmd/bench

It builds a binary with the release flags apart from the version stamp, runs a warm-up and five
timed trials for both boot paths, and prints the boot rows with their medians and ranges alongside
the size of the binary it just built for your platform. Its memory readings come from `ps` on the
host, so on macOS they read high for the reason given above. The Linux memory rows in the table come
from `/proc` inside a container instead, and the per-platform sizes come from the release build
shown under Binary size. Run it on your own hardware and you should get your machine's version of
the boot rows rather than ours.

## Pending re-measurement

An earlier round, measured on 2026-07-29 against v1.30.0, put a $6/month cloud VM with one shared
vCPU at 189 ms cold boot without encryption and 937 ms with it, showing that argon2id dominates
startup on a slow core. Those figures are more than 120 releases old and predate the Rego engine, so
treat them as the shape of the thing rather than as current numbers. This row is due a re-run.

## Head to head

Numbers we quote from a competitor's own marketing are not a benchmark. Every measured cell in the
table below was taken here, on the machine described at the top of the page, and each column says
the date it was taken. The SwitchTender column was measured on 2026-10-05. The Semaphore and AWX
columns are from 2026-09-08, a round in which every cell was measured that same day, and they were
not taken again for this one. The cells that carry no number say why below rather than guessing.

On 2026-09-08 Semaphore was measured in a single container at its current release. AWX was weighed
rather than timed, because it needs Kubernetes and this machine does not have a cluster, and
`docker pull` that day reported that its 24.6.1 tag still resolved to the digest weighed on
2026-08-10.

Method for the sizes: each image's exact byte count from `docker image inspect`, divided by 1024
twice. The SwitchTender image is the one built from this repository's Dockerfile on 2026-10-05 at
commit `7fc25bf`, the merge just before `5bd9bb5`, and its binary layer is the same size to the byte
as a container build of `5bd9bb5`. Method for resident memory: the server process's `VmRSS`, three
seconds after it began serving, on five freshly started containers.

| Measurement | SwitchTender, 2026-10-05 | Semaphore v2.19.12, 2026-09-08 | AWX 24.6.1, 2026-09-08 |
|-------------|--------------------------|--------------------------------|------------------------|
| Container image | 293.9 MiB | 939.2 MiB | 950.3 MiB |
| Processes to run it | one binary, or one container | one container | two container roles, plus a database and a cache |
| External services required | none, SQLite is embedded | none in this mode | Kubernetes, PostgreSQL, Redis, and a Receptor mesh |
| Cold boot to serving | not published, see below | not published, see below | not a single number, see below |
| Idle resident memory | 46.6 MiB | 43.6 MiB | not measured here |

**The container image was published on this page as 40 MiB. That was wrong, and it was wrong on the
dates it was published, not merely stale.** On 2026-09-08, built from this repository's Dockerfile,
the image measured 110.5 MiB at the `snapshot/v1.76.0` tag and 109.8 MiB rebuilt from the
`snapshot/v1.62.0` tag. No release this page has covered ever produced an image of 40 MiB.

**It has grown by 183.4 MiB since, and the binary is the smaller part of why.** The image now
carries Terraform and OpenTofu, so a pull runs five of the seven tools with nothing added. The
layers of the 2026-10-05 image say where the weight goes: 159.0 MiB is the layer holding Terraform
and OpenTofu, 75.6 MiB is the Alpine layer carrying `ansible-core`, Python, Bash, OpenSSH, jq and CA
certificates, 51.2 MiB is the SwitchTender binary, and 8.1 MiB is the Alpine base. The growth is the
difference of the two published image figures, 293.9 less 110.5. PowerShell and the Go toolchain
stay operator-provided, as their tool pages say, because between them they would weigh several times
the rest.

**The container cold-boot row is withdrawn rather than restated.** By the method that row described,
`docker start` to the first successful health response, ten timings on this machine on 2026-09-08
ran from 401 ms to 3.5 seconds, and the `docker start` client call alone accounted for 355 ms to 2.6
seconds of that, before the container's first instruction runs. The 24 ms reading that once stood
here is not reachable by that method, and the 45 ms Semaphore figure beside it was taken the same
way, so neither is republished. The in-process boot table at the top of this page comes from a
harness in this repository, reproduces on demand, and stands.

**Semaphore is a genuinely light one-binary competitor.** Measured on 2026-09-08 in its
single-container mode, with the embedded SQLite database its own entrypoint supports, its image
measured 939.2 MiB. Ours measured 293.9 MiB on 2026-10-05, and dividing the first figure by the
second gives a ratio of about 3.2 to 1, down from about 8.5 to 1 against our 110.5 MiB image of
2026-09-08. Its resident set of 43.6 MiB is now lower than our 46.6 MiB. Size and memory are what
this page can speak to, and they are not a feature comparison: Semaphore's own image describes it as
covering Terraform, OpenTofu, Terragrunt and PowerShell alongside Ansible. What each product does
and does not carry is the [comparison page](comparison.md), including the rows where SwitchTender is
behind. We publish no boot comparison against it, for the reason above.

**AWX is not shaped for a boot-time row, and pretending otherwise would be the dishonest thing.** It
has no single-process mode. Its own image ships two supervisor configurations: the web set runs
nginx, uwsgi, daphne and two helpers, and the task set runs the dispatcher, the websocket relay and
the callback receiver. Its default settings point the message broker, the cache and the websocket
channel layer at Redis, and its dispatcher listens on PostgreSQL `pg_notify`, so a database and a
cache are not optional extras. The same defaults schedule a receptor work-unit reaper every sixty
seconds and set a receptor service advertisement period, so a Receptor mesh stands alongside them.
The `receptor` binary is not in this image, so whatever carries it is not weighed below. The AWX
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
method. We will look, and we will correct the table if it is wrong.
