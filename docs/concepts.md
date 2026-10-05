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
runs the play. A dynamic inventory source reads an inventory plugin config or an inventory file, or
runs a script from a project checkout, and refreshes the result into a stored inventory, with cloud
authentication supplied by a credential. A script at a bare path on the server is refused, since
nothing but the stored path would decide what code runs as the executor.

A stored inventory can also draw its content from an external store, a command, Vault, or Google
Secret Manager, resolved at launch, so the host list lives outside SwitchTender and is fetched fresh
for each run.

A smart inventory is a host filter over the other inventories, and a constructed inventory runs the
constructed plugin over a list of input inventories. Both are resolved at every launch for the
person launching, never reach an input that person may not use, and record on the run the hosts
they resolved to. The [inventories guide](inventories.md) covers both.

## Triggers

A webhook trigger is a URL that launches a template on an inbound git push. The project syncs
fresh first, so the run deploys the commit that was just pushed.

When the server has an encryption key, creating a trigger also mints a signing secret, shown once,
separate from the URL token so a leaked URL cannot forge signed pushes. Set that secret as the
webhook secret on the git host and turn on enforcement, and every inbound push must carry a valid
`X-Hub-Signature-256` HMAC over its body or it is rejected. Rotate the secret at any time. A bad or
missing signature never launches a run.

A git host stops waiting for a webhook's answer after about ten seconds, and a launch can take
longer when the template is a Terraform or OpenTofu plan whose modules the approval gate downloads
first. The fire is recorded on the audit chain before anything launches, and the sender is answered
within five seconds either way: with the run when the launch finished, or with `accepted` when it
is still going, in which case the run launches once the gate has read the configuration. A
redelivery that arrives while the launch is still going joins it, and one that arrives later is
answered with the run the first delivery made, so a delivery never launches twice. A launch that
fails after its sender was answered is recorded on the chain at `/hooks/<trigger>/failed` and in the
server log, since the answer that would have carried the failure reached nobody.

## Credentials

A credential is a secret sealed with AES-256-GCM, decrypted only at execution into the run's
environment or a temporary file created mode 0600 and deleted when the run ends. Fifteen kinds cover SSH keys and SSH passwords, vault passwords, become
passwords and full become settings, network device logins, environment bundles for cloud SDKs, API
tokens, container registry logins, Kubernetes kubeconfigs, and typed AWS, Azure, GCP, VMware, and OpenStack cloud credentials. The
[secrets guide](secrets.md) describes each. Secrets never appear in API responses. Four federated
kinds store no secret at all: each run that carries one receives a short-lived identity token
SwitchTender signs, as [workload identity federation](federation.md) describes.

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

### Organization roles are not global roles

An organization owns objects, and membership in it carries its own role, separate from the account's
global one. Read this part carefully, because the names invite the wrong reading.

A member with organization role **admin** can manage that organization's projects, templates,
inventories and credentials: edit them and delete them. That is how a tenant administers itself
without anyone needing install-wide admin.

The account's global role is still the ceiling. Organization admin on an account whose global role is
viewer confers use, not manage: it can read and launch the organization's objects but not change
them. So a read-only auditor stays read-only however you add them to an organization, and an
organization's admins are operators. Membership is delegation inside what an account may already do,
never a promotion past it, which is the same rule agent tokens follow.

Organization admin also confers nothing over the organization *record* itself. Managing members, and
the organization's own settings, is the install's global admin role, not this one. So an
organization admin can rewrite that organization's credentials but cannot list its members.

## Queues and workers

A worker is any process running the executor against the shared store. Every process, the server
included, competes for pending runs through the store. Workers beyond the server and named queues
are Team features. A run can target a named queue, and only
workers serving that queue run it, which places work across a mixed fleet. A lease keeps a run
attributable, and a janitor requeues work whose holder went away. The [reliability](reliability.md)
page details how work is claimed, bounded, recovered, and kept consistent across workers.

Queues pin at three levels, so they work like AWX instance groups: a run names its own queue, a
template pins every launch, or an inventory pins every run that targets it. The most specific wins:
run, then template, then inventory. Pin a DMZ inventory to workers inside the DMZ and every run
against it lands there, no matter how it was launched.

## Provable audit

Every authenticated mutation is recorded in the audit trail, and each entry is linked into a hash
chain built on SHA-256. Each entry commits to who acted, how they authenticated, the account whose
authority they used, the method and path, a digest of the change payload, the install that wrote it,
and the previous entry's hash. Altering, reordering, or deleting an entry breaks the chain, which
`GET /v1/audit/verify` detects. `GET /v1/audit/bundle` seals the chain into a signed LoomSeal
bundle, so the open `loomseal` verifier proves the trail is intact and unaltered offline, on the
command line or in a browser, without trusting the server that produced it.

**The record covers the change, not only that a call was made.** The link commits to
a digest of the request payload, so a recorded change cannot be re-cast as a different one while the
chain still verifies. The digest is taken over the payload with its secret fields redacted first, so
it proves the shape and non-secret content of a change without becoming a way to brute-force a
secret the request carried. A secret field is one named like a secret, such as a password or a
token, and also one whose name says nothing: the answer to a secret survey question and a secret
question's default, a notification target's address and key, and an approver's reason, which the
decision's own entry commits instead. Each entry also records how the caller authenticated and, for
a token bound to an account, the account it acted on behalf of, so a change an AI agent made under
an operator's authority is attributable to both and cannot later be presented as a person's.

**A receipt names the install that wrote it.** The install's identity takes part in every chain
link, and a verifier checks each claim against the identity the bundle advertises. Without this a
published receipt could be lifted whole: keep the claims and the genuine third-party anchor, rewrite
the producer, re-sign with a second key, and a relying party pinning that key reads somebody else's
history as its own. Rewriting the producer alone now contradicts the claims, and rewriting both
stops the links recomputing, so neither survives a verifier.

This depends on the install having a signing identity. Beside a local database one is created on
first start. A deployment sharing a database between processes is different: every process writing
that chain has to sign as the same install, so none of them will mint a key on its own, and until
one is supplied the server runs with the chain unattributed and unbound. It still records and still
verifies, but its receipts name no install and can be lifted. Set `SWITCHTENDER_AUDIT_KEY` to one
seed on every process, or place the same `producer-key.json` in each host's identity directory.

**A receipt discloses exactly what its entries committed.** A run's receipt carries the run's
outcome record, its spec, and each approval decision and correction beside the digests the chain
committed. Each is redacted before its bytes are fixed and disclosed as those same bytes, so a
LoomSeal verifier from 1.7.0 on checks every one of them as carried, with none of this product's
redaction rules. An outcome is committed under the exact digest form, `sha256e:`. A record whose
redacted form is over 1 MiB is committed and disclosed as its summary: the run, its status, exit
code, and spec digest, and the full record's size and SHA-256. An outcome recorded before the exact
form keeps verifying with `switchtender verify`, and an open verifier reports it as carried and
unchecked. A decision or correction an install recorded before nonces existed carries an unkeyed
digest, which still verifies and is named as legacy, because anyone holding the receipt can confirm
a guess of its body against it. Every entry after the first keyed one is keyed, so an unkeyed
digest after that point fails. Records are read only on the entries the chain says they are, and a
member whose name differs from a record member only in case fails the receipt, so a reader that
folds case cannot be shown a value no verifier checked.

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
install with no token yet. So authentication attempts live in the server log instead, which
records each attempt by username and outcome, success, failure, and rate-limited,
and never the password or a token. A successful single sign-on arrival is recorded as a chain entry,
since the identity provider already vouched for it, and every local sign-in leaves a durable mark
indirectly: it mints a session, and every change that session then makes is recorded with that
account as its actor. Forward the server log to your SIEM to retain and examine authentication
activity alongside the change trail.

**Evidence comes out as documents, not screenshots.** `switchtender audit run <id>` emits one
run's dossier: what ran, its risk grade, who approved it, what happened on each host, and the
receipts and anchors behind all of it. `switchtender audit report --from --to` renders the period's
change register, the sample that SOC 2 CC8.1 and ISO/IEC 27001 A.8.32 reviews ask for. Both are
self-contained HTML that a reviewer reads without tooling and checks against the live chain. The
per-run dossier is free. The period register is a Team feature, so the evidence is yours either way
and what a license pays for is the report that assembles it.

**A witness remembers what the server can no longer take back.** `switchtender witness`, run on a
machine the server's operator does not control, polls the public beat feed, keeps a signed
checkpoint of what it saw, and raises a finding when a beat goes missing, a witnessed beat comes
back rewritten, or the head regresses. The feed carries beats only from a server started with
`--span-cadence`, for example `--span-cadence 60s`, so turn that on before pointing a witness at
it. The witness's memory is first write wins, so a rewrite is never signed into the checkpoint as
if it were the truth, and a standing condition is reported when it appears and again when it
changes, not once per poll. The checkpoint's signer is pinned to the witness's
own key, so a state file replaced by a forger is refused rather than believed, and one state file
holds one server. That key is the witness's own, kept in `witness-key.json` and never the watched
server's `producer-key.json`, so a witness run on the host it watches still signs with a key its
operator does not hold. A feed that serves nothing, or one whose newest beat stops moving for a day, is
a finding in its own right: a witness with nothing to witness must not read as a clean bill of health.

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
them. Deduplicate on the receipt. The cursor also remembers the link of what it last sent, so a
database restored from an older copy, or one whose tail was cut, is noticed when the server starts:
the forwarder logs it and resumes after the newest delivered entry the chain still holds, and the
entries the chain has since appended reach the SIEM rather than being skipped.

**Receipts make an omission detectable by the party it happened to.** Every mutation returns an
`Audit-Receipt: seq:link` header naming where it was recorded, a webhook fire included. Signing in
and out is the exception: a sign-in carries no authenticated actor to record it against, and
recording one would let a stranger append to the chain without bound. Keep the receipts. `switchtender audit
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

One rule needs no file and no row. A run an AI agent asked for is held for a person by default,
named `requested by an agent, held by default`, until a stored rule with `effect: exempt` says that
run may go ahead. People's runs are unaffected. [Agent runs are held by
default](policy.md#agent-runs-are-held-by-default) covers the exemption, what it risks, and the
upgrade.

Point `--policy-file` at a YAML file and that file becomes the source of truth:

    policies:
      - name: prod-terraform-destroy
        tool: terraform
        command_contains: destroy
      - name: large-teardown
        tool: opentofu
        max_destroy: 5

That file holds two policies, so it needs Pro, which holds five. Team removes the cap. Community
holds one, and a file carrying more is refused at startup naming the tier, rather than quietly
enforcing a subset. Drop the second entry to run this example on Community.

A change to what needs approval is then a diff. It goes through whatever review the repository
holding it requires, it is attributable to a commit, and an auditor reads the policy that was in
force at any moment by checking out that commit. The API refuses policy writes with a 409 naming the
file, rather than accepting a change that would have no effect. A malformed file stops the server,
because degrading to no policies turns a typo into an install where nothing is gated and nothing
says so. Editing the file takes effect without a restart, so merging is what deploys.

Omitting `max_destroy` makes a blanket policy that holds every matching run. Setting it makes a
plan-content policy that holds only when a Terraform or OpenTofu plan would destroy more than that
many resources.

The same file can load Rego policies written for Open Policy Agent, so an existing OPA or Conftest
rule ports as it is. [Approval policies](policy.md) covers both kinds, the input a Rego policy
reads, and how its decisions map onto the YAML ones. A Rego `warn` holds the run by default, and a
policy can set `warn: note` to record its warnings on the run and in the evidence without holding
it, so one set of checks can hold production runs and only note staging ones.

### Dry runs and `exclude_dry_run`

Setting `exclude_dry_run` leaves a dry run unmatched, so a preview that changes nothing does not
wait for a person. A [pull request review](pull-request-review.md#plans-and-your-approval-rules)
plan is a dry run, so this one line is what lets plans of pull requests run without waiting while
the apply after merge is still held. A tool's dry-run mode is a promise about the tool, not about
what it is given, so the gate reads what a dry run executes before it exempts it, and exempts only
a dry run it can classify as change free. A dry run it cannot classify is matched as the real run
it may be, and graded that way for `min_risk` and `reversibility` too. A Rego policy is evaluated on
such a dry run both as it is and as the real run, and the stricter answer stands, so a module that
exempts `input.run.dry_run` holds it where the YAML rule does. [Approval
policies](policy.md#dry-runs-that-are-not-change-free) has the details.

An Ansible dry run is `ansible-playbook --check`, and check mode is not a promise about the
playbook: a play, block, task, role, or include that sets `check_mode: false` (or `no`, or a
templated value) runs that work for real even under `--check`. The gate reads the playbook and
everything it pulls in, and a dry run whose playbook forces real tasks with `check_mode` is not
exempt.

A Terraform or OpenTofu dry run is `plan`, and a plan runs the program every `external` data source
names, with the run's credentials and environment. The gate reads the configuration in the run's
working directory, and every module it calls, with the HCL parser Terraform is built on, and a plan
that declares an external data source anywhere is not exempt. The hold names each one by the
address a plan gives it, such as `module.network.data.external.lookup`, with its file and line. A
data source scoped to a `check` block counts, since a plan reads it too. The scan does not evaluate
`count` or `for_each`, so a data source that a count of zero switches off is still reported.

A plan also runs provider code, with the same credentials. Providers are code the team chose and
installed, and the scan does not judge them: it looks for explicit program execution and for
configuration it could not read, nothing else. A finding means the plan cannot be classified as
change free, not that it has side effects.

Both scans fail closed. What a scan cannot read could run work for real too, so a dry run with any
of it is not exempt either:

- For Ansible: a role that is not in the project, an include named at run time, or a playbook past
  the limits one read covers.
- For Terraform and OpenTofu: a registry or remote module the gate could not download, a module
  source or version only known when the run plans (OpenTofu evaluates variables in `source`), a
  file that does not parse, a local module outside the project, or a configuration past the limits
  one read covers.

A commit does not hold `.terraform`, so before it reads a plan whose configuration calls registry
or remote modules, the gate downloads them. It runs the tool's own `terraform get` or `tofu get` in
a private copy of the configuration, which is removed afterward. A get downloads modules and
nothing else. It installs no provider and runs no program a configuration names. It runs with the
run's own credentials, opened the way execution opens them, with the binary or image the plan would
use, and only where the plan itself may run. A plan routed to a named queue runs on a worker whose
network the server does not share, so its modules are not downloaded for it. The download is
bounded at two minutes and at 512 MiB or 50,000 files. One that fails, runs too long, writes too
much, or cannot run leaves the modules unread, with the reason, so the plan is not exempt.

The gate then reads each module where the get installed it, which `.terraform/modules/modules.json`
names, and only when that copy is the one the run's own `init` keeps: the same source, and for a
registry module a version its constraint allows. A `.terraform` the gate did not install itself is
never read, whether a commit carries it or a working directory on the server holds it, since its
manifest could point a module at a harmless copy while `init` installs the real one. The gate copies
the configuration without it and downloads the modules afresh, and no setting makes it trust a copy
it did not install. A module vendored into the repository and called by a local path is read like
any other file.

The run executes exactly the modules the gate read. A plan held for approval can wait days, and a
version constraint that allows more than one release could resolve to a newer one by then, so the
gate records a digest of the module tree it read, the path and content of every file under
`.terraform/modules`, and keeps a copy of the tree. The digest is part of the scan's evidence and of
the spec an approval binds, so an approver releases those modules and no others. When the run
starts, the kept copy goes into its working directory and `init` is told to install no module, so it
resolves no version again. When the copy is not on the machine the run executes on, because it was
kept longer than `--module-keep-for` ago, seven days by default, or the run went to another worker,
the run downloads its modules again and goes ahead only if they have the same digest. Otherwise it
is refused before anything runs, with both digests named, and the plan has to be submitted again so
the gate reads the current modules. The kept copies live under the run-files directory, at most
`--module-keep-max-mib` of them, 2 GiB by default, the oldest dropped first.

Only a dry run whose input was read in full and runs nothing keeps the exemption, and that is true
whichever way it arrives: from a person, a schedule, a webhook, a template, a pull request, or an
agent through MCP.

The run records each scan as `dry_run_scans`: the scanner and its version, the files it read, what
it found, what it could not read, and the classification, `change_free`, `not_change_free`, or
`incomplete`. A scan the gate downloaded modules for carries `fetch`: the command, its exit status,
why it failed when it did, and when it completed, `modules_digest`, the digest the run is held to.
The approver sees the findings on the held run, in the held notification, in the run's dossier and
register, and in the signed outcome record, so the evidence says why a dry run waited for approval.
What executes does not change: an Ansible dry run still passes `--check`, and a plan is still a
plan.

The plan a plan-content rule runs ahead of an apply is not held for what the scan finds. The apply
was asked for as a real run, and an apply within the rule's limit runs the same programs without
waiting, so holding its plan would stop nothing the apply does not do anyway.

### When a dry run you expected to pass is held

The hold says what the scan found, or what it could not read, and names the two clean fixes: rework
what was found so the dry run is safe, or drop `exclude_dry_run` from the rule that held it. A Rego
rule has no `exclude_dry_run`, so its second fix is to stop exempting dry runs in its module. There
is no per-task or per-resource override. A mark saying "this one is safe" is a claim the gate cannot
check, written by whoever wants the run to go through, which is the bypass the scan exists to close.

The trade-off is between the exemption and the playbook or configuration as written. The cases
that come up most:

- A read-only command forced to run. A playbook often reads something with `command: git rev-parse
  HEAD` or `command: cat /etc/app/version`, with `check_mode: false` and `changed_when: false`, so
  later tasks can use the answer during `--check`. The command changes nothing, but the playbook
  does not say so in a form the gate can check, and the same keyword on a restart looks identical.
  To keep the exemption, read with a module that reads under check mode on its own, such as
  `ansible.builtin.stat`, `ansible.builtin.slurp`, or a `*_info` module, or skip the task under
  check mode with `when: not ansible_check_mode` when the dry run does not need its answer. If the
  dry run truly needs the command, drop `exclude_dry_run` from the rule, and every dry run it
  matches waits for approval, which is what that playbook's dry runs are.
- A role that is not in the project. A playbook that names a role installed only on the server, or
  one from Galaxy that the project does not list, cannot be read, so the dry run is incomplete. To
  keep the exemption, list the role in the project's `requirements.yml` with `install_deps` on, so
  the sync installs it where the gate reads it, or commit the role to the repository. Otherwise
  drop `exclude_dry_run`.
- An external data source. To keep the exemption, replace it with a provider data source that
  reads the same thing, or pass the value in as a variable, which a survey answer or template
  variable delivers as `TF_VAR_`. If the program has to run, drop `exclude_dry_run`.
- A registry or remote module. The gate downloads it before it reads the plan, so a hold here
  means the download did not complete, and the hold says why. To keep the exemption, make the
  module reachable with the run's own credentials from where the plan runs, such as an `env`
  credential carrying `TF_TOKEN_<host>` or `TF_CLI_CONFIG_FILE` for a private registry, keep it
  within the download's bounds, or vendor it into the repository and call it by a local path. A
  plan routed to a named queue is not downloaded for, so its modules have to be vendored. Otherwise
  drop `exclude_dry_run`.

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
moment it is held, so editing the policy afterward cannot loosen a decision already pending. A run
submitted by an AI agent is held this way by default, with no policy written first, unless an
exemption covers it, and it is released the same way, only by a human admin, so an operator-bound
agent never approves its own work. See
[Agent runs are held by default](policy.md#agent-runs-are-held-by-default). Both the request and
the decision are recorded in the audit trail, making who asked for a change and who signed off
provable. A Terraform or OpenTofu apply marked this way is planned first instead, and the apply its
plan proposes is what waits, carrying the saved plan the approval binds.

A pull request comment is one more way to decide. A person whose GitHub or GitLab account is linked
to their SwitchTender account can comment `/switchtender apply` on a pull request, naming the plan
id its report showed, which approves the apply of that plan as their SwitchTender account, with the
same admin role, grants, and separation of duties as the queue. The comment is read back from the
forge before it decides, a bot's comment or one an app wrote for its author is refused, and the
decision's chain entry records the comment's and its author's numeric ids, the SHA-256 of its body,
and the plan it approved. See [planning and applying from a
comment](pull-request-review.md#planning-and-applying-from-a-comment).

Approval can also be required by policy rather than by choice. A policy matches runs by tool, command
text, or target inventory, and any matching run is held automatically at submission, so the gate
cannot be skipped by omitting the flag. A Terraform or OpenTofu apply a policy would hold is planned
first, and the apply its plan proposes is what waits, carrying the saved plan the approval binds.
Policies are enforced in the dispatcher, so a scheduled or triggered run is gated the same as one
launched by hand.

A workflow is gated by the same policies as a single run. Every step is checked when the workflow is
submitted, and a match holds the whole workflow before any step starts, so a gated command cannot be
slipped past an approver by wrapping it in a workflow. The whole workflow is held rather than the
one matching step, because a change applied halfway is worse than one that never started. Approving
releases the workflow and it runs from the top. The step graph is stored with it, so an approval
that arrives after a restart still runs the workflow that was approved.

## Approval steps in a workflow

A workflow can also stop partway and wait for a person, at an approval step. The steps the approval
step depends on run first. When nothing else in the workflow can move, it pauses at the step until
an admin approves or denies it, and then it continues.

A step is an approval step when it carries `"type": "approval"`. It runs no tool, so it takes no
playbook, command, inventory, dry run, or retries, and it must have a name, since the approver is
shown it. `description` is what the approver is asked to decide, and `approval_timeout` is how many
seconds it waits, where zero waits until somebody decides.

```json
{"name": "release", "steps": [
  {"name": "build", "tool": "bash", "command": "make release"},
  {"name": "gate", "type": "approval", "description": "Check the canary, then ship",
   "approval_timeout": 3600, "depends_on": ["build"]},
  {"name": "ship", "playbook": "deploy.yml", "depends_on": ["gate"]},
  {"name": "notify", "tool": "bash", "command": "./page-oncall.sh", "if_denied": ["gate"]}
]}
```

The answer decides which path runs, matching AWX's approval node:

- Approved, the steps that depend on the approval step run. That is its approve path.
- Denied, or not decided before the timeout passes, the steps that name it in `if_denied` run
  instead. That is its deny path, the equivalent of an AWX failure path. A timeout is recorded as the
  system ending the wait, not as anybody's decision.
- A denial or a timeout with no deny path fails the workflow, as an AWX approval node with no
  failure path fails its workflow job. With a deny path, the workflow's result is the result of that
  path.
- An approval step cannot be marked continue on failure, since that would run its approve path
  after a denial. A step cannot both depend on an approval step and handle its denial.
- A plain sequence of steps with an approval step in it waits at the step and continues in order.

**Who is told.** A workflow reaching an approval step notifies with its own event,
`workflow.step_awaiting_approval`, distinct from a whole run held for approval. It reaches the same
chat channels, email, webhooks, and named targets attached for `approval` that a hold reaches, and
it names the workflow, the step, what the step asks, and what approving and denying each run next.

**Who may decide.** The same rules as a held run. Deciding is an admin route, so an agent's token,
which is capped at operator, can never approve a step, and the dispatcher refuses an agent's approval
a second time. When any approval policy in force at submission that covers the workflow or one of its
steps sets `require_distinct_approver`, the person who launched the workflow cannot approve its
approval steps, matched by account so a token and a browser session are one person. As with a held
run, the launcher can always deny, which withdraws their own request.

**What an approval binds to.** An approver approves the state they were shown: the workflow's spec,
every step the approval waited behind with how it ended and what it published, and the steps each
answer runs. `GET /v1/approvals` returns that state's `state_digest`, and sending it back with the
decision refuses the decision if the workflow no longer matches. The decision is committed to the
audit chain with that digest before it takes effect, and the workflow is held to it when it resumes:
a workflow whose finished steps or published values changed after the approval is refused rather
than continued.

**Where to decide.** The approvals panel on the Runs page lists every waiting step with what already
ran and what each answer runs next, and the run page of a waiting workflow shows its own. A step is
decided by `POST /v1/runs/{id}/approve` or `/reject` with the step's id. The same call with the
workflow's id is refused with 409, naming each waiting step and the call that decides it: only a
step's own decision moves the workflow, since the step carries the rules that apply to it alone and
the state its approver is shown. Deciding a workflow as a whole run is refused once it has started,
since that would run it from the top.

**Notifications and evidence.** Reaching an approval step notifies the same channels a held run does,
naming the step, including every named notification target attached for the `approval` event to
the workflow's template, schedule, project, or organization. A named target hears the step after the
workflow's start and the steps it comes after, and two steps that run beside each other are not put
in an order between themselves. The request, the approval or denial, and a timeout are each a `DECISION` entry on
the audit chain, at `/runs/{workflow}/steps/{step}/decision/{verdict}`, and the workflow's dossier
and receipt disclose them beside the digests the chain committed.

**Durability.** A paused workflow is held by nothing in memory. When it parks it releases its lease,
so a restart loses nothing, and whichever replica records the decision resumes it from the step
records in the store, without running finished steps again. Resuming is a compare-and-set on the
workflow, so of every replica that sees the decision exactly one continues it, and every replica's
janitor times out expired steps and resumes a decided workflow whose resume was lost to a crash.
A process that dies after listing a step and before parking leaves the workflow running under a
lease nobody renews, and the step can still be decided. Once that lease expires the janitor parks
the workflow rather than interrupting it, and a decision made in the meantime resumes it like any
other. A workflow with another step still executing, or ready to start, is interrupted as any run
whose worker died is. Canceling a paused workflow withdraws its waiting step.
