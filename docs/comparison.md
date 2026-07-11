<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Yardmaster compared to AWX and Semaphore

This page is an honest side-by-side. It states where Yardmaster is ahead, where it is even, and
where it is behind, because credibility comes from being straight about all three. Details of AWX
and Semaphore were current as of mid-2026. Verify them against their latest releases before relying
on them.

## Where Yardmaster is ahead

| Capability | Yardmaster | AWX | Semaphore |
|------------|------------|-----|-----------|
| Deployment | One binary, SQLite by default and PostgreSQL optional. | Kubernetes plus PostgreSQL, Redis, and Receptor. | One binary. |
| Run view | A structured host-by-task matrix with per-task drill-down, painted live over Server-Sent Events. | A text log stream. | A text log stream. |
| Job splitting | Shards balanced by each host's measured past duration, with only the failed shards retried. | Job slicing, round-robin. | Not available. |
| Pipelines | A dependency graph with parallel branches, per-step retries, and typed set_stats outputs passed to dependents. | Visual workflows. | Limited task chaining. |
| Fleet memory | Flaky-host detection, outcome sparklines, per-host history, and task duration trends across runs. | Not available. | Not available. |
| Distributed workers | Store leasing, where the same single binary adds capacity, held together by leases and a janitor. | A Receptor mesh. | Runners, in a paid tier. |
| Per-run provenance | Every run records the exact commit it executed. | Partial. | Partial. |
| Migration in | One command imports an AWX or Semaphore export. | Not applicable. | Not applicable. |
| Drift detection | A dry run reports which hosts have diverged from the desired state, shown per host before the next real run. | No. | No. |
| Directory-driven roles | A directory or JWT group sets a user's role on every sign-in, over LDAP, OIDC, or a bearer JWT. | Organization mapping, complex. | No. Every user is assigned a role by hand. |

## Where they are even

| Capability | Notes |
|------------|-------|
| Multiple runtimes | Yardmaster runs Ansible, Bash, Terraform, Python, and Go, each with a dry run. AWX is Ansible-only. Semaphore also runs Ansible, plus Terraform, OpenTofu, and shell, but not Python or Go. |
| Container execution environments | Yardmaster runs a playbook inside a pinned image, opt-in behind a flag. AWX makes this a core feature. Semaphore favors native runtimes instead. |
| Access control | Yardmaster has global roles plus per-object grants and teams. AWX has mature organization RBAC. Semaphore gates RBAC behind its Enterprise tier. |
| Credentials | All three store secrets encrypted. Yardmaster decrypts only at execution into a temporary file and wipes it after. |
| Scheduling | All three schedule runs. Yardmaster uses cron with highly available claiming so two servers do not double-fire. |
| Surveys and prompts | All three collect typed values at launch. |
| Inbound webhooks | All three launch on a git push. |
| Metrics and audit | Yardmaster exposes Prometheus metrics and an audit trail of every mutation. |
| Directory sign-in | Yardmaster, AWX, and Semaphore all sign in with LDAP and OpenID Connect. |

## Where Yardmaster is behind

| Capability | Status |
|------------|--------|
| Visual workflow editor | Yardmaster defines pipelines through the API and shows them in the UI. AWX has a drag-and-drop editor, and Semaphore has one on its roadmap. |
| Notification breadth | Webhook and email today, against a wider set of integrations in AWX. |
| Maturity | AWX and Semaphore have years of production use and large communities. Yardmaster is young. |

## The short version

Yardmaster wins on the axes that make AWX painful and Semaphore ordinary: no heavy control plane to
deploy, runs you can read as structure instead of scrollback, job splitting that actually balances,
a memory of how every host behaves over time, and a one-command path off either incumbent. It is
behind on runtime breadth and the enterprise identity features, and it is younger. The gap that
remains is being closed on purpose.
