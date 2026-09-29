<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Sample evidence pack

This is what the change-management evidence a reviewer samples looks like, produced by
`switchtender audit report` over a real chain. It is the artifact you hand an auditor or a
prospect's security team. See [Compliance mapping](compliance.md) for which control clauses each
column answers.

The instance below uses fictional accounts and playbook names and touches no real infrastructure, so
nothing here is redacted; on your own instance the same report carries your real changes. The chain
it was rendered from verifies and is anchored to a public timestamp authority, so the header reads:

> The chain verifies and carries 1 anchor(s) fixing it outside this install.

## The period at a glance

| Changes | Approved | Rejected | Failed |
|--------:|---------:|---------:|-------:|
| 6 | 4 | 1 | 2 |

## Every change in the period

| When (UTC) | Change | Actor | Source | Risk | Held by | Decision | Outcome |
|---|---|---|---|---|---|---|---|
| 2026-08-10 11:50 | ansible deploy/site.yml | alice-deploy | api | medium | gate-all-production-change | Approved by platform-admin #10 | failed |
| 2026-08-10 11:50 | ansible tls/rotate-certs.yml | alice-deploy | api | medium | gate-all-production-change | Approved by platform-admin #11 | failed |
| 2026-08-10 11:50 | ansible web/restart-nginx.yml | alice-deploy | api | medium | gate-all-production-change | Rejected by platform-admin #12 | rejected |
| 2026-08-10 11:50 | ansible db/apply-migrations.yml | alice-deploy | api | medium | gate-all-production-change | &mdash; | pending&#95;approval |
| 2026-08-10 11:51 | bash echo deploy complete | alice-deploy | api | low | gate-all-production-change | Approved by platform-admin #15 | succeeded |
| 2026-08-10 11:51 | bash echo cache purged on edge fleet | alice-deploy | api | low | gate-all-production-change | Approved by platform-admin #16 | succeeded |

## How to read it

- **Held by** names the approval policy that stopped every change, so a reviewer sees the gate was in
  force, not that these six happened to be caught.
- **Decision** is the chain-recorded approval or rejection, with the approver and the entry number
  the decision landed at (`#10`), so it is redeemable against the chain rather than a field the
  server asserts. `alice-deploy`, an operator, submitted every change; `platform-admin` decided each
  one, which is the separation of requester and approver a change-management control expects.
- **Outcome** is honest, not curated. Two approved changes then failed at execution, one change was
  rejected and never ran, one is still awaiting a decision, and two succeeded. An audit trail that
  only ever showed green would be the one to distrust.
- Nothing in the row exposes a secret. The chain also commits to a redacted digest of each
  change's payload, so the record proves what changed without carrying the credentials the
  change used.

## Produce your own

```
# a period's change register
switchtender audit report --from 2026-07-01 --to 2026-08-01 --out evidence.html

# one change in depth: spec, approvals, per-host outcomes, receipts, anchors
switchtender audit run <run-id> --out run-dossier.html

# fix the chain in time so a lost tail is detectable, then re-report
switchtender audit anchor
```

The Assurance service (design partners) assembles these on a cadence, mapped to your controls and written
to an archive, so the sample a review asks for exists before anyone asks. The report itself, the
free `--evidence-dir` scheduling, and verifying it all offline stay free in the core.
