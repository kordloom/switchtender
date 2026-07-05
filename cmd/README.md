<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="160">
  </picture>
</p>

# Yardmaster CLI

One binary, a handful of commands.

## serve

Runs everything: the HTTP API, the executor, the cron scheduler, and the web UI.

    yardmaster serve --addr :8080 --db yardmaster.db

| Flag                  | Default         | What                                          |
|-----------------------|-----------------|-----------------------------------------------|
| `--addr`              | `:8080`         | Address the server listens on                 |
| `--db`                | `yardmaster.db` | Path to the SQLite database file              |
| `--schedule-interval` | `15s`           | How often the scheduler checks due schedules  |

The database file is created on first start and carries the full schema forward on every open, so
upgrading in place is just restarting with the new binary.

## version

Prints the build version.

## worker

Reserved for the distributed worker. It parses and exits today; remote execution is on the
roadmap.
