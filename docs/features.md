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
| Runs         | Submit over HTTP, real ansible-playbook underneath, human log plus a structured event stream, per-task drill-down with stdout, stderr, rc, and diff |
| Live view    | Server-Sent Events paint the host matrix and log as the run executes            |
| Splits       | Shard an inventory across parallel slices, merged back into one matrix; hosts packed by measured duration from past runs |
| Shard retry  | Retry a finished split and only its failed shards run again, lineage recorded   |
| Pipelines    | Ordered steps or a dependency graph with parallel branches; failures skip exactly their dependents, per-step retry budgets, set_stats outputs flow to dependent steps as extra vars |
| Workers      | Point `yardmaster worker` at the same database and it competes for queued runs; leases, heartbeats, and a janitor make dead workers safe |
| Scheduling   | Cron schedules fire runs, splits, or pipelines with full history per fire       |
| Fleet memory | Failure rankings, flaky-host detection, outcome sparklines, per-host history, task duration trends, all from persisted structured events |
| Recovery     | Cancellation across processes recorded as canceled, orphaned runs interrupted by lease expiry, terminal saves retried |
| Storage      | SQLite by default; the same flag takes a PostgreSQL DSN for multi-instance      |
| Projects     | Playbooks sourced from git with clone-or-fetch sync; every run records the exact commit it executed |
| Templates    | Saved launch presets bundling project, playbook, credentials, shards, and extra vars; one click or one POST launches |
| Auth         | User accounts with admin, operator, and viewer roles enforced per route; bearer tokens hashed at rest; the API locks down the moment the first token exists |
| Observability| A Prometheus metrics endpoint, webhook notifications when runs finish, and an audit trail of every mutation |
| Inventories  | Stored inventories referenced by id, materialized on whichever executor runs the play |
| Dynamic sources | Inventory plugins and scripts refreshed into stored inventories, with cloud auth from an env credential |
| Credentials  | SSH keys, vault passwords, env bundles for cloud SDKs, become passwords, and registry logins, all encrypted at rest |
| High availability | Two servers on one database share the schedule without double-firing; tokens can carry a lifetime |
| Git triggers | A webhook URL launches a template on push; the project syncs fresh, so it deploys the commit just pushed |
| Surveys      | Templates declare typed launch prompts, validated and injected as extra vars |
| Worker queues | Target a run at a named queue; a worker serving that queue runs it and default workers leave it alone |
| Dependency sync | A project's requirements.yml roles and collections install on each sync, so playbooks that need them just run |
| Execution environments | A project can pin a container image; its runs execute inside it with their own ansible and system dependencies |
| Teams and grants | Group users into teams and grant use or manage on a specific project, template, inventory, or credential; grants layer on the global role and default open |
| Retention | A sweeper drops old run events and deletes terminal runs past a configurable age, keeping the summaries the cross-run views need |
| Email | An SMTP notification on every finished run or on failures only, alongside the finish webhooks |
| Migration | `yardmaster import awx` and `import semaphore` read an export and create the equivalent projects, inventories, templates, surveys, schedules, and credential shells, with a dry-run report first |
