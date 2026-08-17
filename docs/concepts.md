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
environment or a temporary file created mode 0600 and deleted when the run ends. Fourteen kinds cover SSH keys and SSH passwords, vault passwords, become
passwords and full become settings, network device logins, environment bundles for cloud SDKs, API
tokens, container registry logins, and typed AWS, Azure, GCP, VMware, and OpenStack cloud credentials. The
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
hash chain. Each entry commits to who acted, how they authenticated, the account whose authority
they used, the method and path, a digest of the change payload, and the previous entry's hash.
Altering,
reordering, or deleting an entry breaks the chain, which `GET /v1/audit/verify` detects. `GET /v1/audit/bundle`
seals the chain into a signed LoomSeal bundle, so the open `loomseal` verifier proves the trail is
intact and unaltered offline, on the command line or in a browser, without trusting the server that
produced it.

**The record covers the change, not only that a call was made.** The link commits to
a digest of the request payload, so a recorded change cannot be re-cast as a different one while the
chain still verifies. The digest is taken over the payload with its secret fields redacted first, so
it proves the shape and non-secret content of a change without becoming a way to brute-force a
secret the request carried. Each entry also records how the caller authenticated and, for a token
bound to an account, the account it acted on behalf of, so a change an AI agent made under an
operator's authority is attributable to both and cannot later be presented as a person's.

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

**A run created by a request points at the entry that authorized it.** A run's creation is recorded before the
handler runs, at a request path that names the template rather than the run it goes on to create,
so the two cannot be matched by name. The run keeps the receipt instead, the same `seq:link` the
`Audit-Receipt` header returned, and its dossier redeems that receipt against the live chain. A
server that dropped the creation entry cannot answer the receipt, and the dossier says so rather
than showing a run with no origin. A run the scheduler starts carries a receipt too: the fire is
recorded as its own chain entry before the run exists, naming the schedule and committing what it
was configured to launch, and a fire that cannot be recorded is skipped rather than performed
silently.

**Authentication attempts are not in the chain, and this is deliberate.** An assessor reviewing an
append-only trail will notice sign-in attempts are absent, so here is why. A sign-in and a webhook
probe are reachable by anyone on the network, and the audit append is fail-closed: recording every
attempt would let a stranger fill the chain with entries, and once the store filled, the fail-closed
append would refuse every real change and lock the install, including sign-in itself on a fresh
install with no token yet. So authentication attempts live in the server log instead; from the next
release the log records each attempt by username and outcome, success, failure, and rate-limited,
and never the password or a token. A successful single sign-on arrival is recorded as a chain entry,
since the identity provider already vouched for it, and every local sign-in leaves a durable mark
indirectly: it mints a session, and every change that session then makes is recorded with that
account as its actor. Forward the server log to your SIEM to retain and examine authentication
activity alongside the change trail.

**Evidence comes out as documents, not screenshots.** `switchtender audit run <id>` emits one
run's dossier: what ran, its risk grade, who approved it, what happened on each host, and the
receipts and anchors behind all of it. `switchtender audit report --from --to` renders the period's
change register, the sample a SOC 2 CC8.1 or ISO/IEC 27001 A.8.32 review asks for. Both are
self-contained HTML that a reviewer reads without tooling and checks against the live chain.

**A witness remembers what the server can no longer take back.** `switchtender witness`, run on a
machine the server's operator does not control, polls the public beat feed, keeps a signed
checkpoint of what it saw, and raises a finding when a beat goes missing, a witnessed beat comes
back rewritten, or the head regresses. Its memory is first write wins, so a rewrite is never signed
into the checkpoint as if it were the truth, and a standing condition is reported when it appears
and again when it changes, not once per poll. The checkpoint's signer is pinned to the witness's
own key, so a state file replaced by a forger is refused rather than believed, and one state file
holds one server.

**A hosted witness answers auditors, not just operators.** `switchtender witness serve` watches
any number of servers from one process, records every finding durably, and serves what it has
witnessed over a read-only API. Its centerpiece is the attestation: a countersigned statement of
the head it holds for a server, when it saw it, and how many findings it has ever recorded there.
A relying party fetches one and verifies it offline with `switchtender witness verify-attestation`
against the witness key they pinned. The watched operator cannot mint one, cannot alter one, and
cannot answer one that disagrees with the chain they serve, which turns "our history is intact"
from a claim the operator makes into one a third party signs. When the witness cannot see a feed,
the attestation says so rather than going quiet, because a server going dark on its witness is
itself a signal.

A hosted witness reachable off its own host requires a read token on every
API call, set with `--api-token` or `SWITCHTENDER_WITNESS_TOKEN`, so a public deployment does not
hand any caller the list of watched servers or the cross-server findings feed. Offline attestation
verification is unaffected: a relying party checks an attestation it already holds against the
pinned key with no call to the witness at all, so the token gates delivery, not trust.

**The chain streams into the SIEM, and stays checkable there.** `--forward-url` and
`--forward-syslog` deliver every audit entry to the operator's collector, each event carrying its
`seq:link` receipt. That makes the forwarded copy more than a copy: an analyst who samples any
event from the SIEM, months later, redeems its receipt against the live chain and gets proof the
entry still stands at that exact position. The cursor is durable and advances only when every
sink accepted, so delivery is at least once and an outage delays events rather than dropping
them. Deduplicate on the receipt.

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

The apply that a plan proposes is pinned to the commit the plan was read from, and it inherits the
plan's requester. So an approval releases the code the approver actually saw: if the project's branch
has moved by the time the apply runs, the run refuses and names both commits rather than applying
changes nobody judged. The pin appears in the run's dossier.

## Approvals

A run can be marked to require approval. It is held in a pending-approval state that the claim loop
never picks up, until an admin approves it, which releases it to run, or rejects it, which ends it.
An operator can request the run, but only an admin can release it, so duties are separated. Set
`require_distinct_approver` on the policy and the separation covers admins too: the person who asked
for the change cannot be the one who approves it, and the requirement is recorded on the run at the
moment it is held, so editing the policy afterward cannot loosen a decision already pending. A held
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
