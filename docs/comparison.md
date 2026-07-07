# Yardmaster compared to AWX and Semaphore

This page is an honest side-by-side. It states where Yardmaster is ahead, where it is even, and
where it is behind, because credibility comes from being straight about all three. Details of AWX
and Semaphore were current as of mid-2026; verify them against their latest releases before relying
on them.

## Where Yardmaster is ahead

| Capability | Yardmaster | AWX | Semaphore |
|------------|------------|-----|-----------|
| Deployment | One binary, SQLite by default, PostgreSQL optional | Kubernetes plus PostgreSQL, Redis, and Receptor | One binary |
| Run view | Structured host-by-task matrix with per-task drill-down, painted live over Server-Sent Events | Text log stream | Text log stream |
| Job splitting | Shards balanced by each host's measured past duration, with retry of only the failed shards | Job slicing, round-robin | Not available |
| Pipelines | Dependency graph with parallel branches, per-step retries, and typed `set_stats` outputs passed to dependents | Visual workflows | Limited task chaining |
| Fleet memory | Flaky-host detection, outcome sparklines, per-host history, and task duration trends across runs | Not available | Not available |
| Distributed workers | Store leasing, the same single binary adds capacity, with leases and a janitor | Receptor mesh | Runners, a paid tier |
| Per-run provenance | Every run records the exact commit it executed | Partial | Partial |
| Migration in | One command imports an AWX or Semaphore export | Not applicable | Not applicable |

## Where they are even

| Capability | Notes |
|------------|-------|
| Container execution environments | Yardmaster runs a playbook inside a pinned image, opt-in behind a flag. AWX makes this a core feature; Semaphore favors native runtimes instead. |
| Access control | Yardmaster has global roles plus per-object grants and teams. AWX has mature organization RBAC; Semaphore gates RBAC behind its Enterprise tier. |
| Credentials | All three store secrets encrypted; Yardmaster decrypts only at execution into a temporary file and wipes it after. |
| Scheduling | All three schedule runs; Yardmaster uses cron with highly available claiming so two servers do not double-fire. |
| Surveys and prompts | All three collect typed values at launch. |
| Inbound webhooks | All three launch on a git push. |
| Metrics and audit | Yardmaster exposes Prometheus metrics and an audit trail of every mutation. |

## Where Yardmaster is behind

| Capability | Status |
|------------|--------|
| Multiple runtimes | Ansible only today. Semaphore also runs Terraform, OpenTofu, and shell. A pluggable runtime is on the roadmap. |
| SSO and LDAP | Not yet. AWX has it; Semaphore gates it behind Enterprise. |
| Visual workflow editor | Yardmaster defines pipelines through the API and shows them in the UI; AWX has a drag-and-drop editor. |
| Notification breadth | Webhook and email today, against a wider set of integrations in AWX. |
| Maturity | AWX and Semaphore have years of production use and large communities. Yardmaster is young. |

## The short version

Yardmaster wins on the axes that make AWX painful and Semaphore ordinary: no heavy control plane to
deploy, runs you can read as structure instead of scrollback, job splitting that actually balances,
a memory of how every host behaves over time, and a one-command path off either incumbent. It is
behind on runtime breadth and the enterprise identity features, and it is younger. The gap that
remains is being closed on purpose.
