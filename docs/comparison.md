<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# SwitchTender compared to the field

This page is an honest side-by-side. It states where SwitchTender is ahead, where it is even, and
where it is behind. The first three rows are the ones no other product in the table documents in
full: an approval bound to the content that executes, a ceiling on AI agents that keeps them from
approving their own runs, and evidence a third party verifies offline. Where a vendor documents a
partial answer, the cell says so. Everything after those rows is controller work, where the field
is often even.

Every claim about another product was checked against that vendor's own documentation on
2026-10-08, for AWX 24.6.1, Ansible Automation Platform 2.7-9 with automation orchestrator 2026.8,
Semaphore 2.19.16, Ascender 25.6.2, and Rundeck 6.2.1. All of these ship, and a comparison decays
the day it is written. Check a row against the current release before relying on it, and open an
issue if one has gone stale.

## The field, side by side

Six controllers, fourteen capabilities, every cell checked against that vendor's own documentation.
Sources are numbered and listed at the bottom. A cell reading "not documented" means the vendor's
documentation does not describe the capability, established by searching their whole documentation
set rather than by not finding it.

| Capability | SwitchTender | AWX | AAP | Semaphore | Ascender | Rundeck |
|---|---|---|---|---|---|---|
| Approval bound to the content that runs | Yes: a digest of inventory snapshot, image, commit, and plan, recomputed before execution | Node records a decision, not a digest [21] | Node and orchestrator step record a decision, not a digest; the orchestrator step has a 1-day default window; project signing and OPA policy check content at launch, separately from the approval [22] | Workflow approval node, binding not documented; docs say Pro, pricing lists every tier [23] | Node records a decision, not a digest; jobs record the revision used after the fact [24] | No native approval step in either edition; ServiceNow change steps can check a change request's state [25] |
| Ceiling on AI agents | Yes, capped below admin, cannot approve its own run | No AI feature | Task agents with tool access All, Selected, or None and credentials by reference; approval step needs `approval:decide`, agent eligibility not documented [43] | MCP server with read and write, no ceiling documented [43] | No AI feature documented [43] | ACL policy on a dedicated user's token, set by the operator [43] |
| Evidence a third party verifies offline, without trusting the server | Yes, signed hash chain, Apache-2.0 verifier | No [16] | No; Red Hat directs user activity logs to external providers for non-repudiation [17] | No [18] | No [19] | No [20] |
| Runs without Kubernetes | Yes, one binary | No [1] | Yes, Podman on RHEL [2] | Yes, one binary [3] | No [4] | Yes, needs a JVM [5] |
| Datastore | SQLite, Postgres optional (creating a new schema is Team, opening an existing one is never gated) | PostgreSQL 15 [1] | PostgreSQL 15 [2] | SQLite, MySQL or Postgres [3] | PostgreSQL [4] | H2 by default; MySQL, MariaDB, Postgres, MSSQL, Oracle [5] |
| Other runtime services | None | Redis, Receptor [1] | Redis, Receptor [2] | None single-node, Redis for HA [3] | Valkey, Receptor [4] | None required [5] |
| Tools it executes | Ansible, Terraform, OpenTofu, Bash, PowerShell, Python, Go | Ansible only [6] | Ansible playbooks and rulebooks; REST and AI agent steps in automation orchestrator, a separately installed component [7] | Ansible, Terraform, OpenTofu, Terragrunt, PowerShell, Bash, Python, plus any executable as a custom app [8] | Ansible only [9] | Ansible plus generic scripts, no Terraform step [10] |
| Live per-host status | Host-by-task matrix | Host status bar, per-event drill-down [11] | Host status bar, per-event drill-down [12] | Timestamped log; per-host OK and failed counts for Ansible; execution summary is Pro [13] | Host status bar, per-event drill-down [14] | Per-node, per-step monitor [15] |
| Tamper-evident audit | Hash-chained, signed, anchored | No [16] | No; Red Hat directs user activity logs to external providers for non-repudiation [17] | No; activity log with syslog and webhook export in paid tiers [18] | No; activity stream forwarded to log aggregators, Ledger change diffs exported as CSV or PDF [19] | No; activity log in all editions, audit trail commercial, nothing hashed or signed [20] |
| RBAC in the free tier | Full, plus per-object grants | Yes, no paid tier exists [26] | Yes, subscription required for the product [27] | Four fixed project roles; custom roles are Enterprise [28] | Yes [29] | Yes, and finely grained; GUI editor is commercial [30] |
| Drift report and reconcile | Yes, from a scheduled dry run, with one-click reconcile on Team | Not documented | Not documented | Not documented | Ascender Pro through Ledger, described by the vendor as watching baselines between runs; no reconcile documented | Not documented |
| Workflows | DAG with a drag-and-drop editor | Visual editor [31] | Visual editor [32] | DAG with conditional edges, approval, delay, and note nodes, graphical editor, shipped in 2.19 as beta [33] | Visual editor [34] | Engine yes; visualization is commercial [35] |
| Minimum published footprint | One binary, one database | Not published as a single figure | 16 GB RAM, 4 CPU, 60 GB, 3000 IOPS; automation orchestrator needs OpenShift 4.14 or later [2] | 512 MB RAM, 1 CPU core [36] | 2 CPU, 4 GB RAM without Pro, 8 GB with, 20 GB [37] | 8 GB RAM, 2 CPU [5] |
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
| Run view | A structured host-by-task matrix with per-task drill-down, painted live over Server-Sent Events. | A host status bar with per-event drill-down, which is a summary rather than a matrix. | A timestamped log with per-host OK and failed counts for Ansible. |
| Job splitting | Shards balanced by each host's measured past duration, with only the failed shards retried. | Job slicing, round-robin. | Not available. |
| Pipelines | A dependency graph with parallel branches, per-step retries, and typed set_stats outputs passed to dependents. | Visual workflows. | A DAG of task templates with conditional edges. |
| Visual workflow editor | A drag-and-drop canvas at Workflows builds the dependency graph in the browser: draft persistence, undo, keyboard editing, cycle refusal, and a pan, zoom, and fit-to-view viewport, on the same DAG engine the API uses. | A drag-and-drop editor. | A graphical editor, shipped in 2.19 as beta. |
| Fleet memory | Flaky-host detection, outcome sparklines, per-host history, and task duration trends across runs. | Per-host job summaries, with no flaky-host detection or duration trends. | Per-task output; inventory change history is on its roadmap for 2.22. |
| Distributed workers | Store leasing, where the same single binary adds capacity, held together by leases and a janitor that requeues a crashed worker's runs. | A Receptor mesh. | Global runners are free; project-isolated runners and tag routing are Pro. |
| Instance groups | A queue pins work at the run, template, or inventory level, most specific wins, so jobs land on the right worker group (Team). | Instance groups. | Runner tag routing, Pro. |
| High availability | Active-active replicas on PostgreSQL behind any load balancer: store-claimed work, compare-and-set schedules and approvals, automatic failover through stale-lease reclaim, proven by a two-replica integration suite. | Via Kubernetes replicas. | With Redis [3]. |
| Provable audit | A tamper-evident SHA-256 hash chain, exported as a signed LoomSeal bundle and verified offline by an open verifier. | An activity stream. | An activity log of acting user, affected object, action, IP, and user agent; syslog over TLS and Splunk HEC webhook in paid tiers; nothing hashed or signed. |
| Migration in | One command imports an AWX, Semaphore, Chef, Puppet, Rundeck, or Jenkins export, a Rundeck project archive, or a plain crontab. AWX and Semaphore bring projects, inventories, and credential shells across with their templates, surveys, and schedules; Rundeck and Jenkins bring templates, surveys, and schedules against an inventory you name, and a Rundeck project archive brings one project as well when its source control configuration names a repository this can reach; a crontab brings schedules. Neither Rundeck artifact brings an inventory. See [what each source brings over](migration.md#what-each-source-brings-over). | Not applicable. | Not applicable. |
| Drift detection | A dry run reports what has diverged from the desired state, across Ansible hosts and Terraform working directories, with a one-click approval-gated reconcile to fix it. | No. | No. |
| Directory-driven roles | A directory or token group sets a user's role on every sign-in, over LDAP, SAML, or a bearer JWT, on Pro. OIDC provisions every account at one configurable default role instead. | Organization mapping, complex. | Group mapping is listed as Enterprise on its pricing page and targeted at 2.20 on its roadmap; its team docs say it is not currently supported. |
| Notification channels | Eleven server-wide: webhook, email, Slack, Mattermost, Rocket.Chat, Discord, Teams, ntfy, PagerDuty, Grafana, and Twilio. All eleven take a per-template target, so a team pages its own channel; a Twilio or email target names only a recipient and sends through the server-held account, so the account secret never lives in a template. A named target is defined once and attached to templates, schedules, projects, and organizations, the way AWX attaches a notification template. | A similar set plus IRC, without Discord or ntfy, attached to organizations, projects, job templates, workflows, and each inventory source. | Email, Slack, Teams, and others in Community. |

## Where they are even

| Capability | Notes |
|------------|-------|
| Multiple runtimes | SwitchTender runs Ansible, Terraform, OpenTofu, Bash, PowerShell, Python, and Go. Ansible check mode and a Terraform or OpenTofu plan preview a change, and a Bash, PowerShell, Python, or Go dry run only checks the script without running it, which previews nothing. AWX is Ansible-only. Semaphore runs Ansible, Terraform, OpenTofu, Terragrunt, PowerShell, Bash, and Python, and a custom app registers any executable, with no built-in Go app. |
| Container execution environments | SwitchTender pins an image on a template, a run, or a project, most specific wins, with private-registry pulls, opt-in behind a flag. AWX attaches execution environments to job templates. Semaphore can run each task in a Docker container on Pro and as a Kubernetes pod on Enterprise; Community runs tasks natively. |
| Access control | SwitchTender has global roles plus organizations, teams, and per-object read, use, and manage grants, all in the source-available core. AWX has mature organization RBAC. Semaphore gates advanced RBAC (custom roles, identity group mapping) behind its Enterprise tier. |
| Credentials | Sealed with AES-256-GCM under a key derived by Argon2id, decrypted only at execution into the run's environment or a temporary file created mode 0600, kept off the command line either way, and deleted when the run ends. Fifteen credential kinds, kubeconfig among them, and custom types that inject environment variables, extra vars, and templated files the way AWX's do. Four federated kinds store nothing at all: each run gets a short-lived OpenID Connect token SwitchTender signs, exchanged for AWS, Google Cloud, or Azure access, so no durable cloud key is kept. Eleven sources, nine of them external secret managers: HashiCorp Vault static and dynamic, AWS Secrets Manager and STS, Azure Key Vault, GCP Secret Manager, CyberArk Conjur and CCP, and 1Password. Several credentials of different kinds attach to one run. AWX matches this through credential plugins. Semaphore's built-in key store holds an SSH key or a username and password, and secrets can instead live in an external HashiCorp Vault, OpenBao, AWS Secrets Manager, Azure Key Vault, or Devolutions Server. |
| Scheduling | All three schedule runs. SwitchTender takes a cron expression or an RFC 5545 recurrence rule, the form AWX uses, with a timezone, and claims each fire so two servers do not double-fire. |
| Surveys and prompts | All three collect typed values at launch. SwitchTender and AWX also take a secret answer, sealed at rest and never shown back. |
| Inbound webhooks | All three launch on a git push. |
| Planning and applying from a pull request | SwitchTender plans a GitHub or GitLab pull request and comments the plan with the decision its apply would get. A person whose forge account is linked can comment `/switchtender apply` to approve and apply that plan, with the same role, grants, and separation of duties as the queue, the way Atlantis applies from a comment. A rule that requires the approver's reason still needs the decision made in SwitchTender, since a comment carries none. |
| Metrics | SwitchTender exposes Prometheus metrics for scraping. |
| Directory sign-in | SwitchTender, AWX, and Semaphore all sign in with LDAP and OpenID Connect. SwitchTender prices its Pro tier at $500 a year, flat per organization, with five approval policies and SSO. Semaphore Pro starts at $20 a month or $199 a year for 50 managed nodes and 500 managed resources, configurable by node count with an unlimited tier, and one license covers up to three instances. AWX ships SSO free inside a Kubernetes-sized install. |
| Paid tiers | SwitchTender: Community free with one plain rule, Pro $500 a year to 500 hosts (SSO, five approval policies, and email support), Team $9,900 to $54,000 by fleet band (full policy engine, PostgreSQL active-active HA, distributed workers, drift reconcile, change register), Enterprise from $75,000. AWX: free, no paid tier; the paid product is Ansible Automation Platform. Semaphore: Community free, Pro from $199 a year at 50 managed nodes, Enterprise by contract. Ascender: core free; Ascender Pro $100 per node per year Standard or $150 Premium, with no node minimum on the pricing page and no minimum after an initial 100-node purchase on the compare page, as of 2026-10-08. |

## Where SwitchTender is behind

| Capability | Status |
|------------|--------|
| Maturity | AWX and Semaphore have years of production use and large communities. Every SwitchTender release is proven end to end on real machines before it ships, as [the supertest](supertest.md) sets out. AWX's years now cut both ways: its last release was July 2024, and [mid-refactor development builds have moved external authentication out of core](https://forum.ansible.com/t/awx-modernization-moving-forward/45134) into a shared library. |
| Policy language | Approval policies are YAML or, on Team, Rego, the Open Policy Agent language, so OPA rules port. A Sentinel rule does not, because Sentinel is proprietary to HashiCorp, and has to be rewritten in Rego. |
| Some AWX forms | An approval node joined by an "always" edge, injector templates that use Jinja beyond plain substitution, and IRC notifications do not come across. Neither does a workflow with a node that runs another workflow or a sync, a path taken on failure, nodes from more than one project, or an always path beside a success path on one node, nor a project that is not on git, nor a team. The import report names each one rather than bringing it over changed. |

## The short version

SwitchTender is the only product in this table whose approval is bound to a digest of the content
that runs and whose evidence a third party verifies offline. Its AI agents cannot approve their own
runs, which no other product in the table documents. Outside the table, Spacelift's confirmation
accepts the plan generated in the planning phase for infrastructure-as-code runs; it does not cover
an inventory snapshot or a container image. The controller work underneath, one binary, a live
matrix, balanced splits, fleet memory, a visual DAG editor, and a one-command import from six
tools, ships in the same binary. It is younger, and its integration catalog is smaller. Both gaps
are being closed on purpose.

The [reliability](reliability.md) page details how runs execute under load: bounded workers, splits
balanced by measured duration, coordinated pipeline failure, crash recovery through leases, and a
store that stays consistent while a fleet writes to it.

## Sources

Fetched and read on 2026-10-08.

1. AWX install and operator defaults: <https://github.com/ansible/awx/blob/devel/INSTALL.md>, <https://raw.githubusercontent.com/ansible/awx-operator/devel/roles/installer/defaults/main.yml>
2. AAP deployment models and system requirements: <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7>, <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7/install-ref_cont_aap_system_requirements>, <https://docs.redhat.com/en/documentation/automation_orchestrator/2026.8/install-automation_orchestrator_system_requirements>
3. Semaphore introduction and config: <https://semaphoreui.com/docs/introduction/what-is-semaphore>, <https://github.com/semaphoreui/semaphore/blob/develop/util/config.go>
4. Ascender installer and requirements: <https://github.com/ctrliq/ascender-install>, <https://raw.githubusercontent.com/ctrliq/ascender/main/requirements/requirements.txt>, <https://raw.githubusercontent.com/ctrliq/ascender-operator/devel/roles/installer/defaults/main.yml>
5. Rundeck system requirements: <https://docs.rundeck.com/docs/administration/install/system-requirements.html>
6. AWX job types: <https://raw.githubusercontent.com/ansible/awx/devel/awx/main/models/base.py>
7. AAP job template job types, and the REST and AI agent steps of automation orchestrator: <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7>, <https://docs.redhat.com/en/documentation/automation_orchestrator/2026.8>
8. Semaphore supported tools: <https://semaphoreui.com/docs/user-guide/apps>
9. Ascender job templates: <https://docs.ascender-automation.org/userguide/job_templates.html>
10. Rundeck distributed plugins: <https://docs.rundeck.com/docs/manual/plugins/full-list.html>
11. AWX job output and host status bar: <https://raw.githubusercontent.com/ansible/awx/24.6.1/awx/ui/src/screens/Job/JobOutput/shared/HostStatusBar.js>
12. AAP playbook run output: <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7>
13. Semaphore task view: <https://semaphoreui.com/docs/user-guide/tasks>
14. Ascender jobs view: <https://docs.ascender-automation.org/userguide/jobs.html>
15. Rundeck execution monitor: <https://docs.rundeck.com/docs/manual/07-executions.html>
16. AWX activity stream model, no hash or signature fields: <https://raw.githubusercontent.com/ansible/awx/devel/awx/main/models/activity_stream.py>
17. AAP activity stream, and Red Hat's guidance to use external log providers for non-repudiation: <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7/observe-assembly_controller_activity_stream>, <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7/secure-con_logging_log_capture>
18. Semaphore activity and audit logs: <https://semaphoreui.com/docs/admin-guide/logs>
19. Ascender activity stream and logging: <https://docs.ascender-automation.org/administration/logging.html>
20. Rundeck audit trail, which delegates tamper resistance to an external system: <https://docs.rundeck.com/docs/administration/security/audit-trail.html>
21. AWX workflow approval nodes: <https://raw.githubusercontent.com/ansible/awx/devel/docs/workflow.md>
22. AAP approval nodes, orchestrator approval steps, project signing, and policy as code: <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7/develop-ref_controller_approval_nodes>, <https://docs.redhat.com/en/documentation/automation_orchestrator/2026.8/develop-understand_workflow_approvals>, <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.5/html/using_automation_execution/assembly-controller-project-signing>, <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7/integrate-assembly_controller_pac>
23. Semaphore workflow approval node, and the editions and pricing pages that disagree on its tier: <https://semaphoreui.com/docs/user-guide/workflows>, <https://semaphoreui.com/docs/editions>, <https://semaphoreui.com/pricing>
24. Ascender approval nodes: <https://docs.ascender-automation.org/userguide/workflow_templates.html>
25. Rundeck conditional logic, ServiceNow change steps, and the edition comparison, none of which
    documents a native approval step: <https://docs.rundeck.com/docs/manual/jobs/conditional-logic.html>, <https://docs.rundeck.com/docs/manual/jobs/job-plugins/workflow-steps/servicenow.html>, <https://www.rundeck.com/community-vs-enterprise>
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
36. Semaphore minimum footprint: <https://semaphoreui.com/vs/awx>
37. Ascender install requirements: <https://docs.ciq.com/ascender/1/installation/install>
38. AWX license: <https://github.com/ansible/awx/blob/devel/LICENSE.md>
39. AAP licensing: <https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.7>
40. Semaphore license: <https://github.com/semaphoreui/semaphore/blob/develop/LICENSE>
41. Ascender license: <https://raw.githubusercontent.com/ctrliq/ascender/main/LICENSE>
42. Rundeck license: <https://github.com/rundeck/rundeck>
43. AI agent features and the limits each vendor documents. AAP orchestrator agentic steps and approvals: <https://docs.redhat.com/en/documentation/automation_orchestrator/2026.8/discover-agentic_steps_in_a_workflow>, <https://docs.redhat.com/en/documentation/automation_orchestrator/2026.8/develop-understand_workflow_approvals>. Semaphore MCP server: <https://semaphoreui.com/>, <https://semaphoreui.com/pricing>. Rundeck MCP best practices: <https://docs.rundeck.com/docs/mcp/best-practices.html>. Ascender product and compare pages, which describe no AI feature: <https://ciq.com/products/ascender>, <https://ciq.com/compare/ascender-pro-vs-ansible>

Every product here ships, so treat this as a snapshot taken on the date above rather than a standing
claim. A row that has gone stale is a bug: open an issue and it gets corrected.
