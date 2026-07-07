<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Configuration

Yardmaster is one binary with subcommands. This page lists every command, flag, and environment
variable.

## Environment variables

| Variable | Used by | Purpose |
|----------|---------|---------|
| `YARDMASTER_ENCRYPTION_KEY` | serve, worker | Seals stored credentials with AES-256-GCM. Credentials are disabled when unset. |
| `YARDMASTER_PASSWORD` | user new | Initial account password, read instead of prompting so it never lands on the command line. |
| `YARDMASTER_SMTP_PASSWORD` | serve | Password for SMTP authentication when `--smtp-username` is set. |

## serve

Runs the HTTP API, the in-process executor, the scheduler, the retention sweeper, and the web UI.

| Flag | Default | Purpose |
|------|---------|---------|
| `--addr` | `:8080` | Address the server listens on. |
| `--db` | `yardmaster.db` | SQLite file path, or a `postgres://` DSN for the PostgreSQL backend. |
| `--schedule-interval` | `15s` | How often the scheduler checks for due schedules. |
| `--notify-webhook` | none | URL that receives a JSON notification when a run finishes. Repeatable. |
| `--allow-container-ee` | `false` | Allow runs whose project pins a container image to execute inside it. Needs Docker on the executor. |
| `--strict-grants` | `false` | Deny non-admins access to an object that has no grants, instead of deferring to the global role. |
| `--retain-runs` | none | Delete terminal runs older than this, for example `90d`. Empty keeps them forever. |
| `--retain-events` | none | Drop run events and logs older than this, for example `30d`. Empty keeps them forever. |
| `--retention-interval` | `1h` | How often the retention sweeper runs. |
| `--smtp-addr` | none | SMTP server host:port for run notification emails. Empty disables email. |
| `--smtp-from` | none | Sender address for notification emails. |
| `--smtp-to` | none | Recipient address for notification emails. Repeatable. |
| `--smtp-username` | none | SMTP username. The password comes from `YARDMASTER_SMTP_PASSWORD`. |
| `--notify-on` | `failure` | When to email: `failure` for failed runs only, or `finish` for every terminal run. |

Retention windows accept a whole number of days with a `d` suffix, such as `30d`, or Go duration
syntax such as `720h`.

## worker

Leases pending runs from the shared store and executes them. Point it and a server at the same
database and they compete for work.

| Flag | Default | Purpose |
|------|---------|---------|
| `--db` | `yardmaster.db` | SQLite file path, or a `postgres://` DSN. |
| `--name` | host and pid | Worker name stamped on the runs it executes. |
| `--queue` | none | Queue this worker serves. Repeatable. Without any, it serves the default pool. |
| `--allow-container-ee` | `false` | Allow container execution environments on this worker. Needs Docker. |

## token

Manages API tokens. Creating the first token turns on authentication.

- `token new --name <label> [--ttl <duration>]` mints a token, printed once. A zero TTL never
  expires.
- `token list` lists tokens without their secrets.
- `token revoke <id>` deletes a token.

All token subcommands take `--db` and the global `--pretty` flag for indented JSON.

## user

Manages accounts with roles: admin, operator, and viewer.

- `user new <username> --role <role>` creates an account. The password comes from
  `YARDMASTER_PASSWORD` or a prompt, never an argument.
- `user list` lists accounts.
- `user delete <id>` deletes an account; its tokens stop working.

All user subcommands take `--db`.

## import

Migrates from AWX or Semaphore. See [Migration](migration.md).

- `import awx <export.json> [--apply]`.
- `import semaphore <export.json> [--apply]`.

Both take `--db` for the target database. Without `--apply` the command only reports what it would
create.

## demo

Seeds a fresh database with sample data and real runs, then serves it read-only, so a public
instance is safe to expose. It needs ansible on the PATH to run the sample playbooks.

| Flag | Default | Purpose |
|------|---------|---------|
| `--addr` | `:8080` | Address the demo listens on. |
| `--db` | temporary file | Database to seed and serve. Empty uses a fresh temporary SQLite file. |

## Global flags

| Flag | Purpose |
|------|---------|
| `--pretty` | Indent JSON output instead of the compact default. |
