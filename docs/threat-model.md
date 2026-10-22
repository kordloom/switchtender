# Threat model

The homepage makes absolute claims: nothing runs until it clears the gate, a change that cannot be
recorded is refused, every run leaves a receipt anyone can verify without us. Absolute claims invite
attack, and a security reviewer is right to attack them. This page is where they are defended, one
adversary at a time, with the mechanism named, the failure behavior stated, and the limits kept in
plain sight. Anything this page cannot defend, the homepage should not be saying.

## The claims, mapped to mechanism

**"Nothing runs until it clears one path: request, policy, approval."** Every way a run enters,
the API, the UI, a schedule firing, the MCP server, a relayed worker, ends at the same submit call
inside the dispatcher, and the policy pass sits inside that call. There is no second door. The pass
fails closed twice over: if the policy store cannot be read, the submission is refused rather than
waved past a gate that could not be checked, and a deny rule that cannot be evaluated refuses the
run the same way.

**"A change that cannot be recorded is refused."** The audit entry is appended before the handler
runs, not after and not in a goroutine whose error is logged and dropped. If the append fails, the
mutation answers 503 and does not happen. The sequence number is assigned at append, so an entry
that was never written leaves no gap to notice and no gap to hide: the refusal is the evidence.

**"An approval releases exactly what was approved."** The decision is committed to the chain before
the run is released, and it commits a digest of the run's spec at the moment of decision. At claim
time the executor recomputes that digest from the spec it is about to run. If they differ, the run
fails with that stated, rather than executing something nobody approved. The chain would catch the
same tampering at verify time; this refuses to perform it in the first place.

The spec covers what decides a run's effect, not only the request. The container image is resolved
when the run is submitted, from the run, its template, its project, or the server default, and
pinned onto it by the digest its registry serves, so a project's image changed after an approval
cannot move the approved run into another container. When the registry does not answer, the run is
bound to the tag and the approver is told the tag is not pinned to a digest, and the outcome records
the digest that was pulled. A static inventory is snapshotted at submission and the run executes
that snapshot, so an inventory edited after an approval cannot widen or redirect the run. A smart or
constructed inventory is snapshotted the same way, the result it resolved to and every input it drew
from, so an input edited after an approval, a host variable included, does not reach the run. A
Terraform or OpenTofu apply that a rule gates or holds, and a drift reconcile, carries out the saved
plan file its approval bound, and the tool refuses that plan if the infrastructure changed since. A dynamic inventory source is the one input that
resolves at execution by nature: the approval binds its definition and the outcome records the hosts
it reached.

**"The requester cannot release their own change."** A rule can require a distinct approver, and
the check runs before anything is recorded, so a refused self-approval leaves no decision entry
behind. Approving is admin-only. An agent identity is operator-bound, so an agent can propose work
and can never decide on it, its own or anyone else's.

**"Every run leaves a receipt anyone can verify without us."** Each mutation carries a SHA-256 link
to the one before it and a digest of its own content, and each entry is bound to the install that
wrote it. The chain exports as a bundle signed by the install's key, and an open verifier that is
not SwitchTender checks the signature, walks every link, and recomputes the content digests, on a
machine with no network. The verifier's spec has independent implementations that are held to agree
on the same conformance vectors, accepting and rejecting the same bundles.

## Adversaries

### A requester who wants to skip the gate

They cannot submit around it: every entry path shares the one submit call. They cannot approve
their own held run where a rule requires a second person. They cannot edit the spec after approval,
because the executor refuses a digest that no longer matches. And they cannot do any of it quietly,
because the submission, the decision, and the outcome are all separate chain entries, each naming
its actor, and the run itself records which rule held it.

### An operator who wants to rewrite history

Editing an entry breaks its content digest. Replacing an entry breaks the hash link of everything
after it. Truncating the chain moves the head backward, which is exactly what a witness on another
machine is watching for: it keeps a signed checkpoint of the head it saw and raises a finding when
history shrinks or a heartbeat goes missing. A bundle already exported is out of reach entirely; it
is signed, self-contained, and in somebody else's hands.

### An administrator of the controller

Root on the box is the honest edge of the design, so it is stated rather than waved at. What root
gains going forward: it can stop the service, and from the moment of compromise it can feed the
chain false entries, because a writer who controls the process controls what gets written. What
root does not gain: the past. Entries already witnessed, anchored, or exported verify against keys
and checkpoints the box does not hold, so the compromise is bounded in time to the moment it began,
and a witness dates that boundary. The chain proves the record was not altered after writing. It
has never claimed to prove the writer was honest at the time of writing, and this page will not
claim it either.

### Whoever can write the database

This principal can write every table, the audit chain's tables included, and can recompute every
hash the chain carries, since the chain's links are plain SHA-256 over public fields. It does not
control the server process and does not hold the install's producer identity seed. A principal that
also controls the process or holds the seed is the administrator of the section above, and nothing
here changes that boundary.

What holds depends on the server loading the producer identity, which the precondition below
states. The server derives an approval binding key from the producer identity with HKDF-SHA256 and
holds it only in memory. The database never holds it. Every run the server creates or releases
carries a seal, an HMAC under that key over the run's spec binding, its governance fields, its
parent's binding, and the audit chain entries it derives from, each named by id and content digest
and by the actor, actor type, delegated account, and method of the entry, so who decided is under
the seal. The governance fields are who asked, whether an agent or a person asked, who it acted for, the
queue, the labels, the reversibility grade, the timeout, and the creation time, among others. A
writer can read every row and still cannot mint a seal for a row it edited.

Before a run executes, the executor recomputes the seal from the row and from the chain entries it
fetches by id, and refuses the run on any difference. This covers held runs, approval nodes, shards
of a split run, workflow steps, and steps released by an approval. Approving or rejecting a run, or
deciding an approval step, also checks the seal first, so a rewritten row is refused before
anyone's decision attaches to it. Once an install's adoption is recorded, a keyed executor checks
every row, so a row whose seal a writer removed is refused as carrying none. A claim on a decision
in flight carries its own seal, so a decision claim the process did not write is never recorded as
a decision. Because the governance fields are sealed, a writer cannot mark a run as already
approved to skip a rule that makes it plan first, and cannot turn an agent's run into a person's to
skip the approval agent runs need.

Who decided is held the same way. A writer who renames the approver on a DECISION entry and
re-links the chain leaves a chain that verifies, but the executor reads the entry by id and checks
no link, and the seal it recomputes covers the name, so the run is refused before it runs. A receipt
is built only by the process that holds the key, and it is refused rather than signed when the run,
or an approval step whose approval it carries, no longer matches its seal, or when the install holds
no key. The dossier and the change register name each approver from the chain entry, hold the row
the approval released to its seal, and flag a name whose seal drifted as not trusted, and the
dossier then reads not verified.

An approval step's timeout and the time it started waiting are sealed. A forged timeout, whether
backdated or added, is refused: the workflow fails with the reason and neither the approved nor the
denied path runs. A step whose approval request could not be recorded fails the workflow instead of
running the deny path. The deny path runs only after a recorded verdict. A finished step's status
and outputs carry a result seal minted when the process writes them, so a workflow resumed later
feeds forward only what the process wrote.

Each attempt appends a START entry under an id fixed by the run and the attempt, after the executor
takes the run and before any tool runs. A finished run that a writer resets to pending is refused
and runs nothing, and an honest retry gets a new attempt and runs. If the START entry cannot be
recorded, the run does not run. A relay control node does the same for the runs it hands to relay
workers, and checks each run's seal before it hands it out. Launching a step that an approval
released appends one RELEASE entry under a fixed id, naming the decision entries that released it
by content digest. A receipt holds the released step's spec to the decision that released it, and
this product's verifier and every LoomSeal verifier from 1.10.0 on fail a receipt whose release or
decision was edited. A receipt that withholds its outcome reports its releases as verified but not
compared. When the check cannot be made, because a chain entry cannot be read, the run fails closed
and its error says the check could not be made. It never says the run was refused or changed.

All of this holds only on a server that loads the install's producer identity, and every server of
one install must load the same one. A server with no identity holds no key. It compares the digests
stored on each run and warns at startup that approval binding is row-checked only, which a writer
who also writes matching digests passes. A worker that shares the database directly holds no key,
so the runs it claims are held only to the digests stored on them. A server whose key names another
install refuses to start.

Work a release without the key left behind carries no seal. `switchtender adopt` seals it once per
install, run by a person. It verifies the chain from its first entry, refuses if any row already
carries a seal or the chain holds an entry only a keyed install writes, shows every row it would
seal, with who asked and who decided, and asks before it writes. It records the adoption on the
chain under an id the key fixes. A keyed server that starts with nothing to adopt records the
adoption itself, trusting no row, so adopt cannot later be run over that install. A keyed start
that finds sealed work and no adoption record is refused as a removed record.

Several things do not hold. The chain's links use public hashes. A writer can delete the adoption
record, re-link every later entry, and delete the anchors the database holds, and then adopt runs a
second time and seals whatever the writer left. Only an anchor or a witness checked outside the
database bounds that. An install enrolled with a witness is bounded: adopt fetches the witness's
signed head and refuses a chain that does not reach it, and refuses to adopt while the witness
cannot answer. A witness that holds nothing for an install says so in a signed statement, so an
unsigned answer saying the same is refused, and adopt and `witness enroll` refuse a plain-http
witness that is not on a loopback address. After it writes, adopt appends a span beat and pushes it,
so the witness's head covers the adoption record before adopt returns, and a keyed server on an
enrolled install refuses to start on a chain that does not reach the witness's head. One window
remains. From the moment adopt's push fails, or from a start that found entries the witness has not
seen while the witness could not be reached, until the next push lands, those newest entries are
outside the witness's memory, and a writer who removes the record and re-links the chain inside that
window is not refused by the witness. Adopt exits non-zero when its push fails, and the server logs
a warning when it starts without the check. Adopt prints the chain's length, its newest anchor and
the install it is bound to before it asks, and a request to adopt an install that was adopted or
served before is itself evidence that its chain was rewritten. The same re-link reopens a finished
run: a writer who deletes its START entry and re-links the chain makes the attempt look unstarted.
It also reaches receipts. The server signs the chain it reads, so a re-linked edit or a cut tail on
a chain that no anchor or witness covers verifies. Anchors and the witness detect each of these, as
for adoption. A workflow that ended interrupted or canceled before its walk finished can be resumed
by a writer who resets its status, and it then launches the steps that never ran. No step runs
twice, but nobody asked for the resume. Adopt's check that no row carries a seal is a tripwire for
an honest restore of the wrong backup, not a defense: one statement clears every seal. A rule's
policy is read from the database when a run nobody approved executes, so a writer who edits a policy
changes which unapproved applies are made to plan first.

Who decided has limits of its own. Outside the process nothing changes. The decider on the chain is
bound by the public link, so an exported chain that was re-linked carries the rewritten name and
verifies until an anchor or a witness beat covering that entry is compared, and inside the
unwitnessed tail nothing bounds it. A receipt issued before the rewrite is unaffected and is not
checked again later. An install with no producer identity holds no key, so the decider is
rewritable there like every other field, and the dossier and the register say unchecked and never
verified for it. The decision record kept beside the chain, with its actor, its account, and its
reason text, is covered by no seal and no link. The dossier names the decider from the sealed entry
and shows a record that disagrees beside it, without resolving the disagreement. When a decision was
made is not sealed. The entry's time is bound by the link only, because a seal is made before its
entry is appended and the append truncates the time, so sealing it needs an encoding fixed on the
stored form. Adopt seals the decider as the chain shows it at adoption, so a rewrite made before
adopt is sealed as true, which the witness fence bounds as it does every other adopted field. A
rejection, a denied approval step, and the policy refusal `system:policy` records release nothing,
so no row's seal covers who made them, and they are display evidence only.

Two more limits apply. Adopting an unbound chain from a machine whose identity differs from the
servers' binds the chain to that machine's key, and the servers then refuse to start. That is an
operator's error caught at the first keyed start, not a path for the writer. And a writer can always
stop work by damaging rows. Every refusal fails closed, which the availability limit below already
states.

### Whoever holds the signing key

The key signs bundles, so its holder can sign a forged one. Three things bound the damage. The key
is published at a well-known path and can be pinned out of band, so a verifier is not taking the
server's word for which key is real. The hosted witness countersigns what it observed, so a forged
bundle also needs the witness to have seen a history that was never served. And every entry is
bound to its install identity, so a bundle signed by one install cannot be replayed as another's.

The operator who runs the install holds that key, so the operator is this adversary too, and the
cheapest forgery open to them is not a new history but an edited receipt: change what a receipt
discloses beside the chain, sign it again, and leave every link intact. That is now bound. Each
member a receipt discloses beside the links is held against something the anchored links commit:
an approval decision and a correction against their entry's content digest, an approver's reason
against the commitment its decision carries, the spec against the digest the decision and the
outcome name, the outcome under the exact digest form against its entry's content digest, and a
span beat's numbers against the path its link commits. Edit any of them and re-sign, and this
product's verifier fails the receipt, and so does every LoomSeal verifier from 1.7.0 on, the Go
verifier, its browser build, and the independent Python reference alike. Planting beside a checked
member a second one whose name differs only in case, for a reader that folds case to take instead,
fails the receipt the same way. A member nothing commits is reported as unchecked, and the verdict
says how many, rather than reading as verified.

The limit is the one every append-only record shares. The chain proves that history was not
rewritten after it was anchored or witnessed. It does not prove that an entry was true when it was
written: an operator who records a false approval at the moment records it as faithfully as a true
one. Anchors fix when history existed, so a rewrite made after an anchor shows. A witness that
countersigns heads as they are served fixes them in a record the operator does not control. Neither
makes an entry true, and before the first anchor the operator's key is the only thing the record
rests on.

### An AI agent holding a token

The agent holds one credential: a scoped SwitchTender token. It holds no SSH keys and no cloud
credentials; those are injected by the executor at run time and never pass through the agent. The
token is operator-bound, so the agent can propose runs and launch what policy allows, and it cannot
approve anything, touch identity or secrets, or read the audit surface. Every run it asks for waits
for a person's approval unless a rule with `effect: exempt` covers it, as [agent runs are held by
default](policy.md#agent-runs-are-held-by-default) describes, and it cannot write a schedule, a
trigger, or a template that would launch one later under another name. Every action it takes is
chained with its actor type recorded, and with the human it acted for named when one delegated. A
high-risk change it proposes is held by the same rules that hold a human's, decided by a human admin
the agent cannot be.

### A reader who does not trust this server

They are the design's favorite adversary. Pull the bundle, walk away, and verify it on an
air-gapped machine with an open verifier this project does not control, against a key fingerprint
pinned from somewhere other than the server. Nothing in that procedure asks the reader to believe
anything the server says. The live demo reseeds nightly, which from outside is indistinguishable
from a rewritten chain, and a witness pointed at it will refuse the rebuilt history the next
morning. That alarm is the product working, and anyone can run the exercise today.

## What this does not defend against

**A change made outside SwitchTender.** A person or process holding its own SSH key can change a
host behind the controller's back, and no audit system records what it never saw. The chain proves
the record it holds; it does not prove the record is exhaustive. For human operators this boundary
is organizational, taking the side keys away, and SwitchTender makes that workable by holding the
credentials runs use, or minting them per run and revoking them after, so nobody needs a standing
key to production. For agents it closes structurally, because an agent that was only ever given a
SwitchTender token has no path around the gate. What SwitchTender does see is the effect. A scheduled dry run flags any host or Terraform directory that no longer matches
what your automation asserts, so a change made around the gate to anything your automation manages
surfaces on the next check, and the fix goes back through the gate. Drift says what diverged, not
who changed it, and state your automation never asserts is invisible to it. [The side
door](side-door.md) puts all three parts of this in one place.

**A controller compromised before the write.** Stated above and worth repeating as a limit:
tamper-evidence begins when the entry is written. Records forged at the source are internally
consistent. Witnessing bounds when such a window could have begun; it does not prevent one.

**Perfect secret masking.** Masking recognizes known secret shapes and the values of stored
credentials, in plain text and in the encodings tools commonly print them in: hex, base64, and URL
and JSON escaping. A tool that transforms a secret some other way, or prints one the product never
held, has leaked it to the log. Treat masking as a reduction, not a guarantee, and prefer tools
that do not print secrets.

**A grade is a reading of what will run, not a proof of it.** The reversibility grade reads a
playbook and everything it pulls in: roles, their dependencies, task includes, imported playbooks,
and the Galaxy roles and collections a sync installs into the project. It reads them from the
project at the commit the run executes, and the gate fetches the project before it grades, so a
change pushed a moment before launch is the change that is graded. What it cannot read, it names in
the grade: a role or include whose name is only computed at run time, one that lives outside the
project, and a module this product does not know to be destructive. An unread include never lowers
a grade, but a rule that holds only irreversible changes will not hold one whose destructive work
sits somewhere the grade could not see. Where missing one is unacceptable, hold every change that
alters state. And a run the gate lets through executes what the branch holds when it starts, a few
seconds after the grade, so a push landing in that window runs on a grade of the commit before it.
A held run has no such window, because it is pinned to the commit its approver was shown.

**Isolation between runs on one executor.** A run without an execution image runs as the server's
own user, so while two runs overlap, either one's code can read the other's materialized
credentials. SwitchTender governs what runs. It does not sandbox mutually untrusted code on one
executor. Give such work an execution image or a worker of its own.

**Availability.** Fail-closed means exactly what it says: if the chain cannot be written or the
policies cannot be read, changes stop. An attacker who can take down the database can halt
automation. That trade is deliberate, refusing silently unrecorded changes at the cost of refusing
loudly, and it is the right trade for a system whose product is the record.

## Failure cases, stated

- **Chain append fails.** The mutation is refused with 503. Nothing executes off the record.
- **Policy store unreadable.** Submission refused. A gate that cannot be evaluated has not passed.
- **Spec changed after approval.** The run fails at claim time, naming the digest mismatch.
- **A run changed in the database after approval.** On a server with the producer identity it is
  refused before it runs, naming what does not match.
- **The chain cannot be read while a run is checked.** The run fails closed, and its error says the
  check could not be made rather than that the run was changed.
- **Project fetch fails at the gate.** The grade reads the commit the last sync left and says which
  commit that was. The execution that follows syncs the same remote and fails on the same fault.
- **Database restored from an older backup.** The head moves backward. A witness raises a finding,
  and bundles exported after the restore point no longer extend the ones exported before it.
- **Witness offline.** The witness sees nothing while it is down, and nothing records its downtime.
  When it comes back it checks the feed against its signed checkpoint, so a beat it had already
  witnessed that has since vanished or changed is still a finding. History written and removed
  entirely while it was away is outside what it saw. Anchors bound that loss independently of any
  witness. `witness serve` reports a server it cannot reach rather than going quiet.
- **Signing key lost.** A server that finds the chain bound to an install whose key it does not
  hold refuses to start, keyed or unkeyed, so no replica runs beside keyed ones checking no seal.
  Bundles, receipts, anchors and witness statements made before the loss verify against the
  pinned key as they always did. Runs waiting or in flight carry seals only that key can check,
  so they never execute. The remedy is to restore the key. With no copy anywhere, the install
  starts again as a new one: a backup restored into a new database, with a new key, a new
  install id, a new fingerprint to pin and a new witness enrollment. Anyone pinning the old
  fingerprint sees the change. History on the old chain that no bundle exported before the loss
  is not evidence anyone can sign.

If a claim on the homepage is not defended on this page, report it as a bug: either the page is
missing an argument or the homepage is overclaiming, and both deserve a fix.

For the agent boundary specifically, [the red team transcript](agent-red-team.md) is the
attempt to break it in practice rather than defend it in prose.
