<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Features

What Yardmaster does today.

| Capability   | What you get                                                                    |
|--------------|---------------------------------------------------------------------------------|
| Runs         | Submit over HTTP, real ansible-playbook underneath, human log plus a structured event stream, per-task drill-down with stdout, stderr, rc, and diff.|
| Live view    | Server-Sent Events paint the host matrix and log as the run executes.           |
| Splits       | Shard an inventory across parallel slices, merged back into one matrix. Hosts packed by measured duration from past runs.|
| Shard retry  | Retry a finished split and only its failed shards run again, lineage recorded.  |
| Pipelines    | Ordered steps or a dependency graph with parallel branches. Failures skip exactly their dependents, per-step retry budgets, set_stats outputs flow to dependent steps as extra vars.|
| Workflow editor | A drag-and-drop canvas at Workflows builds the dependency graph in the browser: draft persistence, undo and redo, full keyboard editing, and cycle refusal, submitted as a pipeline.|
| Workers      | Point `yardmaster worker` at the same database and it competes for queued runs. Leases, heartbeats, and a janitor make dead workers safe.|
| Scheduling   | Cron schedules fire runs, splits, or pipelines with full history per fire.      |
| Fleet memory | Failure rankings, flaky-host detection, outcome sparklines, per-host history, task duration trends, all from persisted structured events.|
| Recovery     | Cancellation across processes recorded as canceled, orphaned runs interrupted by lease expiry, terminal saves retried.|
| Storage      | SQLite by default. The same flag takes a PostgreSQL DSN for multi-instance.     |
| Projects     | Playbooks sourced from git with clone-or-fetch sync. Every run records the exact commit it executed.|
| Templates    | Saved launch presets bundling project, playbook, credentials, shards, and extra vars. One click or one POST launches.|
| Auth         | User accounts with admin, operator, and viewer roles enforced per route. Bearer tokens hashed at rest. The API locks down the moment the first token exists.|
| Approvals    | Mark a run to require sign-off, or require it automatically by policy on tool, command, or target. A held run never executes until an admin approves or rejects it, and the request and decision land in the tamper-evident audit trail.|
| Observability| A Prometheus metrics endpoint, webhook notifications when runs finish, and an audit trail of every mutation.|
| Tamper-evident audit | Every mutation is linked into a SHA-256 hash chain. `GET /audit/verify` flags the first altered or deleted entry, and a signed export verified with `yardmaster audit verify` proves the whole chain offline.|
| Inventories  | Stored inventories referenced by id, materialized on whichever executor runs the play.|
| Dynamic sources | Inventory plugins and scripts refreshed into stored inventories, with cloud auth from an env credential.|
| Sourced inventories | An inventory's content can come from a command, Vault, or Google Secret Manager, resolved at launch, so the host list need not live in Yardmaster.|
| Credentials  | SSH keys, vault passwords, env bundles for cloud SDKs, API tokens and JWTs, become passwords, and registry logins, all encrypted at rest.|
| Secret masking | Credential values are redacted from run logs, live streams, and events, so a tool that echoes a secret shows `***` instead of the value.|
| High availability | Two servers on one database share the schedule without double-firing. Tokens can carry a lifetime.|
| Git triggers | A webhook URL launches a template on push. The project syncs fresh, so it deploys the commit just pushed.|
| Surveys      | Templates declare typed launch prompts, validated and injected as extra vars.|
| Worker queues | Target a run at a named queue. A worker serving that queue runs it and default workers leave it alone.|
| Dependency sync | A project's requirements.yml roles and collections install on each sync, so playbooks that need them just run.|
| Execution environments | A project can pin a container image. Its runs execute inside it with their own ansible and system dependencies.|
| Teams and grants | Group users into teams and grant use or manage on a specific project, template, inventory, or credential. Grants layer on the global role and default open.|
| Retention | A sweeper drops old run events and deletes terminal runs past a configurable age, keeping the summaries the cross-run views need.|
| Email | An SMTP notification on every finished run or on failures only, alongside the finish webhooks.|
| Slack | A formatted message posts to a Slack incoming webhook when a run finishes, with the run label, status, and elapsed time.|
| AI triage | An optional AI provider, local Ollama or a cloud API, explains a failed run on demand from its masked log and failed task events. Advisory only: it never runs anything, so every action still goes through the same policy and audit gates.|
| AI drafting | Describe a bash, python, or go step in the workflow editor and the provider drafts the script into the command field for you to review, edit, and save. The draft never executes on its own.|
| Drift reconcile | One click on a drifted host builds the fix: the check run's playbook limited to that host, run for real, born held for approval. A machine proposes, a person releases, and the audit trail records both.|
| Fleet questions | Ask the overview a plain-language question and the provider answers from a bounded snapshot of run counts, recent runs, host health, and drift. Metadata only, rate limited, and it can start nothing.|
| Guided tours | A tour launcher in the top bar walks the product, the pitch, and the migration path step by step in the live UI, fully keyboard accessible.|
| Desktop mode | `yardmaster desktop` serves on a stable loopback port, keeps its data in a private per-user directory, and opens the UI. Packaging recipes cover a macOS app bundle and a Windows installer.|
| Migration | `yardmaster import awx` and `import semaphore` read an export and create the equivalent projects, inventories, templates, surveys, schedules, and credential shells, with a dry-run report first.|
