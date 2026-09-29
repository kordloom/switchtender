<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# SwitchTender compared to AWX and Semaphore

This page is an honest side-by-side. It states where SwitchTender is ahead, where it is even, and
where it is behind, because credibility comes from being straight about all three. Details of AWX
and Semaphore were current as of mid-2026. Verify them against their latest releases before relying
on them.

## Where SwitchTender is ahead

| Capability | SwitchTender | AWX | Semaphore |
|------------|------------|-----|-----------|
| Deployment | One binary, SQLite by default and PostgreSQL optional. | Kubernetes plus PostgreSQL, Redis, and Receptor. | One binary. |
| Run view | A structured host-by-task matrix with per-task drill-down, painted live over Server-Sent Events. | A text log stream. | A text log stream. |
| Job splitting | Shards balanced by each host's measured past duration, with only the failed shards retried. | Job slicing, round-robin. | Not available. |
| Pipelines | A dependency graph with parallel branches, per-step retries, and typed set_stats outputs passed to dependents. | Visual workflows. | Limited task chaining. |
| Visual workflow editor | A drag-and-drop canvas at Workflows builds the dependency graph in the browser: draft persistence, undo, keyboard editing, cycle refusal, and a pan, zoom, and fit-to-view viewport, on the same DAG engine the API uses. | A drag-and-drop editor. | On the roadmap. |
| Fleet memory | Flaky-host detection, outcome sparklines, per-host history, and task duration trends across runs. | Not available. | Not available. |
| Distributed workers | Store leasing, where the same single binary adds capacity, held together by leases and a janitor that requeues a crashed worker's runs. | A Receptor mesh. | Runners, in a paid tier. |
| Instance groups | A queue pins work at the run, template, or inventory level, most specific wins, so jobs land on the right worker group. | Instance groups. | Not available. |
| High availability | Active-active replicas on PostgreSQL behind any load balancer: store-claimed work, compare-and-set schedules and approvals, automatic failover through stale-lease reclaim, proven by a two-replica integration suite. | Via Kubernetes replicas. | Not documented. |
| Per-run provenance | Every run records the exact commit it executed. | Partial. | Partial. |
| Provable audit | A tamper-evident SHA-256 hash chain, verified offline, with an optional ed25519-signed export. | An activity stream. | An activity log. |
| Migration in | One command imports an AWX or Semaphore export. | Not applicable. | Not applicable. |
| Drift detection | A dry run reports what has diverged from the desired state, across Ansible hosts and Terraform working directories, with a one-click approval-gated reconcile to fix it. | No. | No. |
| Directory-driven roles | A directory or token group sets a user's role on every sign-in, over LDAP, SAML, or a bearer JWT. OIDC provisions every account at one configurable default role instead. | Organization mapping, complex. | No. Every user is assigned a role by hand. |
| Notification channels | Eleven server-wide: webhook, email, Slack, Mattermost, Rocket.Chat, Discord, Teams, ntfy, PagerDuty, Grafana, and Twilio. Seven of those, the ones that need only a URL, also route per template, so a team pages its own channel. PagerDuty, Grafana, Twilio, and email hold server-held account credentials and stay server-wide. | A similar set plus IRC, without Discord or ntfy, attached per job template. | Fewer, some in a paid tier. |

## Where they are even

| Capability | Notes |
|------------|-------|
| Multiple runtimes | SwitchTender runs Ansible, Terraform, OpenTofu, Bash, PowerShell, Python, and Go, each with a dry run. AWX is Ansible-only. Semaphore runs Ansible, Terraform, OpenTofu, and shell, but not Python, PowerShell, or Go. |
| Container execution environments | SwitchTender pins an image on a template, a run, or a project, most specific wins, with private-registry pulls, opt-in behind a flag. AWX attaches execution environments to job templates. Semaphore favors native runtimes instead. |
| Access control | SwitchTender has global roles plus organizations, teams, and per-object read, use, and manage grants, all in the source-available core. AWX has mature organization RBAC. Semaphore gates RBAC behind its Enterprise tier. |
| Credentials | Sealed with AES-256-GCM under a key derived by Argon2id, decrypted only at execution into the run's environment or a temporary file created mode 0600, kept off the command line either way, and deleted when the run ends. Thirteen credential kinds and eleven sources, nine of them external secret managers: HashiCorp Vault static and dynamic, AWS Secrets Manager and STS, Azure Key Vault, GCP Secret Manager, CyberArk Conjur and CCP, and 1Password. Several credentials of different kinds attach to one run. AWX matches this through credential plugins. Semaphore stores an SSH key or a username and password, with no external secret managers. |
| Scheduling | All three schedule runs. SwitchTender uses cron with highly available claiming so two servers do not double-fire. |
| Surveys and prompts | All three collect typed values at launch. |
| Inbound webhooks | All three launch on a git push. |
| Metrics | SwitchTender exposes Prometheus metrics for scraping. |
| Directory sign-in | SwitchTender, AWX, and Semaphore all sign in with LDAP and OpenID Connect. |

## Where SwitchTender is behind

| Capability | Status |
|------------|--------|
| Maturity | AWX and Semaphore have years of production use and large communities. SwitchTender is young. AWX's years now cut both ways: its last release was July 2024, and its next one removes LDAP, SAML, and OIDC from core. |

## The short version

SwitchTender wins on the axes that make AWX painful and Semaphore ordinary: no heavy control plane to
deploy, runs you can read as structure instead of scrollback, job splitting that actually balances,
a memory of how every host behaves over time, a visual workflow editor on a provable DAG engine,
and a one-command path off either incumbent. It is younger, and its integration catalog is smaller.
Both gaps are being closed on purpose.

The [reliability](reliability.md) page details how runs execute under load: bounded workers, splits
balanced by measured duration, coordinated pipeline failure, crash recovery through leases, and a
store that stays consistent while a fleet writes to it.
