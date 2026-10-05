# Upgrading

This page lists what an install running v1.102.0 notices when it upgrades to this release, and what
to do about each change. Upgrading is replacing the binary, the package, or the image and starting
it again. Each process carries the database schema forward when it opens the database, so there is
no separate migration step.

## Before you upgrade

- Take a backup with v1.102.0, `switchtender backup --db <database> --out before-upgrade.stbak`, if
  you might roll back. This release writes backup format version 3, which v1.102.0 refuses to
  restore.
- Decide every run waiting for approval and let queued runs finish. A run submitted before the
  upgrade that names a stored inventory is refused when it starts afterward, as [runs submitted
  before the upgrade](#runs-submitted-before-the-upgrade) explains.
- On a server whose clock is not set to UTC, give each schedule that names no time zone the zone it
  is meant to fire in, as [schedules with no time zone run in
  UTC](#schedules-with-no-time-zone-run-in-utc) explains.
- List your tokens with `switchtender token list` and check that every AI agent's token has the kind
  `agent`. Only those are held as an agent's.
- Upgrade every server and relay worker together, and write a rule with `effect: exempt` only once
  all of them run this release. v1.102.0 does not know that effect.

## Agent runs are held by default

A run an agent token asks for now waits for a person before it executes, with no policy written,
under a built-in rule named `requested by an agent, held by default`. Its dry runs wait too, since a
check or a plan still runs code with the server's credentials, whatever the dry-run scan finds. Its
Terraform or OpenTofu apply takes two approvals: it is held before anything plans, and once a person
releases it, the apply its plan proposes is held again, carrying the saved plan. A person's runs are
unaffected. Under v1.102.0 such a run went ahead unless a policy held it.

What to do:

- Write a rule with `effect: exempt` for the routine agent work that should keep running unattended,
  and leave everything else held. An exemption that names an agent's `actor` must also name the
  `account` its token is bound to, and one that names the `actor` alone is refused. A Community
  install's one rule can be that exemption. [Agent runs are held by
  default](policy.md#agent-runs-are-held-by-default) covers what an exemption matches and what it
  risks.
- Plan for two approvals on each Terraform or OpenTofu apply an agent asks for, or cover the routine
  ones with an exemption, which lets an apply plan and apply as the rules allow for a person's.
- A token minted without `--agent` is recorded as a person's and is never held. Mint a replacement
  with `switchtender token new --user <account> --agent` for any agent holding such a token, then
  revoke the old one.
- Make sure a person with the admin role is there to approve, since an agent never can.

## Dry runs a rule exempts are scanned first

Under v1.102.0, `exclude_dry_run` let through any run submitted as a dry run. It now lets through
only a dry run the gate read in full without finding a known way for it to act for real. These are
matched as the real run would be, and a hold the scan caused carries a note naming what it found:

- An Ansible dry run whose playbook sets `check_mode` to anything but true, or runs a `pipe` lookup,
  written as `lookup`, `query`, or `q` or as a `with_pipe` loop, in a play's vars or environment or
  anywhere in a task. The lookup runs its command on the controller under `--check`.
- A Terraform or OpenTofu plan whose configuration declares an `external` or `aws_lambda_invocation`
  data source, or an `http` data source with a write method or a request body. A method the scan
  cannot read counts as a write.
- A dry run whose playbook, configuration, or modules could not be read in full. The gate does not
  download modules for a plan routed to a named queue, so such a plan that calls registry or remote
  modules is not exempt either.
- A dry run of a tool a plugin or the SDK added, which nothing scans.

The scan finds the known ways a preview acts. It does not prove one harmless, since lookups,
plugins, and providers run code during a check or a plan, so a rule with `exclude_dry_run` trusts
the playbooks and configurations it lets through.

What to do: read the hold note, then rework the task, the lookup, or the data source it names, or
vendor the module into the repository and call it by a local path. Keep `exclude_dry_run` to rules
over code your team reviews. [When a dry run you expected to pass is
held](concepts.md#when-a-dry-run-you-expected-to-pass-is-held) walks through the common cases.

## A held Terraform or OpenTofu apply is planned first

Under v1.102.0, only a `max_destroy` rule, or a risk or reversibility floor, planned an apply first.
An apply any other rule held, or one submitted with a request for approval, waited without a plan.
It is now planned at once, with the run's credentials, and the apply the plan proposes is what
waits, carrying the saved plan the approval binds. An agent's apply is held before anything plans
and again carrying the saved plan, as [agent runs are held by
default](#agent-runs-are-held-by-default) describes.

What to do: expect a plan run to start as soon as such an apply is submitted, and approve the apply
it proposes. [A gated apply carries out the approved
plan](tool-terraform.md#a-gated-apply-carries-out-the-approved-plan) has the details.

## Runs submitted before the upgrade

This release takes a snapshot of a run's stored inventory when the run is submitted, the approval
binds that snapshot, and the run executes against it. A run submitted under v1.102.0 carries no
snapshot, so one that names a stored inventory and starts after the upgrade is refused, with a
message saying to submit it again. That includes a run that was waiting for approval during the
upgrade.

What to do: decide held runs and let queued ones finish before you upgrade, and submit again any run
refused this way. The new submission takes its snapshot and is held again where a rule holds it.

## Schedules with no time zone run in UTC

Under v1.102.0, a schedule that named no time zone fired by the clock of whichever server evaluated
it. It now fires in UTC. The first server of this release to open the database writes `UTC` onto
every such schedule, an edit that names no zone keeps it in UTC, and so does restoring it from a
backup. A schedule created or imported from now on without a zone is given the server's own zone, by
name. A schedule whose cron expression or recurrence rule names its zone is unaffected.

What to do: on a server whose clock is not set to UTC, set `timezone` on each of those schedules to
the zone it is meant to fire in, before or after the upgrade. The published container image and the
Helm chart run in UTC, so those installs see no change. [Schedule
timezone](api.md#schedule-timezone) has the details.

## Backups are format version 3

This release writes backup format version 3 and restores versions 2 and 3. Version 3 carries
notification targets and their attachments, a template's sealed provisioning callback key, and a
secret survey question's sealed default. v1.102.0 refuses a version 3 file rather than restoring
part of it. A backup carries no forge account links, so after a restore each person links their
GitHub or GitLab account again.

What to do: keep a backup taken with v1.102.0 for as long as you might roll back to it.
[Restore](backup.md#restore) has the rest.

## A decision on a run that is no longer waiting is answered 409

An approve or a reject on a run that is no longer waiting for a decision is answered `409` and now
records nothing. That covers a run another decision or a cancel settled first, on this server or on
another one sharing the database. A decision claims the run before it is recorded, so the decision
that loses leaves no entry on the audit chain. Under v1.102.0, a decision that lost a race could
stay on the chain after the run's outcome, and checking the chain could then report it as not
verified. A `/switchtender apply` comment that arrives after the decision gets a reply naming the
run.

What to do: in scripts and chat bots, read a `409` from approve or reject as already decided, and
read the run to see which decision took effect.

## A `.terraform` directory the gate did not install is never trusted

The gate never reads a `.terraform` directory it did not install, whether a commit carries it or a
working directory on the server holds it. It copies the configuration without it, downloads the
registry and remote modules itself, and reads those. A run the gate downloaded modules for executes
exactly those modules, with `init` told to install none, and is refused when its modules no longer
match the digest the gate recorded, or when it sets `TF_DATA_DIR` to a directory of its own.

What to do: stop relying on a committed `.terraform`. Vendor a module into the repository and call
it by a local path to have it read from the commit, make registry modules reachable with the run's
own credentials from where the plan runs, and remove `TF_DATA_DIR` from the runs and credentials
that set it. [Dry runs and `exclude_dry_run`](concepts.md#dry-runs-and-exclude-dry-run) has the
details.

## Run files have a private directory the server checks at startup

Each run now stages its keys, tokens, and secret files in a private directory under a root the
server proves before it serves: owned by its account, closed to other accounts, on a local
filesystem it knows, and with locks that hold. A root on NFS, SMB, FUSE, or another filesystem it
does not know, or `/dev/shm`, stops the process with the reason and the flag to set. Under a systemd
unit written by v1.102.0, which sets no `RuntimeDirectory=`, run files go to the temporary
directory, and the server and the doctor warn about it. A Docker Compose install also stages them in
the container's temporary directory. The Helm chart mounts a 256Mi memory-backed volume for them,
which counts against the pod's memory limit.

What to do: add these lines to the `[Service]` section of a unit written by v1.102.0, then run
`systemctl daemon-reload` and restart the service:

    RuntimeDirectory=switchtender
    RuntimeDirectoryMode=0700
    RuntimeDirectoryPreserve=no
    LimitCORE=0
    KillMode=mixed
    TimeoutStopSec=90

Elsewhere, point `--runfiles-dir` at a private tmpfs. On Kubernetes, raise a tight pod memory limit.
[Where run files live](run-files.md#where-run-files-live) and [Kubernetes](run-files.md#kubernetes)
have the details.

## Cloud command line tools keep their state in the run's directory

A run that hands the AWS, Google Cloud, or Azure command line tools a credential now points their
configuration and cache variables into the run's private directory, so what they write leaves with
the run. Such a run no longer reads the configuration in the home directory of the account
SwitchTender runs as, such as the region in `~/.aws/config`. A run that hands a tool no credential
is unaffected.

What to do: set the region, and any other setting such a run took from that home directory, on the
credential. [Tool state](run-files.md#tool-state) lists the variables.

## A run's container image is fixed when it is submitted

For a run that executes in a container image, which `--allow-container-ee` turns on, the image is
now chosen when the run is submitted, from the run, its template, its project, or the server's
`--default-image`, and resolved to the digest its registry serves. `--image-digest-lookup` is on by
default, so the server asks the registry when the run is submitted. The approval covers that exact
image. A worker's own `--default-image` never applies to an approved run, so a run approved to
execute on the host is refused there rather than moved into another image.

What to do: set the fallback image with `--default-image` on `serve`, and turn
`--image-digest-lookup` off where the server cannot reach a registry.
[serve](configuration.md#serve) describes both flags.

## A plan's submission can wait for its modules

A Terraform or OpenTofu dry run whose configuration calls registry or remote modules is read by the
gate before the submission answers, and the gate downloads those modules first.
`--module-fetch-timeout` bounds the download at two minutes by default. A webhook still answers
within five seconds.

What to do: let API clients that submit plans wait that long. [How long a submission can
take](api.md#how-long-a-submission-can-take) has the details.

## Attention alerts are on

The server now alerts on the notification channels already configured when waiting work cannot move:
a run with no connected worker serving its queue for 15 minutes, a run blocked for 15 minutes, and a
lost worker's run one minute after it last reported. Each condition alerts once. An approval that
waits never alerts unless a thresholds file turns that on.

What to do: tune the thresholds, or turn an alert off, in the file `--attention-file` names.
[Attention thresholds](configuration.md#attention-thresholds) lists them.

## Requests that are refused with 400

A request whose path, query, or JSON body holds the NUL character, or text that is not valid UTF-8,
is now refused with `400` naming the field, and an import holding such text is refused before it
writes anything. An `Idempotency-Key` header, a queue, a username, or a token name past 255 bytes,
and a grant or membership id past 512 bytes, are refused with `400` stating the bound.

What to do: nothing for an ordinary client. A script that sent such values now gets `400`. [What a
request may carry](api.md#what-a-request-may-carry) has the bounds.

## Decisions made from a pull request comment

This release adds approving and applying from a GitHub or GitLab pull request comment. A decision
made that way is a `DECISION` entry at `/runs/<id>/decision/approved` like any other, recorded under
the commenter's SwitchTender account with the decider type `forge_comment`, and it commits the
forge, the comment's and its author's numeric ids, the SHA-256 of the comment body, and the plan it
approved. A run a comment plans or proposes carries `forge_comment` as its actor type, which a
policy reads as a person's, so a rule with `actor_kind: human` matches it.

What to do: nothing, unless a tool of yours reads the chain or the decisions API and expects only
the decider types it already knew. Comments act only for people who link a forge account, which
needs `--forge-oauth` and `--public-url`. [What the evidence
holds](pull-request-review.md#what-the-evidence-holds) has the details.

## Smaller changes

- An Ansible run whose inventory the native engine resolved is checked against the executor's own
  `ansible-inventory` just before the play, and refused on any difference, with each difference
  named. [How the agreement is kept](inventories.md#how-the-agreement-is-kept) describes the check.
- Secret masking also catches quoted and escaped assignments, so a log can show `***` where it used
  to show a value.
- `switchtender ansible install` can put a pinned ansible-core beside the database. Nothing changes
  until it is run, and once it is, runs use that ansible-core ahead of the one on the PATH unless
  `--ansible-bin` names another, as [which Ansible a run
  uses](ansible-runtime.md#which-ansible-a-run-uses) describes.
