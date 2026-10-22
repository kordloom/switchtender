# Hosted witness

The hosted witness is a push witness KordLoom operates at `https://witness.switchtender.com`,
outside your install. An enrolled install sends it every span beat with the tree root at that beat
and a consistency proof from the root the witness last signed. The witness checks the proof from
hashes alone, writes the new root to its memory, and only then countersigns. A chain rewritten at
or before any position it countersigned is never countersigned again, and no export from that chain
reads `VERIFIED` when `switchtender verify --witness` holds it to the witness's own signed
statement.

It is included on Enterprise and sold to Team as an add-on. The same witness ships in the binary as
`switchtender witness serve`, so you can also run one yourself, as the last sections describe.

## What it receives and what it keeps

An install is one producer signing key. The witness keeps its memory under that key's id, the
`sha256:` fingerprint every bundle carries, and the install id is a label the key was born to. Only
the holder of a key can sign for it, so no install can occupy, push for, or block another
install's memory.

At enrollment the witness receives the producer public key, the install id, and the license file,
which it verifies against the same keys compiled into the binary. On every push it receives one
beat: its number, its time, its chain position, the entry hash at that position, the tree size and
root, and a consistency proof, all signed by the producer key. Each of those is a counter, a time,
or a hash. No audit entry, run content, host name, credential, or personal data is sent.

Per install it stores the producer key and its id, the install id, the license id and the end of
its term, when the install enrolled, the signed checkpoint of the tree it holds, a log of every
coordinate it countersigned, and the record of its refusals and notes. The license's organization
name is read to check the license and is never written. The witness stores no URL, organization,
contact, or address, and no run content.

The service runs on a DigitalOcean droplet and listens only on loopback. A Cloudflare Tunnel is its
one path from the internet, and Cloudflare terminates TLS, so Cloudflare's edge logs hold the
address your server connects from for Cloudflare's retention period. The witness itself writes no
address. [Vendor risk](vendor-risk.md#subprocessors) names both companies.

## Enroll

Enrollment needs a license that includes the hosted witness: Enterprise, or Team with the add-on.
Run it once on the install, pinned to the witness key below:

    switchtender witness enroll --db /var/lib/switchtender/switchtender.db \
      --witness https://witness.switchtender.com \
      --witness-key sha256:82047e9e6247134152a5ca482c61c1f642899586c6bc5562f1dbe3e110ca2804

The request is signed by the install's producer key and carries the license file, and the answer
is signed by the witness key. With `--witness-key`, an answer signed by any other key is refused and
nothing is written. The enrollment is written to `witness-enrollment.json` beside the license file,
so a backup and a restore carry both or neither, and it is recorded on the audit chain. It holds no
secret, because every push is authenticated by the producer signature. Enrolling again answers the
same.

Then start the server with span beats on, for example `--span-cadence 60s`. From that start it
pushes every beat it appends. An enrolled install refuses to start without `--span-cadence`, since
it would run unwitnessed while its enrollment says otherwise. At start it holds the audit chain to
the head the witness countersigned and refuses to start on a chain that does not reach it. A
witness that does not answer at start is a warning, and the server starts.

`GET /v1/audit/witness`, admin only, reports the witness and its key id, the newest beat
countersigned since the process started, the queue, and the last error, refusal, and note. The
server logs the same standing in one line at start.

One license enrolls up to ten installs, counted as producer signing keys. An HA pair whose servers
share one producer identity is one install. Each server pushes the beats it appended, and a beat
that another server's push has already passed is stale: dropped, counted in the status, and never
a refusal.

A rotated identity, an install whose key was changed under its install id through
`SWITCHTENDER_AUDIT_KEY`, cannot enroll. The witness binds an install id to the key it was born
to, and `witness enroll` refuses such an identity before it sends anything. A succession link from
an old key to a new one is not built yet. An install started again after its key was lost enrolls
as a new record under its new key, and the old record keeps its last countersigned head.

## The witness key

The witness key id is `sha256:82047e9e6247134152a5ca482c61c1f642899586c6bc5562f1dbe3e110ca2804`.

Its public key is `akNXijRpCjWw1CbuJ3xPWJMHpgghStLfzEd9qwEeFAM=` in base64, which is
`6a43578a34690a35b0d426ee277c4f589307a608214ad2dfcc477dab011e1403` in hex, valid from
2026-10-22T02:10:33Z. The witness also publishes it in a key list it signs, at
`https://witness.switchtender.com/.well-known/loomseal.json`, and in every enrollment answer. This
page is in the repository's history and in every release that carries it, so pin from here and
compare with the key list and with what `witness enroll` prints.

The key is one ed25519 key, kept as a file readable only by the account that runs the witness, mode
0600, on the witness host. KordLoom holds encrypted backups of it, never on the witness host, and
no snapshot of the witness's state carries it. There is no hardware security module. Root on the
witness host, or whoever holds a backup and its passphrase, can sign false countersignatures, and
this page does not claim otherwise.

When the key rotates, the witness publishes a new key list signed by the new key and endorsed by
the retired one. `switchtender witness enroll --refresh-keys` fetches it and keeps it only when a
key the enrollment already holds signed it, and a running server takes it the next time the
witness answers with the new key. Countersignatures a retired key made before it retired verify
against the list for as long as anyone keeps them, and this page then carries the new id. A
rotation after a compromise is a published statement that the retired key's later signatures are
not the witness's word, not a revocation a verifier learns of offline.

## Check an export against the witness

A relying party needs the export, the producer key id that signed it, and the witness's statement
for that key. Fetch the statement first and export second, so the export reaches the newest entry
the statement names. `PRODUCER_KEY_ID` stands for the install's `sha256:` key id, which
`switchtender audit bundle` prints and every bundle carries:

    curl -fsS -o statement.json \
      https://witness.switchtender.com/v1/installs/PRODUCER_KEY_ID/attestation
    switchtender audit bundle --db /var/lib/switchtender/switchtender.db --out bundle.json
    switchtender verify bundle.json --pubkey PRODUCER_KEY_ID --witness statement.json \
      --witness-pubkey sha256:82047e9e6247134152a5ca482c61c1f642899586c6bc5562f1dbe3e110ca2804

With `--witness`, verify holds the export to the statement in order. The statement's signature
verifies and its signer is the pinned witness key. The statement is for the producer key that
signed the export and names the same install id. The witness refused no push from this install.
The export carries the entry the witness countersigned last, with the link the witness
countersigned there, and when the export holds every entry through the tree size the statement
names, the tree over them has the witness's root.

A chain rewritten at or before that entry fails, naming the position. Once the witness has refused
a push from the install, every export held to its statement fails on the refusal count instead,
and the findings route names the position. A statement for another producer key fails naming both
key ids, and never says the chain was rewritten. An export that ends
before the witness's newest entry, or does not carry it, is not checkable, and neither is a
statement of a witness that holds nothing for the install yet. Not checkable never reads
`VERIFIED`. A run receipt usually ends before the newest entry, so hold a full bundle to the
statement. A sparse receipt carries tree leaf hashes rather than the chain links a witness
countersigns, so it is not checkable under `--witness` at all.

Without `--witness`, every countersignature an export carries is still verified and counted on the
`witness` line, and one that does not verify fails the export, since the entry it names was altered
or the countersignature was forged. `--witness-pubkey` counts only the pinned witness, and a
countersignature by any other key is named and not counted. That shows the witness signed those
entries. Only the statement shows the chain was not rewritten since.

The witness also answers each push with a head attestation over the tree root. The install stores
it, and it rides only on a tree bundle that holds every entry through that size. `switchtender
verify` refuses a bundle that carries one by name and points to the open LoomSeal verifier, so only
the countersignatures on entries count here.

## Refusals, notes, and what verify requires

A refusal is the witness's testimony that a push signed with the install's key contradicted what
it remembers of that install: a tree at or past the size it holds that does not extend the root it
holds, or a beat, position, or link that disagrees with one it already countersigned. The kinds are
`rewritten_history`, `rewritten_beat`, `seq_regression`, `head_regression`, and `duplicate_beat`.
Nothing is adopted or countersigned, and the refusal is counted in the statement's
`refusals_total`. `verify --witness` requires `refusals_total` to be zero, so a refusal fails every
export held to that record for the life of the record. The only release is KordLoom deleting the
record, after which the install enrolls again into a fresh memory whose first note says where it
starts.

A note is about the witness's own coverage and changes nothing about what was countersigned. The
kinds are `first_root`, where the witness's memory of the install starts, `missing_beat`, beats it
never received while the proof shows the chain intact, `memory_restored`, a rewind of the
witness's own memory, and `silent_install`, an install that pushed nothing the witness could hold
against its memory for a day. Notes are counted in `notes_total`, which verify prints at the end of
the `witness` line and never fails on.

A 409 that names the size the witness holds is negotiation, not a finding. The install proves again
from that size, or drops the beat as stale when the witness already holds a tree at or past it. A
400 is a push the witness could not check, which is dropped and recorded nowhere. `GET
/v1/installs/PRODUCER_KEY_ID/findings` lists both classes with both totals.

A chain cut back below the size the witness holds is stale on every push, and it is recorded as
`rewritten_history` once it grows back to that size. Until then every export from it ends before
the witness's newest entry and is not checkable, and `silent_install` is noted after a day.

## Outages and the queue

When the witness cannot be reached, the server keeps beating and anchoring, and the timestamp anchor
and the witness push never wait on each other. Pushes queue in memory, in order, up to one day of
beats at the cadence. Each is proved at the moment it is sent from the size the witness last
acknowledged, so a push dropped from a full queue never breaks the next proof. Past a day the
oldest push is dropped with a log line, the status's `dropped` counter rises, and the witness later
notes the gap as `missing_beat`. A network error or a 5xx answer is an outage, never a refusal.

The witness takes a burst of sixty pushes from one install and then one every ten seconds, so a
full day's queue drains over several hours once the witness answers again. A bundle exported during
an outage carries countersignatures through the last beat the witness acknowledged and none after,
and verify reports the newest countersigned entry and nothing it did not check.

The queue does not survive a restart, and neither does the status's newest countersigned beat:
after a restart the start line says nothing has been countersigned since the start, while every
countersignature already received stays in the database. The first push after a start is answered
with the size the witness holds, and the install proves again from there.

When the witness itself is restored from a snapshot, its memory rewinds to the snapshot. At its
next start it records one `memory_restored` note per install naming the size it now holds, and
each install's next push proves again from there. Countersignatures issued after the snapshot still
verify by their signatures, but the witness no longer holds those entries. The entries after the
restored size, up to each install's first push after the restore, are the window a relying party
weighs, and the note names where it starts.

## Lapse and retention

The license term in the enrollment is the clock. For thirty days after the term ends the witness
still countersigns the install's pushes, so a renewal that lands late costs no coverage. After that
it answers each push with 402, and the server logs it, marks its status lapsed, and stops pushing.
For twelve months more the record is read only: the statement and the findings stay readable, so an
auditor asking about the covered period still gets an answer, and the record still counts toward
its license id's cap of ten installs. Then the record is deleted. Every countersignature the install
stored, and every export that carries one, keeps verifying against the published key.

A renewal that arrives while the record is still held reopens it with its memory. Install the new
license file, run `switchtender witness enroll` again so the witness records the new term, and
restart the server if it already stopped pushing. A fresh memory starts only after the record is
deleted, not at lapse.

## Run your own push witness

`switchtender witness serve --accept-enrollments --enrollment-policy open` runs the same push
witness on your own network. Under `open` it accepts any enrollment whose signature and label
verify, with no license and no term, so who can reach the witness at all is the control. Under
`licensed`, the policy the hosted witness runs, it enrolls only an install whose license includes
the hosted witness, up to `--max-installs-per-license` producer keys per license.

`witness enroll` needs a license that includes the hosted witness whichever witness it names, so an
install enrolls with a witness of its own under Enterprise or the Team add-on. An install accepts a
plain-http witness only on a loopback address. To move an install from one witness to another,
remove `witness-enrollment.json` and enroll again. Watching a server's beat feed with
`switchtender witness` or `switchtender witness serve --watch` needs no license on either side.

A witness listening off loopback needs `--api-token` for the `/witness` routes, which list watched
servers and their findings. The push routes stay open: a push is authenticated by its signature,
and a read by producer key id reveals only counters, heads, and findings.
[deploy/witness](https://github.com/kordloom/switchtender/tree/main/deploy/witness) is the runbook
the hosted witness is deployed from.

## Limits

- The witness bounds history only from the first root it countersigned for an install. History
  before enrollment is bounded by anchors.
- Between two beats it sees nothing. A rewrite made and undone inside one cadence is invisible to
  it, as to any witness, and the cadence is yours to choose.
- Before the witness holds a countersigned head for an install, it bounds nothing. `switchtender
  adopt` holds the chain to the witness's head only once there is one. Adopt also refuses when the
  install's own store holds countersignatures while the witness signs that it holds nothing, but
  that tripwire lives in the database, so a writer who rewrites the chain can delete the
  countersignatures too.
- Root on the witness host, or a holder of a key backup and its passphrase, can sign false
  countersignatures.
- Only the countersignatures on entries count in `switchtender verify`. Head attestations are
  refused by name.
- A sparse receipt is not checkable under `--witness`.
- The status's newest countersigned beat and the queue are held in memory and start empty after a
  restart.
- A rotated identity cannot enroll until a succession link ships.
- On an active-active install a beat another server's push already passed is stale and not
  countersigned, so coverage has small gaps.
- The witness can see which producer keys are enrolled and when they push. That is metadata, not
  run content, host names, credentials, or personal data.
