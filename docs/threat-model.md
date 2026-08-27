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
because the submission, the hold, the decision, and the outcome are all separate chain entries,
each naming its actor.

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

### Whoever holds the signing key

The key signs bundles, so its holder can sign a forged one. Three things bound the damage. The key
is published at a well-known path and can be pinned out of band, so a verifier is not taking the
server's word for which key is real. The hosted witness countersigns what it observed, so a forged
bundle also needs the witness to have seen a history that was never served. And every entry is
bound to its install identity, so a bundle signed by one install cannot be replayed as another's.

### An AI agent holding a token

The agent holds one credential: a scoped SwitchTender token. It holds no SSH keys and no cloud
credentials; those are injected by the executor at run time and never pass through the agent. The
token is operator-bound, so the agent can propose runs and launch what policy allows, and it cannot
approve anything, touch identity or secrets, or read the audit surface. Every action it takes is
chained with its actor type recorded, and with the human it acted for named when one delegated. A
high-risk change it proposes is held by the same rules that hold a human's, decided by a human
admin the agent cannot be.

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
is organizational, taking the side keys away. For agents it closes structurally, because an agent
that was only ever given a SwitchTender token has no path around the gate.

**A controller compromised before the write.** Stated above and worth repeating as a limit:
tamper-evidence begins when the entry is written. Records forged at the source are internally
consistent. Witnessing bounds when such a window could have begun; it does not prevent one.

**Perfect secret masking.** Masking recognizes known secret shapes and the values of stored
credentials. A tool that prints a secret in a form the masks do not recognize has leaked it to the
log. Treat masking as a reduction, not a guarantee, and prefer tools that do not print secrets.

**Availability.** Fail-closed means exactly what it says: if the chain cannot be written or the
policies cannot be read, changes stop. An attacker who can take down the database can halt
automation. That trade is deliberate, refusing silently unrecorded changes at the cost of refusing
loudly, and it is the right trade for a system whose product is the record.

## Failure cases, stated

- **Chain append fails.** The mutation is refused with 503. Nothing executes off the record.
- **Policy store unreadable.** Submission refused. A gate that cannot be evaluated has not passed.
- **Spec changed after approval.** The run fails at claim time, naming the digest mismatch.
- **Database restored from an older backup.** The head moves backward. A witness raises a finding,
  and bundles exported after the restore point no longer extend the ones exported before it.
- **Witness offline.** The gap shows up as an unattested window in verification output, reported
  with its duration rather than hidden. Attestation coverage is a claim the verifier checks, not
  an assumption.
- **Signing key lost.** Old bundles still verify against the pinned key. New exports need a new
  key, and the change of key is itself visible to anyone pinning the old fingerprint.

If a claim on the homepage is not defended on this page, report it as a bug: either the page is
missing an argument or the homepage is overclaiming, and both deserve a fix.
