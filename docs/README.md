<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Documentation

Yardmaster runs Ansible, Bash, Terraform, and Python across a fleet and treats every run as
structured data. One binary: `serve` is the API, the executor, the scheduler, and the web UI;
`worker` adds capacity.
State lives in one database, SQLite by default or PostgreSQL by DSN. These pages also render inside
the app at `/ui/docs`.

| Guide | What |
|-------|------|
| [Quickstart](quickstart.md) | Zero to a first run in a few minutes.|
| [Switching from AWX](switching-from-awx.md) | Import what you have, or set up from scratch.|
| [Tutorials](tutorials.md) | Task-focused walk-throughs for AWX and Semaphore users.|
| [Concepts](concepts.md) | Runs, splits, pipelines, projects, templates, and the rest.|
| [Configuration](configuration.md) | Every command, flag, and environment variable.|
| [Features](features.md) | The full capability list.|
| [HTTP API](api.md) | Every endpoint the server exposes.|
| [Migration](migration.md) | Moving off AWX or Semaphore in detail.|
| [Comparison](comparison.md) | How Yardmaster compares to AWX and Semaphore.|

For deployment, the repository root holds a `docker-compose.yml` for a server, a database, and a
worker, and [deploy/helm](../deploy/helm) holds a Helm chart.
