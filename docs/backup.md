# Backup and restore

SwitchTender writes a portable, encrypted backup of its control-plane configuration and secrets, and
restores it into either the SQLite or the PostgreSQL backend. The same file moves a deployment to a
new host or migrates it between backends.

## What a backup contains

A backup holds the configuration and secrets a deployment needs to stand back up:

- Credentials, with their sealed secrets.
- Projects, templates, inventories, and inventory sources.
- Each imported template's binding to its AWX job template id, including the binding of a template
  since deleted, so its AWX-compatible callback address keeps answering, or keeps answering gone,
  after a restore.
- Schedules and webhook triggers.
- Users, teams, organizations, their memberships, and access grants.
- API tokens, stored and restored by their hashes, so existing tokens keep working after a restore.
- Approval policies, unless the install pins them from a file with `--policy-file`, in which case
  that file is the source of truth and is backed up alongside your other configuration.
- Custom credential types, which every typed credential injects through.

A restored schedule waits for its next real occurrence rather than firing the moment the restore
finishes. Missed occurrences are skipped the way cron skips them, so recovering from a day-old
backup does not fire the whole estate's nightly work at once.

Run history and the audit chain are not included. The audit chain has its own signed, self-verifying
export through `switchtender audit`, which keeps its integrity guarantees intact.

The estate's state history is not included either, for the same reason: it is the record of what runs
observed rather than configuration, so it belongs with the history a restore does not carry. A restored
install answers the estate from the point it starts gathering, not from before the backup. Move the
database itself if that history has to travel.

Forge account links are not included either. A link is proven through the forge's own sign-in and
recorded on the audit chain when it is made, and a restored link would let a pull request comment act
as an account with no record of the link on the chain. After a restore, each person links their GitHub
or GitLab account again on the Linked accounts page.

The signing identity is not included either, and it is the one thing to copy by hand. `producer-key.json`
sits in the state directory beside the database and is what makes a bundle attributable to this install:
every entry and a tree anchor's Merkle leaves are bound to the install id derived from that key. A
deployment restored without it will not mint a replacement: once a chain names an install, the server and
every command refuse to sign or bind as another one, because a new key would sign as a different install
and no earlier entry would verify under it. The server refuses to start, keyed or unkeyed, since a server
without the key over that chain would check no seal and pass no gate beside servers that hold it.
Commands that record a change still record it, unbound, and say why, so a backup can still be taken. Copy
the file with the same care as the encryption key, and keep it out of the same place if you keep the
backup somewhere a reader could reach both. The remedy for a missing key is to restore it. With no copy
anywhere, see [Signing key lost](#signing-key-lost).

Against PostgreSQL there is no directory beside the database, so the key has to be supplied rather than
found: set `SWITCHTENDER_AUDIT_KEY` to one seed on every process, or place the same `producer-key.json` in
each host's identity directory. A shared database with no identity supplied will not mint a per-host key,
because two replicas signing as two installs is a fleet whose own anchors disagree with it. The server
still starts and still records the chain. It warns, and runs with the chain unattributed and unbound, so
its entries commit to no install and its receipts can be lifted onto another one. Supply the key before
the receipts matter to anybody. Once any process has signed the chain, a process without the seed refuses
to start. The first keyed start binds the chain, so from then on no replica without the key runs beside
the ones that hold it.

## Signing key lost

A server that finds the chain bound to an install whose key it does not hold refuses to start. If a copy
of `producer-key.json` or the seed exists anywhere, put it back: the server starts keyed over the same
chain. A new key would sign as a different install, and no earlier entry would verify under it, so there
is no command that hands the chain to another key.

With no copy anywhere, the chain cannot be signed again, and the install starts again as a new one:

1. Stop every server.
2. Take a backup of the install with `switchtender backup`. Commands still record unbound, so this works
   without the key.
3. Restore it into a new, empty database with `switchtender restore --db <new database>`.
4. Start the server there with a new key. A SQLite install mints one beside the new file. A shared
   database takes a new `SWITCHTENDER_AUDIT_KEY` on every process.
5. Publish the new fingerprint, enroll the new key with the witness, and have each person link their
   forge account again.
6. Keep the old database read-only as an archive.

The new chain opens with the restore and its provenance entry, which carries the instant the backup was
taken and its counts, then the empty adoption record of the first keyed start. That boundary is written
by the install that owns it.

What it costs:

- Runs waiting or in flight never execute, since nothing can check their seals, so they are resubmitted
  on the new install.
- Run and estate history stay in the old database.
- Bundles, receipts, anchors and witness statements made before the loss verify against the old pinned
  key as they always did. Nothing exports the old chain again, so history on it that no bundle exported
  before the loss is not evidence anyone can sign.
- The witness keeps the old install's record at its last countersigned head, and the new install enrolls
  as a new record under its new key. Anyone pinning the old fingerprint sees the change.

## How it is secured

The whole backup is sealed with the deployment's encryption key using AES-256-GCM before it is
written, so the file is both confidential and tamper-evident:

- Nothing is in the clear. Configuration, password hashes, and sealed secrets never appear as
  plaintext in the file.
- A restore into a deployment with a different encryption key, or of an altered file, fails the
  authentication check and imports nothing.

Because the file is sealed with the encryption key, backup and restore both require
`SWITCHTENDER_ENCRYPTION_KEY` and `SWITCHTENDER_ENCRYPTION_SALT` to be set, and a backup restores only
where the same key and salt are configured. A written file is created readable only by its owner, and
it replaces any existing file only after it is written in full.

## Back up

    SWITCHTENDER_ENCRYPTION_KEY=... SWITCHTENDER_ENCRYPTION_SALT=... \
      switchtender backup --db switchtender.db --out backup.stbak

Without `--out` the backup is written to standard output, so it can be piped or redirected. The object
counts are written to standard error, so a piped backup stays clean.

The backup reads every table at one instant, on SQLite and on PostgreSQL alike, so a server writing
while it runs cannot leave the file holding a state the database never was in.

## Restore

    SWITCHTENDER_ENCRYPTION_KEY=... SWITCHTENDER_ENCRYPTION_SALT=... \
      switchtender restore --db switchtender.db --in backup.stbak

Restore upserts every object by id. It overwrites an object that already exists with the same id and
never deletes objects that are absent from the file, so restoring into a live deployment merges rather
than replaces. Without `--in` the backup is read from standard input. Nothing is applied until the
whole file has decrypted and decoded, so a corrupt or truncated file cannot leave a half-applied
restore.

A backup file names its format version. This release writes version 3 and restores versions 2 and 3.
A release that reads only version 2 refuses a version 3 file rather than restoring part of it,
because version 3 carries objects that release does not know, such as notification targets and a
template's sealed provisioning callback key. Restore a backup with the release that wrote it or a
later one.

## Moving from SQLite to PostgreSQL

Backup and restore are also the growth path. An install that started on SQLite moves to
PostgreSQL with the same two commands, because both accept either a file path or a
`postgres://` DSN:

    switchtender license install team.json
    SWITCHTENDER_ENCRYPTION_KEY=... SWITCHTENDER_ENCRYPTION_SALT=... \
      switchtender backup --db switchtender.db --out move.stbak
    SWITCHTENDER_ENCRYPTION_KEY=... SWITCHTENDER_ENCRYPTION_SALT=... \
      switchtender restore --db "postgres://user:pass@host/db" --in move.stbak
    switchtender serve --db "postgres://user:pass@host/db"

The restore initializes the PostgreSQL schema, which requires a Team license, so install the
license first. Use the same encryption key and salt on both sides, since they seal the file and
the stored secrets.

Two things stay behind by design. Run history and the audit chain remain in the SQLite file,
which stays valid evidence: keep the file, and every receipt minted from it verifies offline
exactly as before. New history accrues on PostgreSQL from the first run there.
