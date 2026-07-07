<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="160">
  </picture>
</p>

# Yardmaster CLI

One binary, a handful of commands. This page is a map; the full flag and environment reference is in
[docs/configuration.md](../docs/configuration.md).

## serve

Runs the HTTP API, the executor, the cron scheduler, the retention sweeper, and the web UI.

    yardmaster serve --addr :8080 --db yardmaster.db

The `--db` value is a SQLite file path, or a `postgres://` DSN for the PostgreSQL backend. The
schema carries forward on every open, so upgrading in place is just restarting with the new binary.

## worker

Leases pending runs from the shared store and executes them. Point a worker and a server at the
same database, a PostgreSQL DSN for separate machines, and they compete for work.

    yardmaster worker --db postgres://user:pass@host:5432/yard?sslmode=disable --name worker-1

## import

Migrates from AWX or Semaphore. Reports what it would create, then writes it with `--apply`.

    yardmaster import awx export.json --db yardmaster.db --apply

## token

Manages API tokens. Creating the first token turns authentication on.

    yardmaster token new --name ci --db yardmaster.db

## user

Manages accounts with admin, operator, and viewer roles.

    yardmaster user new operator-jane --role operator --db yardmaster.db

## version

Prints the build version.
