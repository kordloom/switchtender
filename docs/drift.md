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

See also the [FAQ](faq.md) and the [tutorials](tutorials.md).
