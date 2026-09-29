<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# SwitchTender compared to the field

This page is an honest side-by-side. It states where SwitchTender is ahead, where it is even, and
where it is behind, because credibility comes from being straight about all three.

Every claim about another product was checked against that vendor's own documentation on
2026-07-31, for AWX 24.6.1, Ansible Automation Platform 2.7, Semaphore 2.18.29, Ascender 25.4.0,
and Rundeck 6.0.1. All of these ship, and a comparison decays the day it is written. Check a row
against the current release before relying on it, and open an issue if one has gone stale.

## The field, side by side

Six controllers, eleven capabilities, every cell checked against that vendor's own documentation.
Sources are numbered and listed at the bottom. A cell reading "not documented" means the vendor's
documentation does not describe the capability, established by searching their whole documentation
set rather than by not finding it.

| Capability | SwitchTender | AWX | AAP | Semaphore | Ascender | Rundeck |
|---|---|---|---|---|---|---|
| Runs without Kubernetes | Yes, one binary | No [1] | Yes, Podman on RHEL [2] | Yes, one binary [3] | No [4] | Yes, needs a JVM [5] |
| Datastore | SQLite, Postgres optional | PostgreSQL 15 [1] | PostgreSQL 15 [2] | SQLite, MySQL or Postgres [3] | PostgreSQL [4] | H2, not for production [5] |
| Other runtime services | None | Redis, Receptor [1] | Redis, Receptor [2] | None single-node, Redis for HA [3] | Valkey, Receptor [4] | None required [5] |
| Tools it executes | Ansible, Terraform, OpenTofu, Bash, PowerShell, Python, Go | Ansible only [6] | Ansible only [7] | Ansible, Terraform, OpenTofu, PowerShell, Bash, Python [8] | Ansible only [9] | Ansible plus generic scripts, no Terraform step [10] |
| Live per-host status | Host-by-task matrix | Host status bar, per-event drill-down [11] | Host status bar, per-event drill-down [12] | Status and a streamed log [13] | Host status bar, per-event drill-down [14] | Per-node, per-step monitor [15] |
| Tamper-evident audit | Hash-chained, signed, anchored | No [16] | No [17] | No [18] | No [19] | No [20] |
| Verifiable offline by a third party | Yes, open format and verifier | Not documented | Not documented | Not documented | Not documented | Not documented |
| Approval gates | Yes, by policy | Yes [21] | Yes [22] | Not documented [23] | Yes [24] | Not documented [25] |
| RBAC in the free tier | Full, plus per-object grants | Yes, no paid tier exists [26] | Yes, subscription required for the product [27] | Four fixed project roles; custom roles are Enterprise [28] | Yes [29] | Yes, and finely grained; GUI editor is commercial [30] |
| Drift detection | Yes, from a dry run | Not documented | Not documented | Not documented | Not documented | Not documented |
| Workflows | DAG with a drag-and-drop editor | Visual editor [31] | Visual editor [32] | Basic build and deploy chaining [33] | Visual editor [34] | Engine yes; visualization is commercial [35] |
| Minimum published footprint | One binary, one file | Not published as a single figure | 16 GB RAM, 4 CPU, 60 GB [2] | Not published [36] | 8 GB RAM, 2 CPU, 20 GB [37] | 8 GB RAM, 2 CPU [5] |
| License | BSL 1.1, converts to Apache 2.0 | Apache 2.0 [38] | Paid subscription [39] | MIT [40] | Apache 2.0 [41] | Apache 2.0, commercial tier separate [42] |

### Where the field is even or ahead

Three rows above deserve saying out loud rather than leaving in a table.

**Semaphore is the same shape as SwitchTender.** It is a single Go binary defaulting to SQLite, with
no Kubernetes, Docker, or JVM requirement, and it is MIT licensed, which is more permissive than
BSL 1.1. "One binary, no dependencies" is a tie against Semaphore, not an advantage, and its free
tier includes project RBAC, a full API, scheduling, and an encrypted key store.

**Rundeck matches the live per-host view.** It has shown a per-node, per-step execution monitor for
years, with each node's current step, its duration, and a summary of waiting, running, and done.
That is a peer capability, not a gap.

**Rundeck's free authorization model is more granular than ours in one respect.** Its ACL policies
are files: two contexts, regex matching on user and group, per-resource and per-action rules, and
deny-first precedence, all version-controllable. SwitchTender answers the file half of that with
`--policy-file` for approval policies, but Rundeck applies the file model to authorization as a
whole.

**AWX and AAP show more than a text log.** Both render a host status bar with per-event host
drill-down and track ok, changed, failed, unreachable, skipped, rescued, and ignored per host. The
difference against SwitchTender is a matrix against a summary with drill-down, which is narrower
than "structured versus scrollback".

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

## Sources

Fetched and read on 2026-07-31.

1. AWX install and operator defaults: <https://github.com/ansible/awx/blob/devel/INSTALL.md>, <https://raw.githubusercontent.com/ansible/awx-operator/devel/roles/installer/defaults/main.yml>
2. AAP deployment models and system requirements: <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7>
3. Semaphore admin introduction and config: <https://semaphoreui.com/docs/admin-guide/introduction>, <https://github.com/semaphoreui/semaphore/blob/develop/util/config.go>
4. Ascender installer and requirements: <https://github.com/ctrliq/ascender-install>, <https://raw.githubusercontent.com/ctrliq/ascender/main/requirements/requirements.txt>
5. Rundeck system requirements: <https://docs.rundeck.com/docs/administration/install/system-requirements.html>
6. AWX job types: <https://raw.githubusercontent.com/ansible/awx/devel/awx/main/models/base.py>
7. AAP job template job types: <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7>
8. Semaphore supported tools: <https://semaphoreui.com/docs/admin-guide/introduction>
9. Ascender job templates: <https://docs.ascender-automation.org/userguide/job_templates.html>
10. Rundeck distributed plugins: <https://docs.rundeck.com/docs/manual/plugins/full-list.html>
11. AWX job output and host status bar: <https://raw.githubusercontent.com/ansible/awx/24.6.1/awx/ui/src/screens/Job/JobOutput/shared/HostStatusBar.js>
12. AAP playbook run output: <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7>
13. Semaphore task view: <https://semaphoreui.com/docs/user-guide/tasks>
14. Ascender jobs view: <https://docs.ascender-automation.org/userguide/jobs.html>
15. Rundeck execution monitor: <https://docs.rundeck.com/docs/manual/07-executions.html>
16. AWX activity stream model, no hash or signature fields: <https://raw.githubusercontent.com/ansible/awx/devel/awx/main/models/activity_stream.py>
17. AAP activity stream, and Red Hat's guidance to use external log providers for non-repudiation: <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7>
18. Semaphore activity and audit logs: <https://semaphoreui.com/docs/admin-guide/logs>
19. Ascender activity stream and logging: <https://docs.ascender-automation.org/administration/logging.html>
20. Rundeck audit trail, which delegates tamper resistance to an external system: <https://docs.rundeck.com/docs/administration/security/audit-trail.html>
21. AWX workflow approval nodes: <https://raw.githubusercontent.com/ansible/awx/devel/docs/workflow.md>
22. AAP approval nodes: <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7>
23. Semaphore documentation set, searched in full for approval features
24. Ascender approval nodes: <https://docs.ascender-automation.org/userguide/workflow_templates.html>
25. Rundeck documentation set, searched in full for approval features
26. AWX licensing, which returns an open license for every AWX build: <https://raw.githubusercontent.com/ansible/awx/devel/awx/main/utils/licensing.py>
27. AAP access management and subscription types: <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7>
28. Semaphore team roles and Extended RBAC: <https://semaphoreui.com/docs/user-guide/team>, <https://semaphoreui.com/pricing>
29. Ascender RBAC: <https://docs.ascender-automation.org/userguide/security.html>
30. Rundeck authorization: <https://docs.rundeck.com/docs/administration/security/authorization.html>, <https://www.rundeck.com/community-vs-enterprise>
31. AWX workflow visualizer: <https://raw.githubusercontent.com/ansible/awx/devel/docs/workflow.md>
32. AAP workflow visualizer: <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7>
33. Semaphore pipelines: <https://semaphoreui.com/docs/admin-guide/cicd>
34. Ascender workflow visualizer: <https://docs.ascender-automation.org/userguide/workflow_templates.html>
35. Rundeck workflows and the commercial visualization feature: <https://docs.rundeck.com/docs/manual/jobs/job-workflows.html>, <https://www.rundeck.com/community-vs-enterprise>
36. Semaphore publishes no numeric minimum
37. Ascender install requirements: <https://docs.ciq.com/ascender/1/installation/install>
38. AWX license: <https://github.com/ansible/awx/blob/devel/LICENSE.md>
39. AAP licensing: <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7>
40. Semaphore license: <https://github.com/semaphoreui/semaphore/blob/develop/LICENSE>
41. Ascender license: <https://raw.githubusercontent.com/ctrliq/ascender/main/LICENSE>
42. Rundeck license: <https://github.com/rundeck/rundeck>

This table needs re-verifying on a schedule, quarterly at least. Every product here ships, and a
claim about a competitor that has gone stale costs more than a missing row.
