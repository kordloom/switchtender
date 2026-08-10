<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Features

What SwitchTender does today.

| Capability   | What you get                                                                    |
|--------------|---------------------------------------------------------------------------------|
| Runs         | Submit over HTTP, real ansible-playbook underneath, human log plus a structured event stream, per-task drill-down with stdout, stderr, rc, and diff.|
| Multi-tool   | Ansible, Terraform, OpenTofu, Bash, PowerShell, Python, and Go through one engine, one UI, and one audit trail, each with a dry-run mode.|
| Live view    | Server-Sent Events paint the host matrix and log as the run executes.           |
| Splits       | Shard an inventory across parallel slices, merged back into one matrix. Hosts packed by measured duration from past runs.|
| Shard retry  | Retry a finished split and only its failed shards run again, lineage recorded.  |
| Pipelines    | Ordered steps or a dependency graph with parallel branches. Failures skip exactly their dependents, per-step retry budgets, set_stats outputs flow to dependent steps as extra vars.|
| Workflow editor | A drag-and-drop canvas at Workflows builds the dependency graph in the browser: draft persistence, undo and redo, full keyboard editing, cycle refusal, and a pan, zoom, and fit-to-view viewport that stays smooth on a large graph, submitted as a pipeline.|
| Workers      | Point `switchtender worker` at the same database and it competes for queued runs. Leases, heartbeats, and a janitor make dead workers safe.|
| Scheduling   | Cron schedules fire runs, splits, or pipelines with full history per fire.      |
| Fleet memory | Failure rankings, flaky-host detection, outcome sparklines, per-host history, task duration trends, all from persisted structured events.|
| Recovery     | Cancellation across processes recorded as canceled, orphaned runs interrupted by lease expiry, terminal saves retried.|
| Storage      | SQLite by default. The same flag takes a PostgreSQL DSN for multi-instance.     |
| Projects     | Playbooks sourced from git with clone-or-fetch sync. Every run records the exact commit it executed.|
| Templates    | Saved launch presets bundling project, playbook, credentials, shards, and extra vars. One click or one POST launches.|
| Auth         | User accounts with admin, operator, and viewer roles enforced per route. Bearer tokens hashed at rest. The API locks down the moment the first token exists.|
| Approvals    | Mark a run to require sign-off, or require it automatically by policy on tool, command, or target. A held run never executes until an admin approves or rejects it, and the request and decision land in the tamper-evident audit trail.|
| Policy as code | Point `--policy-file` at a YAML file and it becomes the source of truth for approval policies. A change to what needs approval is then a reviewed diff with a commit behind it, the API refuses policy writes with a 409 that says where to make the change, and a malformed file stops the server rather than silently gating nothing.|
| Observability| A Prometheus metrics endpoint, webhook notifications when runs finish, and an audit trail of every mutation.|
| Tamper-evident audit | Every mutation is linked into a SHA-256 hash chain. `GET /v1/audit/verify` flags the first altered or deleted entry, and a signed export verified with `switchtender audit verify` proves the whole chain offline. A change that cannot be recorded is refused rather than performed silently.|
| Anchored history | `switchtender audit anchor` has a public RFC 3161 timestamp authority sign the current chain head. The token travels in every bundle and is checked offline by anyone, so a chain that has lost its tail no longer reaches its anchor.|
| Audit receipts | Every mutation returns an `Audit-Receipt` header naming where it landed in the chain. `switchtender audit receipt` redeems one, so a party who holds a receipt can prove their entry was not dropped.|
| Evidence report | `switchtender audit report` renders a signed export into a self-contained HTML compliance report, verifying it offline in the same pass, so a vendor-security or SOC 2 reviewer reads the verdict and re-verifies it against the export with no tooling.|
| Launch receipts | A run created by a recorded request, whether an API launch or a webhook fire, records the `seq:link` of the chain entry that authorized it, so who asked is redeemable evidence rather than a field the server asserts. A creation entry the chain can no longer produce is reported. A run started by the scheduler, or created before this release, carries none and its dossier says so plainly rather than implying one was lost.|
| Run dossier | `switchtender audit run <id>`, or the run page's Evidence button, emits one self-contained document tying the run's spec, risk grade, approval decisions, per-host outcomes, chain receipts, and covering anchors together, so an auditor's sample request is answered in one export.|
| Recorded holds | A run held for approval records which rule held it, by the name that rule carried at the hold, so the register answers "what stopped this change" even after the policy is renamed or deleted. A plan held for destroying too much records the threshold it crossed.|
| Scheduled evidence packs | `--evidence-dir` with `--evidence-cadence` writes a change register covering each elapsed period, named for the period to the second, so an archive sorts chronologically and a missing period is a missing file. Progress lives in the archive rather than in memory, so a restart resumes where the last pack ended and a period that failed to write is covered by the next attempt instead of being skipped. The evidence a review samples from exists before it is asked for.|
| Change register | `switchtender audit report --from --to`, or the audit page's Evidence pack button, renders every change in the period with its actor, chain-recorded decision, risk grade, and outcome. That is the change-management sample a SOC 2 CC8.1 or ISO/IEC 27001 A.8.32 review asks for.|
| Chain witness | `switchtender witness` watches the public beat feed from another machine, remembers what it saw in a signed checkpoint, and raises a finding on a missing beat, a rewritten beat, or a head regression, so an insider with database access cannot silently truncate history.|
| Run comparison | Every run page carries a Compare button answering "this worked yesterday, what changed": host-by-host verdicts (broke, recovered, still failing), task timing swings, and the duration delta against the previous run of the same template, exportable like every table.|
| SIEM forwarding | `--forward-url` and `--forward-syslog` stream every audit entry into the tools your security team already watches, as NDJSON over HTTP or RFC 5424 syslog. Every event carries its `seq:link` receipt, so any event sampled from the SIEM months later is redeemable against the live chain with `switchtender audit receipt`. Delivery is at least once behind a durable cursor: an outage delays events, it does not drop them.|
| Hosted witness | `switchtender witness serve` watches many servers from one process, records every finding durably, and serves countersigned attestations of what it holds over a read-only API. A relying party verifies one offline with `switchtender witness verify-attestation` against the witness key they pinned, so "our history is intact" becomes a statement a third party signs rather than a claim the operator makes.|
| Inventories  | Stored inventories referenced by id, materialized on whichever executor runs the play.|
| Dynamic sources | Inventory plugins and scripts refreshed into stored inventories, with cloud auth from an env credential.|
| Sourced inventories | An inventory's content can come from a command, Vault, or Google Secret Manager, resolved at launch, so the host list need not live in SwitchTender.|
| Credentials  | Fourteen kinds: SSH keys and SSH passwords, vault passwords, become passwords and full become settings, network device logins, env bundles for cloud SDKs, API tokens and JWTs, registry logins, and typed AWS, Azure, GCP, VMware, and OpenStack cloud credentials, all encrypted at rest.|
| Secret masking | Credential values are redacted from run logs, live streams, and events, so a tool that echoes a secret shows `***` instead of the value.|
| High availability | Two servers on one database share the schedule without double-firing. Tokens can carry a lifetime.|
| Git triggers | A webhook URL launches a template on push. The project syncs fresh, so it deploys the commit just pushed.|
| Surveys      | Templates declare typed launch prompts, text, multiline, integer, boolean, or choice, each with bounds checked before launch: min and max on a number, length and a regular-expression pattern on text, and a help line on any field. A launch that breaks a bound is refused with the field it failed, and injected as extra vars.|
| Ansible controls | A run or template pins `tags` and `skip_tags` to select or exclude tagged plays, `forks` for how many hosts run at once, `verbosity` for `-v` through `-vvvv`, and `diff_mode` to show the before and after of every changed file, no hand-built command needed. They carry through schedules, triggers, and retries.|
| Relaunch failures | One action re-runs only the hosts a finished run left failed or unreachable, targeted at exactly that set, so a partial outage is closed without repeating the hosts that already succeeded.|
| Worker queues | Target a run at a named queue. A worker serving that queue runs it and default workers leave it alone. Pin a queue on a run, a template, or an inventory, most specific wins, so queues work like AWX instance groups.|
| Dependency sync | A project's requirements.yml roles and collections install on each sync, so playbooks that need them just run.|
| Execution environments | A template, run, or project pins a container image and its runs execute inside it, with their own tool and system dependencies, for any of the seven tools. The most specific wins: run, then template, then project. Private registries pull with a stored credential.|
| Teams and grants | Group users into teams and grant read, use, or manage on a specific project, template, inventory, or credential, each level implying the ones below it. A read grant delegates view of one object without a global viewer role; a manage grant delegates editing and deleting without global admin. Grants layer on the global role and default open, or lock down with strict grants.|
| Reach isolated networks | A worker dials out to the control node over the mesh relay with a token and needs no inbound port, so it runs jobs inside an air-gapped segment, a DMZ, or a customer network the control node cannot reach directly. Point `switchtender worker --server` at the control node and set the worker token.|
| Backup and restore | One encrypted file holds the whole control plane: credentials, projects, templates, inventories, schedules, triggers, and access. Sealed with the deployment key, so it is confidential and tamper-evident, and it restores into either SQLite or Postgres.|
| Retention | A sweeper drops old run events and deletes terminal runs past a configurable age, keeping the summaries the cross-run views need.|
| Email | An SMTP notification on every finished run or on failures only, alongside the finish webhooks.|
| Slack | A formatted message posts to a Slack incoming webhook when a run finishes, with the run label, status, and elapsed time.|
| Mattermost and Rocket.Chat | The same message posts to a Mattermost or Rocket.Chat incoming webhook, which both accept the Slack-compatible payload.|
| Discord | A formatted message posts to a Discord incoming webhook when a run finishes, the same summary as Slack in Discord markdown.|
| Microsoft Teams | An Adaptive Card posts to a Teams incoming webhook when a run finishes, colored by outcome with the status, elapsed time, and any error.|
| ntfy | A notification publishes to an ntfy topic when a run finishes, at raised priority for a failure, with optional token auth for a protected topic.|
| PagerDuty | A failed or interrupted run triggers a PagerDuty incident through the Events API, deduplicated per run, so an on-call responder is paged. A succeeded run pages no one.|
| Grafana | A finished run posts an annotation to the Grafana annotations API, tagged with switchtender and the run status, so runs show as markers on dashboards.|
| Twilio SMS | A failed or interrupted run texts each configured phone number through the Twilio API. A succeeded run texts no one.|
| AI triage | An optional AI provider, local Ollama or a cloud API, explains a failed run on demand from its masked log and failed task events. Advisory only. It never runs anything, so every action still goes through the same policy and audit gates.|
| AI drafting | Describe a bash, python, powershell, or go step in the workflow editor and the provider drafts the script into the command field for you to review, edit, and save. The draft never executes on its own.|
| Drift reconcile | One click on a drifted target builds the fix, run for real and born held for approval: an Ansible host reruns its playbook limited to that host, a Terraform directory applies. A machine proposes, a person releases, and the audit trail records both.|
| Fleet questions | Ask the overview a plain-language question and the provider answers from a bounded snapshot of run counts, recent runs, host health, and drift. Metadata only, rate limited, and it can start nothing.|
| Run proposals | Describe a run on the runs page and the provider turns it into a validated run the server builds. It is born held for approval, stamped with your exact words, and runs nothing until an admin reviews the generated command and releases it. The audit trail records the request and the decision.|
| Go SDK | One public package registers new execution tools, AI providers, secret engines, and notification channels, compiled into the binary. A registered tool validates, executes, masks, and audits like a built-in. See [Extend in Go](sdk.md).|
| Drop-in plugins | The same extension builds as its own binary and loads from `--plugins-dir` on a stock release, no recompile. Plugins speak gRPC over a local socket with mutual TLS, supervised by the server, on the server and on workers.|
| Guided tours | A tour launcher in the top bar walks the product, the pitch, and the migration path step by step in the live UI, fully keyboard accessible.|
| Desktop mode | `switchtender desktop` serves on a stable loopback port, keeps its data in a private per-user directory, and opens the UI. Packaging recipes cover a macOS app bundle and a Windows installer.|
| Migration | `switchtender import awx` and `import semaphore` read an export and create the equivalent projects, inventories, templates, surveys, schedules, and credential shells, with a dry-run report first. `switchtender import rundeck` reads a Rundeck job export, converting Quartz schedules and refusing to downgrade secure options to plaintext. `switchtender import cron` brings a crontab under governance, turning each cron line into an approved, recorded schedule.|
| Schedule timezones | A schedule reads its cron expression in an IANA timezone such as `America/New_York`, following that zone's daylight-saving shifts, so a nightly window stays put across the year instead of drifting with the server clock.|
| Starter templates | `switchtender examples` seeds a fresh install with a few templates that run with no project, inventory, or credential, so the first launch works on the spot instead of facing an empty list.|

## Operator toolbelt

- Rerun any finished run with its exact stored spec, credentials included, from the run page.
  Split parents rerun as a new split. Every rerun is authorized like a fresh launch.
- Launch a template with overrides: host limit, a different stored inventory, selectable
  credential picks, extra vars merged over the template's, and a dry-run toggle. Overrides pass
  the same authorization as the template's own objects.
- The doctor at /ui/doctor verifies every registered reference: templates to inventories,
  projects, and credentials, schedules to templates, and every cron expression, plus credentials
  still waiting for a secret. Each finding links to the page that fixes it.
- Schedules preview their next firings as you type the cron expression.
- Every list exports the filtered rows as CSV or JSON. Runs export events as NDJSON, the full
  log as text, and per-host results as JSON. Credentials are excluded from export on purpose.
- Per-template notification routing is editable in the template dialog, with failure-only delivery
  per target. All eleven channels take a per-template target. Webhook, Slack, Mattermost,
  Rocket.Chat, Discord, Teams, and ntfy targets name a URL. A PagerDuty target names its own
  routing key and a Grafana target its own instance and token, so each template can page or
  annotate its own team. A Twilio or email target names only a recipient and sends through the
  server-held account, so the account secret never lives in a template. Routing keys and tokens
  are masked on read the same as webhook URLs.
- The overview charts runs per day, and every outcome tick in fleet health links to its run.

## Run provenance and search

- Every run records what fired it: the API, a template, a schedule, a rerun, a drift fix, or a
  proposal, along with the authenticated actor and the object behind it. The runs list shows an
  Origin column that links back to the template, schedule, or earlier run.
- A rerun records the run it replayed, so lineage survives across repeated executions.
- Runs carry labels, arbitrary key values such as env=prod or ticket=OPS-123, set at launch and
  clickable in the list to filter by that pair.
- The runs search box accepts fielded terms alongside free text: status:failed, tool:bash,
  source:schedule, actor:alice, host:web01, and label:env=prod, all resolved by the server across
  the whole history rather than the loaded page.
- Playbook names open the file itself, read only, from the project's cached checkout.

## Host facts

- A play that gathers facts records what each host is: distribution and version, kernel,
  architecture, vCPUs, memory, address, service manager, virtualization, and Python version.
- The facts appear on the host's page with when they were gathered and a link to the run that
  gathered them. A later gather replaces an earlier one, so the page always shows current truth.
- Nothing is gathered unless a play asks for it, so a fleet that runs with `gather_facts: false`
  stores nothing and the page says so.

## Running a public demo

- `switchtender demo` seeds a database with sample projects, templates, inventories, and real
  runs, then serves it with every change rejected, so it is safe to expose.
- `--seed-only` and `--no-seed` split seeding from serving, so a public instance can build its
  next database off to the side and swap it in without a gap in service.
