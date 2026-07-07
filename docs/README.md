# Yardmaster documentation

Yardmaster runs Ansible playbooks across a fleet and treats every run as structured data. One
binary: `serve` is the API, the executor, the scheduler, and the web UI; `worker` adds capacity.
State lives in one database, SQLite by default or PostgreSQL by DSN.

- [Quickstart](quickstart.md): zero to a first run in a few minutes.
- [Switching from AWX](switching-from-awx.md): import what you have, or set it up from scratch.
- [Configuration](configuration.md): every command, flag, and environment variable.
- [Concepts](concepts.md): runs, splits, pipelines, projects, templates, inventories, sources,
  triggers, credentials, teams, grants, queues, and workers.
- [Migration](migration.md): move off AWX or Semaphore with one command.
- [Comparison](comparison.md): how Yardmaster compares to AWX and Semaphore.

For deployment, the repository root holds a `docker-compose.yml` for a server, a database, and a
worker, and `deploy/helm` holds a Helm chart.
