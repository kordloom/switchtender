<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Compliance mapping

This maps what SwitchTender records to the change-management and audit-control clauses an assessor
asks about. It is a reference for the person answering the audit, not a claim of certification: the
tool produces the evidence, and your assessor decides whether your program satisfies the control.

Every mapping below points at the tamper-evident chain and the change register
`switchtender audit report` renders from it, all in the free core. Rows marked next release describe
the actor-identity and content-digest fields that land in the upcoming release; everything else ships
in the current download. Nothing here is a paid feature. The paid Governed tier (early access) adds
control-mapped evidence packs and auditor-facing attestation reports, assembled for you on the same
cadence the free `--evidence-dir` registers already run on.

## What the record holds

Every authenticated change is one entry in a SHA-256 hash chain. Each entry commits to:

| Field | What it is |
|-------|------------|
| Actor | Who acted. From the next release, `actor_type` joins it: how they authenticated, a session, a token, or the command line. |
| On behalf of | Next release. For a token bound to an account, the account whose authority it used, so an agent's change is attributable to both the token and the operator behind it. |
| Method and path | The operation performed. |
| Content digest | Next release. A hash of the change payload with secret fields redacted, so the record proves what a change contained, not only that a call was made, without exposing the secret. |
| Time, sequence, link | When it happened and its tamper-evident position in the chain. |

The change register (`switchtender audit report --from --to`) renders these per change with the
run's actor, the approval decision recorded over it, the risk grade, and the outcome. The run dossier
(`switchtender audit run <id>`) is the per-change deep view: spec, approvals, per-host results, and
the chain receipts and anchors behind them.

## SOC 2 CC8.1 (change management)

CC8.1 asks that changes are authorized, tested, and tracked before they reach production.

| The control expects | Where SwitchTender shows it |
|---------------------|-----------------------------|
| Changes are authorized before they take effect | The approval decision on each register row, recorded in the chain; an empty approval policy holds every run for a person, and approve is admin-only so an operator or agent cannot release its own change. |
| Change activity is tracked and attributable | The actor on every entry, joined by `actor_type` and `on_behalf_of` in the next release; the change register lists every change in the period. |
| The change record is complete and unaltered | The hash chain: a change that cannot be recorded is refused rather than made, and altering or deleting an entry breaks verification, provable offline with `switchtender audit verify`. |
| A dry run or test preceded the change | A run's dossier records whether it ran in the tool's no-change mode; drift is shown from a dry run before the fix is built. |
| Segregation between requester and approver | Approve and reject are admin-only; the operator or agent that submits cannot approve. |

## ISO/IEC 27001:2022 A.8.32 (change management)

A.8.32 asks that changes to information processing facilities are controlled through a formal process.

| The control expects | Where SwitchTender shows it |
|---------------------|-----------------------------|
| Changes follow a defined, enforced procedure | Policy-as-code gates changes on tool, command, or target; a gated change holds automatically. |
| Changes are documented | The change register for the period and the per-change dossier, both self-contained and re-verifiable. |
| Changes are approved by an appropriate authority | The chain-recorded approve or reject decision, with the approver's identity, on each change. |
| Changes can be traced and, where needed, reversed | Each run records what fired it and what it was a rerun or drift-fix of; the register and dossier trace the lineage. |
| The change log is protected from tampering | The hash chain, its offline verification, and an RFC 3161 anchor that bounds how much history could vanish unnoticed. |

## HIPAA 45 CFR 164.312(b) (audit controls)

164.312(b) requires mechanisms that record and examine activity in systems that handle electronic
protected health information.

| The control expects | Where SwitchTender shows it |
|---------------------|-----------------------------|
| Activity in the system is recorded | Every authenticated mutation is an entry in the chain, over the API and the command line alike. |
| The record cannot be silently altered | The SHA-256 chain and its offline verification; a change that cannot be recorded is refused. |
| The record can be examined | The change register and per-change dossier render as self-contained HTML a reviewer reads without tooling. |
| The record is retained and its integrity demonstrable over a period | Scheduled evidence packs write a register per period to an archive, and an anchor fixes the chain in time so a lost tail is detectable. |
| Access to the audit trail is itself controlled | Reading the trail and the run evidence is an admin-role operation, not open to every viewer. |

## The boundary, stated plainly

The chain proves the record was not altered; with the content digest, next release, it also proves
what each change contained.
It does not prove that every change to your infrastructure went through SwitchTender: a person or a
process holding its own SSH key can change a host behind the controller, and no audit system records
what it never saw. For an AI agent the gap closes, because an agent that holds only a SwitchTender
token has no path around the gate. State this boundary to an assessor rather than implying the record
is exhaustive; it is the honest and defensible position. The same statement is in
[Concepts](concepts.md#provable-audit).

## Producing the evidence

- One change, in depth: `switchtender audit run <id>`, or the Evidence button on a run.
- A period's change register: `switchtender audit report --from 2026-07-01 --to 2026-08-01`.
- A signed, offline-verifiable bundle of the whole chain: `switchtender audit bundle`.
- On a cadence, unattended: `--evidence-dir` with `--evidence-cadence`, so the sample a review asks
  for already exists.

A redacted example the reader can hold is in [the sample evidence pack](sample-evidence-pack.md).
