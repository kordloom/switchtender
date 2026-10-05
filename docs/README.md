<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Documentation

SwitchTender runs Ansible, Terraform, OpenTofu, Bash, PowerShell, Python, and Go across a fleet and treats every run as
structured data. One binary. `serve` is the API, the executor, the scheduler, and the web UI.
`worker` adds capacity.
State lives in one database, SQLite by default or PostgreSQL by DSN. These pages also render inside
the app at `/ui/docs`.

| Guide | What |
|-------|------|
| [Quickstart](quickstart.md) | Zero to a first run in a few minutes.|
| [Switching from AWX](switching-from-awx.md) | Import projects, inventories, templates, workflows, surveys, schedules, notification templates, and organizations with a dry run first, or set up from scratch.|
| [Tutorials](tutorials.md) | Task-focused walk-throughs for everyday work.|
| [Concepts](concepts.md) | Runs, splits, pipelines, projects, templates, and the rest.|
| [Reliability](reliability.md) | How runs execute: workers, splits, failure, recovery, durability.|
| [Configuration](configuration.md) | Every command, flag, and environment variable.|
| [Run files](run-files.md) | Where a run stages keys and tokens on disk, how they go after a crash, and keeping them off disk.|
| [Approval policies](policy.md) | YAML rules and Rego policies, the input a policy reads, and how it fails closed.|
| [Inventories](inventories.md) | Static, dynamic, smart, and constructed inventories, and the hosts a run resolved to.|
| [Managed Ansible runtime](ansible-runtime.md) | One command installs a pinned, hash-checked ansible-core for runs to use.|
| [Federated cloud credentials](federation.md) | Short-lived AWS, Google Cloud, and Azure access minted per run, with nothing stored.|
| [Pull request review](pull-request-review.md) | Plan comments and commit statuses on GitHub and GitLab pull requests.|
| [Desktop](desktop.md) | Run SwitchTender as a local desktop app.|
| [Features](features.md) | The full capability list.|
| [Compliance mapping](compliance.md) | What the record shows for SOC 2, ISO 27001, and HIPAA change controls.|
| [Threat model](threat-model.md) | Each homepage claim mapped to its mechanism, the adversaries defended against, and the limits.|
| [Sample evidence pack](sample-evidence-pack.md) | What the evidence a reviewer samples actually looks like.|
| [Advisory AI](ai.md) | The five AI features, the guarantees, providers, and what a model sees.|
| [AI agents](agents.md) | Run an AI agent through the gate: one token, gated, chained, provable.|
| [Extend in Go](sdk.md) | The SDK: add tools, AI providers, secret engines, and notifiers.|
| [HTTP API](api.md) | Every endpoint the server exposes.|
| [Inventory engine](inventories.md#which-engine-resolves-an-inventory) | How a static inventory is read without Ansible, held in CI to every ansible-core minor from 2.16 to 2.21.|
| [Provisioning callbacks](tool-ansible.md#provisioning-callbacks) | A host launches its template against itself, answering at the AWX callback address too.|
| [Dry runs](concepts.md#dry-runs-and-exclude-dry-run) | When a rule lets a dry run through, and when the gate holds it as the real run it could be.|
| [Key rotation](federation.md#signing-keys) | How the federation signing key rotates on schedule, or at once in an emergency.|
| [Relay worker secrets](configuration.md#delivering-secrets-to-relay-workers) | Each run's secrets sealed to a worker pool's key and bound to the claim.|
| [Notification delivery](api.md#delivery-order) | Deliveries to named targets attempted in the run's order from a queue in the database, retried, and resumed by the next server.|
| [Schedules](tutorial-schedule-a-job.md) | Cron or RFC 5545 recurrence rules, previewed before saving, with one run per occurrence across restarts.|
| [Receipts](concepts.md#provable-audit) | What a run's receipt discloses, and what a LoomSeal verifier from 1.7.0 on checks in it.|
| [Benchmarks](benchmarks.md) | Binary size, idle memory, and boot time, with the method and the date.|
| [Migration](migration.md) | Moving off AWX, Semaphore, Chef, Puppet, Rundeck, Jenkins, or cron in detail, with the table of what each source brings over.|
| [Comparison](comparison.md) | How SwitchTender compares to AWX, AAP, Semaphore, Ascender, and Rundeck.|

For deployment, the repository root holds a `docker-compose.yml` for a server, a database, and a
worker, and [deploy/helm](https://github.com/kordloom/switchtender/tree/main/deploy/helm) holds a
Helm chart.
