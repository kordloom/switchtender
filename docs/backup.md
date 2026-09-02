# Backup and restore

SwitchTender writes a portable, encrypted backup of its control-plane configuration and secrets, and
restores it into either the SQLite or the PostgreSQL backend. The same file moves a deployment to a
new host or migrates it between backends.

## What a backup contains

A backup holds the configuration and secrets a deployment needs to stand back up:

- Credentials, with their sealed secrets.
- Projects, templates, inventories, and inventory sources.
- Schedules and webhook triggers.
- Users, teams, organizations, their memberships, and access grants.
- Approval policies, unless the install pins them from a file with `--policy-file`, in which case
  that file is the source of truth and is backed up alongside your other configuration.
- Custom credential types, which every typed credential injects through.

A restored schedule waits for its next real occurrence rather than firing the moment the restore
finishes. Missed occurrences are skipped the way cron skips them, so recovering from a day-old
backup does not fire the whole estate's nightly work at once.

Run history and the audit chain are not included. The audit chain has its own signed, self-verifying
export through `switchtender audit`, which keeps its integrity guarantees intact.

The signing identity is not included either, and it is the one thing to copy by hand. `producer-key.json`
sits in the state directory beside the database and is what makes a bundle attributable to this install:
a tree anchor's Merkle leaves are bound to the install id derived from that key, so a deployment restored
without it signs as a different install and its own anchors can no longer be recomputed. Copy the file
with the same care as the encryption key, and keep it out of the same place if you keep the backup
somewhere a reader could reach both. An anchor records which install took it, so a mismatch is reported
as an identity that does not match rather than as a chain that was rewritten, but the remedy is still to
restore the key.

Against PostgreSQL there is no directory beside the database, so the key has to be supplied rather than
found: set `SWITCHTENDER_AUDIT_KEY` to one seed on every process, or place the same `producer-key.json` in
each host's identity directory. A shared database with no identity supplied will not mint a per-host key,
because two replicas signing as two installs is a fleet whose own anchors disagree with it. The server
still starts and still records the chain; it warns, and runs with the chain unattributed and unbound, so
its entries commit to no install and its receipts can be lifted onto another one. Supply the key before
the receipts matter to anybody.

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

## Restore

    SWITCHTENDER_ENCRYPTION_KEY=... SWITCHTENDER_ENCRYPTION_SALT=... \
      switchtender restore --db switchtender.db --in backup.stbak

Restore upserts every object by id. It overwrites an object that already exists with the same id and
never deletes objects that are absent from the file, so restoring into a live deployment merges rather
than replaces. Without `--in` the backup is read from standard input. Nothing is applied until the
whole file has decrypted and decoded, so a corrupt or truncated file cannot leave a half-applied
restore.

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
