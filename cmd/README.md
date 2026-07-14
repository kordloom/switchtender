<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Railwarden" width="160">
  </picture>
</p>

# Railwarden CLI

One binary, a handful of commands. This page is a map; the full flag and environment reference is in
[docs/configuration.md](../docs/configuration.md).

## serve

Runs the HTTP API, the executor, the cron scheduler, the retention sweeper, and the web UI.

    railwarden serve --addr :8080 --db railwarden.db

The `--db` value is a SQLite file path, or a `postgres://` DSN for the PostgreSQL backend. The
schema carries forward on every open, so upgrading in place is just restarting with the new binary.

## worker

Leases pending runs from the shared store and executes them. Point a worker and a server at the
same database, a PostgreSQL DSN for separate machines, and they compete for work.

    railwarden worker --db postgres://user:pass@host:5432/yard?sslmode=disable --name worker-1

## import

Migrates from AWX or Semaphore. Reports what it would create, then writes it with `--apply`.

    railwarden import awx export.json --db railwarden.db --apply

## token

Manages API tokens. Creating the first token turns authentication on.

    railwarden token new --name ci --db railwarden.db

## user

Manages accounts with admin, operator, and viewer roles.

    railwarden user new operator-jane --role operator --db railwarden.db

## demo

Seeds a fresh database with sample data and real runs, then serves it read-only. Safe to expose.

    railwarden demo --addr :8080

## version

Prints the build version.
