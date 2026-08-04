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

A credential is a secret sealed with AES-256-GCM, decrypted only at execution into the run's
environment or a temporary file created mode 0600 and deleted when the run ends. Thirteen kinds cover SSH keys and SSH passwords, vault passwords, become
passwords and full become settings, network device logins, environment bundles for cloud SDKs, API
tokens, container registry logins, and typed AWS, Azure, GCP, and VMware cloud credentials. The
[secrets guide](secrets.md) describes each. Secrets never appear in API responses.

## Teams and grants

The global roles, admin, operator, and viewer, decide the broad strokes. On top of them, a grant
gives a user or a team access to a specific project, template, inventory, or credential. A read grant
lets them see the object in a listing without using or changing it. A use grant lets them launch or
reference the object, such as running a template or attaching a credential. A manage grant lets them
edit and delete that object, so management of one project or credential can be delegated to a team
without handing out the global admin role. The levels nest, so use includes read and manage includes
use. Grants are additive: an object with no grants defers to the global role, so nothing changes on
upgrade until grants are added. Under strict grants a read grant also scopes what a non-admin sees, so
a listing returns only the objects they are granted, closing the gap where the only way to give read
access was the global viewer role over everything.

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
hash chain. It carries the previous entry's hash and its own hash over its content. Altering,
reordering, or deleting an entry breaks the chain, which `GET /v1/audit/verify` detects. A signed
export from `GET /v1/audit/export` seals the chain with an ed25519 signature, so
`switchtender audit verify` proves the trail is intact and unaltered offline, without trusting the
server that produced it.

A chain proves that what it holds was not altered. On its own it cannot prove that nothing is
missing, because the same server decides both what happens and what gets written down, and because a
prefix of a valid chain is itself a valid chain. Three things close that gap.

**A change that cannot be recorded does not happen.** The entry is written before the handler runs,
so a mutation whose audit write fails is refused with a 503 rather than performed silently. Changes
made from the command line, including creating an account and minting a token, are recorded the same
way.

**Anchors fix the chain in time.** `switchtender audit anchor` asks a public RFC 3161 timestamp
authority to sign the moment it saw the current head. The token is embedded in every bundle built
afterwards and is checked offline by any verifier, with no network and no trust in this install, so
a chain that has quietly lost its tail no longer reaches its anchor and says so. Anchor on a
schedule. An anchor bounds how much history can vanish unnoticed to whatever happened since the last
one.

**Evidence comes out as documents, not screenshots.** `switchtender audit run <id>` emits one
run's dossier: what ran, its risk grade, who approved it, what happened on each host, and the
receipts and anchors behind all of it. `switchtender audit report --from --to` renders the period's
change register, the sample a SOC 2 CC8.1 or ISO/IEC 27001 A.8.32 review asks for. Both are
self-contained HTML that a reviewer reads without tooling and checks against the live chain.

**A witness remembers what the server can no longer take back.** `switchtender witness`, run on a
machine the server's operator does not control, polls the public beat feed, keeps a signed
checkpoint of what it saw, and raises a finding when a beat goes missing, a witnessed beat comes
back rewritten, or the head regresses. Its memory is first write wins, so a rewrite is reported on
every watch and never signed into the checkpoint as if it were the truth. The checkpoint's signer is
pinned to the witness's own key, so a state file replaced by a forger is refused rather than
believed, and one state file holds one server.

**Receipts make an omission detectable by the party it happened to.** Every mutation returns an
`Audit-Receipt: seq:link` header naming where it was recorded. Keep them. `switchtender audit
receipt 41:9f2c...` confirms the chain still holds that exact link at that exact position, and a
server that omitted the entry cannot produce a chain containing the receipt.

What none of this defends against is an operator running modified code on the machine itself. That
is the boundary of every audit system, it costs an attacker the whole controller to reach, and it
does not hand them the past. Anything already anchored stays fixed and still proves what it proved.

None of this changes when the operator is an AI agent. A change an agent makes through the API
chains identically to one a person makes: recorded before it executes, with the agent's token label
as the actor, covered by the same receipts and anchors. [Running an agent](agents.md) covers giving
an agent that token and nothing else.

## Policy as code

Approval policies decide which runs a person has to sign off, so who may change them is the whole
question. Held as rows they are changed by anyone the API lets through, and the change leaves a row
indistinguishable from the row before it.

Point `--policy-file` at a YAML file and that file becomes the source of truth:

    policies:
      - name: prod-terraform-destroy
        tool: terraform
        command_contains: destroy
      - name: large-teardown
        tool: opentofu
        max_destroy: 5

A change to what needs approval is then a diff. It goes through whatever review the repository
holding it requires, it is attributable to a commit, and an auditor reads the policy that was in
force at any moment by checking out that commit. The API refuses policy writes with a 409 naming the
file, rather than accepting a change that would have no effect. A malformed file stops the server,
because degrading to no policies turns a typo into an install where nothing is gated and nothing
says so. Editing the file takes effect without a restart, so merging is what deploys.

Omitting `max_destroy` makes a blanket policy that holds every matching run. Setting it makes a
plan-content policy that holds only when a Terraform or OpenTofu plan would destroy more than that
many resources.

## Approvals

A run can be marked to require approval. It is held in a pending-approval state that the claim loop
never picks up, until an admin approves it, which releases it to run, or rejects it, which ends it.
An operator can request the run, but only an admin can release it, so duties are separated. A held
run submitted by an AI agent is released the same way, only by a human admin, so an operator-bound
agent never approves its own work. Both the request and the decision are recorded in the audit
trail, making who asked for a change and who signed off provable.

Approval can also be required by policy rather than by choice. A policy matches runs by tool, command
text, or target inventory, and any matching run is held automatically at submission, so the gate
cannot be skipped by omitting the flag. Policies are enforced in the dispatcher, so a scheduled or
triggered run is gated the same as one launched by hand.

A workflow is gated by the same policies as a single run. Every step is checked when the workflow is
submitted, and a match holds the whole workflow before any step starts, so a gated command cannot be
slipped past an approver by wrapping it in a workflow. The whole workflow is held rather than the one
matching step, because a change applied halfway is worse than one that never started. Approving
releases the workflow and it runs from the top; the step graph is stored with it, so an approval that
arrives after a restart still runs the workflow that was approved.
