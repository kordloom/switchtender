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
| Observability| A Prometheus metrics endpoint, webhook notifications when runs finish, and an audit trail of every mutation.|
| Tamper-evident audit | Every mutation is linked into a SHA-256 hash chain. `GET /v1/audit/verify` flags the first altered or deleted entry, and a signed export verified with `switchtender audit verify` proves the whole chain offline.|
| Inventories  | Stored inventories referenced by id, materialized on whichever executor runs the play.|
| Dynamic sources | Inventory plugins and scripts refreshed into stored inventories, with cloud auth from an env credential.|
| Sourced inventories | An inventory's content can come from a command, Vault, or Google Secret Manager, resolved at launch, so the host list need not live in SwitchTender.|
| Credentials  | Thirteen kinds: SSH keys and SSH passwords, vault passwords, become passwords and full become settings, network device logins, env bundles for cloud SDKs, API tokens and JWTs, registry logins, and typed AWS, Azure, GCP, and VMware cloud credentials, all encrypted at rest.|
| Secret masking | Credential values are redacted from run logs, live streams, and events, so a tool that echoes a secret shows `***` instead of the value.|
| High availability | Two servers on one database share the schedule without double-firing. Tokens can carry a lifetime.|
| Git triggers | A webhook URL launches a template on push. The project syncs fresh, so it deploys the commit just pushed.|
| Surveys      | Templates declare typed launch prompts, validated and injected as extra vars.|
| Worker queues | Target a run at a named queue. A worker serving that queue runs it and default workers leave it alone. Pin a queue on a run, a template, or an inventory, most specific wins, so queues work like AWX instance groups.|
| Dependency sync | A project's requirements.yml roles and collections install on each sync, so playbooks that need them just run.|
| Execution environments | A template, run, or project pins a container image and its runs execute inside it, with their own tool and system dependencies, for any of the seven tools. The most specific wins: run, then template, then project. Private registries pull with a stored credential.|
| Teams and grants | Group users into teams and grant use or manage on a specific project, template, inventory, or credential. A manage grant delegates editing and deleting that object without the global admin role. Grants layer on the global role and default open.|
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
| AI triage | An optional AI provider, local Ollama or a cloud API, explains a failed run on demand from its masked log and failed task events. Advisory only: it never runs anything, so every action still goes through the same policy and audit gates.|
| AI drafting | Describe a bash, python, powershell, or go step in the workflow editor and the provider drafts the script into the command field for you to review, edit, and save. The draft never executes on its own.|
| Drift reconcile | One click on a drifted target builds the fix, run for real and born held for approval: an Ansible host reruns its playbook limited to that host, a Terraform directory applies. A machine proposes, a person releases, and the audit trail records both.|
| Fleet questions | Ask the overview a plain-language question and the provider answers from a bounded snapshot of run counts, recent runs, host health, and drift. Metadata only, rate limited, and it can start nothing.|
| Run proposals | Describe a run on the runs page and the provider turns it into a validated run the server builds. It is born held for approval, stamped with your exact words, and runs nothing until an admin reviews the generated command and releases it. The audit trail records the request and the decision.|
| Go SDK | One public package registers new execution tools, AI providers, secret engines, and notification channels, compiled into the binary. A registered tool validates, executes, masks, and audits like a built-in. See [Extend in Go](sdk.md).|
| Drop-in plugins | The same extension builds as its own binary and loads from `--plugins-dir` on a stock release, no recompile. Plugins speak gRPC over a local socket with mutual TLS, supervised by the server, on the server and on workers.|
| Guided tours | A tour launcher in the top bar walks the product, the pitch, and the migration path step by step in the live UI, fully keyboard accessible.|
| Desktop mode | `switchtender desktop` serves on a stable loopback port, keeps its data in a private per-user directory, and opens the UI. Packaging recipes cover a macOS app bundle and a Windows installer.|
| Migration | `switchtender import awx` and `import semaphore` read an export and create the equivalent projects, inventories, templates, surveys, schedules, and credential shells, with a dry-run report first.|
