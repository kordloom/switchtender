<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Drift detection

Drift is when a host no longer matches the state a playbook asserts. Yardmaster detects it from a dry
run and shows it per host on the Drift page, so divergence surfaces before the next real run.

## How it works

A dry run executes in check mode, which reports what a task would change without changing it. A task
that would change means the host has diverged from the desired state. Yardmaster records each host's
changed count from its checks, and the Drift page shows each host's most recent check: how many tasks
would change now, when it was checked, and the run that observed it. A host whose latest check would
change nothing is in sync.

Because drift comes from the same structured events as every run, it needs no separate agent and no
extra setup. Schedule a dry run of a playbook on a cadence and the Drift page stays current.

## What counts

Drift is defined for tools that assert a desired state and support a no-change check: Ansible with
`--check` and Terraform with a plan. Bash, Python, and Go have no desired state, so they do not report
drift.

## From the API

    curl -s localhost:8080/drift

Each host carries its drifted task count, the check run id, and when it was checked, worst drift first.

## Reconcile a drifted host

A drifted row on the Drift page carries a Propose reconcile button. It builds the fix for you: the
same playbook, inventory, and credentials as the check that observed the drift, limited to that host
and run for real instead of in check mode. The construction is deterministic. No model builds it.

The proposal never starts on its own. It is created held for approval, so an approver reviews it and
releases or rejects it, and the audit trail records that a machine proposed it and who decided. When
an AI provider is configured, the Explain button on the held proposal summarizes what drifted and
what approving will change, from the check run's masked events.

The same action is one request, for an operator token:

    curl -s -X POST localhost:8080/drift/reconcile -d '{"host": "web01"}'

Reconcile is defined for Ansible drift, where limiting the playbook to the drifted host applies
exactly the divergent tasks.

See also the [FAQ](faq.md) and the [tutorials](tutorials.md).
