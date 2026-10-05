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
`plan` with a detailed exit code, which distinguishes a clean plan from one with pending changes. A dry
run that finds changes is recorded as drift keyed on its working directory rather than a host, with the
plan's changed-resource count, and one that finds none records the directory in sync, so the Drift
page shows where the directory stands after its latest check rather than the last drift any check saw.
A check that fails records nothing. Bash, Python, PowerShell, and Go have no desired-state check, so
they do not report drift.

A check run is only a no-change check when its playbook lets it be one. A play, block, task, role,
or include that sets `check_mode: false` runs for real under `--check`, and what it changes is
counted on the Drift page beside what the rest would change. The gate does not treat such a check
as a preview: a rule that excludes dry runs still holds it, and the run records which tasks forced
it. A plan is the same when its configuration declares an `external` data source, whose program
runs while the plan does: the run records the data source by its address, and the rule holds it.
See [dry runs and `exclude_dry_run`](concepts.md#dry-runs-and-exclude-dry-run).

## Changes made around the gate

A change made outside SwitchTender, in an SSH session or a cloud console, never reaches the audit
chain, because it never passed through the gate. It still shows up here. If it touched anything your
automation asserts, the next scheduled check shows that host or Terraform directory as drifted, and
the reconcile that puts it back is held for approval like any other change. Drift names what
diverged, not who changed it, so pair it with the hosts' own logs when the question is who.
[The side door](side-door.md) covers the rest: how agents never get one, and how people stop
needing one.

## From the API

    curl -s -H "Authorization: Bearer $ST_TOKEN" localhost:8080/v1/drift

Each target carries its changed count, the check run id, and when it was checked, worst drift first. A
target is an Ansible host or a Terraform working directory.

## Reconcile a drifted target

A drifted row on the Drift page carries a Propose reconcile button. It builds the fix for you from the
check that observed the drift, run for real instead of as a check: an Ansible check reruns its playbook
limited to the drifted host, applying exactly the divergent tasks, and a Terraform or OpenTofu check's
apply carries out the plan file the check saved. The construction is deterministic. No model builds
it.

A Terraform or OpenTofu check keeps the plan it saved, sealed, when it finds drift, and the reconcile
carries that plan with its digest bound into the approval, so the approver releases the plan shown
with the check and the apply runs `apply <planfile>` rather than planning again. A later check of the
same working directory replaces the kept plan, and a later check that finds the directory in sync
takes it off the page's drifted rows. A check that kept none, because it ran before plans were kept,
cannot be reconciled from and the request is refused with `409`: run the check again, then propose
the reconcile. If the state changed after the check made its
plan, the tool refuses the stale plan when the approved reconcile runs, applies nothing, and the run
says to run the check again and propose the reconcile from it. The
[Terraform page](tool-terraform.md#a-gated-apply-carries-out-the-approved-plan) has the details.

The proposal never starts on its own. It is created held for approval, so an approver reviews it and
releases or rejects it, and the audit trail records that a machine proposed it and who decided. When
an AI provider is configured, the Explain button on the held proposal summarizes what drifted and
what approving will change, from the check run's masked events.

The same action is one request, for an operator token:

    curl -s -X POST localhost:8080/v1/drift/reconcile \
      -H "Authorization: Bearer $ST_TOKEN" -d '{"host": "web01"}'

The `host` field names the drifted target, an Ansible host or a Terraform working directory.

Detecting drift is free. One-click reconcile, both the button and this request, needs a Team license,
and is refused rather than ignored without one. Without it, drift is still reported and still fixed
by launching the run yourself.

See also the [FAQ](faq.md) and the [tutorials](tutorials.md).
