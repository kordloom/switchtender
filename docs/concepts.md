<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Concepts

## Runs

A run is one execution of a playbook against an inventory. SwitchTender shells out to
`ansible-playbook` and captures both a human log and a structured event stream through an embedded
callback plugin, so a run is queryable data, not just scrollback. Every run records its status,
timing, exit code, the extra vars going in, and the `set_stats` outputs coming out.

## Splits

A split shards one inventory across parallel slices that run the same playbook, each limited to its
hosts, with the parent rolling up the result into one merged host matrix. Hosts are packed into
shards by their measured average duration in recent runs, so each shard carries a similar amount of
work. A finished split can retry only its failed shards.

## Pipelines

A pipeline runs playbook steps in order, or as a dependency graph when steps declare what they
depend on. Each step is itself a run, so it gets the full matrix, events, and history. A step can
retry on failure, and values a step publishes with `set_stats` flow to the steps that depend on it.

## Projects

A project sources playbooks from a git repository. Every run records the exact commit it executed,
so history answers what version ran, not just what file name. A project can install its
`requirements.yml` roles and collections on each sync, and can pin a container image its runs
execute inside.

## Templates

A template is a saved launch preset bundling a project, playbook, inventory, shard count,
credentials, and extra vars. One click in the UI or one POST launches it. A template can also
declare a survey.

## Inventories and sources

A stored inventory is inventory content referenced by id and materialized on whichever executor
runs the play. A dynamic inventory source runs an inventory plugin or script and refreshes the
result into a stored inventory, with cloud authentication supplied by a credential.

A stored inventory can also draw its content from an external store, a command, Vault, or Google
Secret Manager, resolved at launch, so the host list lives outside SwitchTender and is fetched fresh
for each run.

## Triggers

A webhook trigger is a URL that launches a template on an inbound git push. The project syncs
fresh first, so the run deploys the commit that was just pushed.

When the server has an encryption key, creating a trigger also mints a signing secret, shown once,
separate from the URL token so a leaked URL cannot forge signed pushes. Set that secret as the
webhook secret on the git host and turn on enforcement, and every inbound push must carry a valid
`X-Hub-Signature-256` HMAC over its body or it is rejected. Rotate the secret at any time. A bad or
missing signature never launches a run.

## Credentials

A credential is a secret sealed with AES-256-GCM, decrypted only at execution into a temporary file
and wiped afterward. Kinds are SSH keys, vault passwords, environment bundles for cloud SDKs,
become passwords, and container registry logins. Secrets never appear in API responses.

## Teams and grants

The global roles, admin, operator, and viewer, decide the broad strokes. On top of them, a grant
gives a user or a team access to a specific project, template, inventory, or credential. A use grant
lets them launch or reference the object, such as running a template or attaching a credential. A
manage grant lets them edit and delete that object, so management of one project or credential can be
delegated to a team without handing out the global admin role. Grants are additive: an object with no
grants defers to the global role, so nothing changes on upgrade until grants are added.

## Queues and workers

A worker is any process running the executor against the shared store. Every process, the server
included, competes for pending runs through the store. A run can target a named queue, and only
workers serving that queue run it, which places work across a mixed fleet. A lease keeps a run
attributable, and a janitor requeues work whose holder went away. The [reliability](reliability.md)
page details how work is claimed, bounded, recovered, and kept consistent across workers.

Queues pin at three levels, so they work like AWX instance groups: a run names its own queue, a
template pins every launch, or an inventory pins every run that targets it. The most specific wins:
run, then template, then inventory. Pin a DMZ inventory to workers inside the DMZ and every run
against it lands there, no matter how it was launched.

## Provable audit

Every authenticated mutation is recorded in the audit trail, and each entry is linked into a SHA-256
hash chain: it carries the previous entry's hash and its own hash over its content. Altering,
reordering, or deleting an entry breaks the chain, which `GET /audit/verify` detects. A signed export
from `GET /audit/export` seals the chain with an ed25519 signature, so `switchtender audit verify`
proves the trail is intact and unaltered offline, without trusting the server that produced it.

## Approvals

A run can be marked to require approval. It is held in a pending-approval state that the claim loop
never picks up, until an admin approves it, which releases it to run, or rejects it, which ends it.
An operator can request the run, but only an admin can release it, so duties are separated. Both the
request and the decision are recorded in the audit trail, making who asked for a change and who signed
off provable.

Approval can also be required by policy rather than by choice. A policy matches runs by tool, command
text, or target inventory, and any matching run is held automatically at submission, so the gate
cannot be skipped by omitting the flag. Policies are enforced in the dispatcher, so a scheduled or
triggered run is gated the same as one launched by hand.
