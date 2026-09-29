<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Drift detection

Drift is when your infrastructure no longer matches the state your automation asserts. SwitchTender
detects it from a dry run and shows it on the Drift page, so divergence surfaces before the next real
run.

## How it works

A dry run reports what would change without changing anything. For Ansible that is check mode, which
reports task by task, per host, what would change. For Terraform and OpenTofu it is a plan, which
reports the resources that would change in a working directory. Either way, a dry run that would
change something means the target has diverged from the desired state. The Drift page shows each
target's most recent check: how much would change now, when it was checked, and the run that observed
it. A target whose latest check would change nothing is in sync.

Because drift comes straight from the run, it needs no separate agent and no extra setup. Schedule a
dry run on a cadence and the Drift page stays current.

## What counts

Drift comes from a dry run that has a no-change check. Ansible's `--check` reports, host by host, which
tasks would change, and each host's changed count feeds the Drift page. Terraform and OpenTofu run
`plan` with a detailed exit code, which distinguishes a clean plan from one with pending changes; a dry
run that finds changes is recorded as drift keyed on its working directory rather than a host, with the
plan's changed-resource count. Bash, Python, PowerShell, and Go have no desired-state check, so they do
not report drift.

## From the API

    curl -s localhost:8080/v1/drift

Each target carries its changed count, the check run id, and when it was checked, worst drift first. A
target is an Ansible host or a Terraform working directory.

## Reconcile a drifted target

A drifted row on the Drift page carries a Propose reconcile button. It builds the fix for you from the
check that observed the drift, run for real instead of as a check: an Ansible check reruns its playbook
limited to the drifted host, applying exactly the divergent tasks, and a Terraform or OpenTofu check
applies its working directory. The construction is deterministic. No model builds it.

The proposal never starts on its own. It is created held for approval, so an approver reviews it and
releases or rejects it, and the audit trail records that a machine proposed it and who decided. When
an AI provider is configured, the Explain button on the held proposal summarizes what drifted and
what approving will change, from the check run's masked events.

The same action is one request, for an operator token:

    curl -s -X POST localhost:8080/v1/drift/reconcile -d '{"host": "web01"}'

The `host` field names the drifted target, an Ansible host or a Terraform working directory.

See also the [FAQ](faq.md) and the [tutorials](tutorials.md).
