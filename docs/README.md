<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Railwarden" width="140">
  </picture>
</p>

# Documentation

Railwarden runs Ansible, Terraform, OpenTofu, Bash, PowerShell, Python, and Go across a fleet and treats every run as
structured data. One binary: `serve` is the API, the executor, the scheduler, and the web UI.
`worker` adds capacity.
State lives in one database, SQLite by default or PostgreSQL by DSN. These pages also render inside
the app at `/ui/docs`.

| Guide | What |
|-------|------|
| [Quickstart](quickstart.md) | Zero to a first run in a few minutes.|
| [Switching from AWX](switching-from-awx.md) | Import what you have, or set up from scratch.|
| [Tutorials](tutorials.md) | Task-focused walk-throughs for everyday work.|
| [Concepts](concepts.md) | Runs, splits, pipelines, projects, templates, and the rest.|
| [Reliability](reliability.md) | How runs execute: workers, splits, failure, recovery, durability.|
| [Configuration](configuration.md) | Every command, flag, and environment variable.|
| [Desktop](desktop.md) | Run Railwarden as a local desktop app.|
| [Features](features.md) | The full capability list.|
| [Advisory AI](ai.md) | The five AI features, the guarantees, providers, and what a model sees.|
| [Extend in Go](sdk.md) | The SDK: add tools, AI providers, secret engines, and notifiers.|
| [HTTP API](api.md) | Every endpoint the server exposes.|
| [Migration](migration.md) | Moving off AWX or Semaphore in detail.|
| [Comparison](comparison.md) | How Railwarden compares to AWX and Semaphore.|

For deployment, the repository root holds a `docker-compose.yml` for a server, a database, and a
worker, and [deploy/helm](../deploy/helm) holds a Helm chart.
