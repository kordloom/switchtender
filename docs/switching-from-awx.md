<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Switching from AWX

This guide assumes you know AWX and have never run SwitchTender. It gets you from an AWX setup to a
working SwitchTender run two ways: import what you already have, or build it from scratch to learn the
pieces. If a term is unfamiliar, the [concepts](concepts.md) page defines it.

## What is different, in one paragraph

SwitchTender runs the same playbooks against the same inventories, and drives Bash, Terraform, and
Python besides, but there is no Kubernetes, no Redis, and no separate task engine to operate. One
binary is the API, the executor, the scheduler,
and the UI. State is one database: a SQLite file to start, or PostgreSQL when you want more than one
instance. You still have projects, inventories, templates, surveys, schedules, and credentials. They
just live behind a smaller, faster surface.

## The mental model

| In AWX | In SwitchTender |
|--------|---------------|
| Organization | No direct equivalent. Scope access with teams and grants instead.|
| Project (git) | Project.|
| Inventory | Stored inventory, or a dynamic inventory source that refreshes into one.|
| Job template | Template.|
| Survey | Template survey, the same typed questions.|
| Schedule | Schedule, cron instead of a recurrence rule.|
| Credential | Credential, secret sealed at rest.|
| Job | Run.|
| Job slicing | Split, balanced by measured host duration.|
| Workflow | Pipeline, ordered steps or a dependency graph.|
| Instance group | Worker queue.|
| Execution environment | A container image pinned on a project.|

## Path A: import your AWX

The fast path. It reads an AWX export and creates the equivalent SwitchTender objects.

1. Export from AWX with its own CLI: `awx export` produces a JSON document of your projects, inventories, job
   templates, credentials without secrets, schedules, and surveys.

2. Preview the import. Nothing is written yet. You get a report of exactly what would be created and
   every warning.

        switchtender import awx awx-export.json --db switchtender.db

3. Apply it.

        switchtender import awx awx-export.json --db switchtender.db --apply

4. Re-enter secrets. Exports never contain secrets, so credentials arrive as named shells. The
   report lists which ones to fill in. Open the UI, go to Credentials, and set each secret. Until
   then, everything else is already in place.

5. Launch a template and watch it run.

The full mapping and its limits are in the [migration guide](migration.md).

## Path B: set it up from scratch

Do this once to understand the pieces, even if you imported. It mirrors the order you would build a
job template in AWX.

### 1. Start the server

    SWITCHTENDER_ENCRYPTION_KEY=change-me SWITCHTENDER_ENCRYPTION_SALT=change-me-too \
      ./switchtender serve --addr :8080 --db switchtender.db

The key and salt seal credentials at rest. Keep the salt stable across restarts. Open
http://localhost:8080 for the UI. The API is open until you create the first account or token, so
you can set up before locking it down.

### 2. Create your first account

    SWITCHTENDER_PASSWORD=secret ./switchtender user new admin-you --role admin --db switchtender.db

Sign in through the UI with that username and password. Roles are admin, operator, and viewer: admins
manage configuration, operators launch and cancel runs, viewers read.

### 3. Add a project

A project is a git repository your playbooks live in. In the UI, open Projects and add one with its
repository URL and branch. For a private repository, first add an SSH key credential (next step) and
select it on the project. Every run records the exact commit it executed.

### 4. Add credentials

Open Credentials and add what your runs need. Kinds:

- `ssh_key`: an SSH private key, used to reach hosts and to clone private git projects.
- `vault_password`: an Ansible Vault password.
- `env`: `KEY=VALUE` lines injected into the run, how cloud SDK credentials reach plugins.
- `token`: a single API token or JWT, exposed to the run as the `SWITCHTENDER_TOKEN` environment variable.
- `become_password`: a privilege escalation password, delivered without touching the command line.
- `registry`: a container registry login, for pulling a pinned execution image.

Secrets are encrypted at rest and never returned by the API.

### 5. Add an inventory

Open Inventories and paste an inventory, or point a dynamic source at an inventory plugin or script
that refreshes into one. An inventory is referenced by id, so any run or template can target it.

### 6. Create a template

A template is the equivalent of an AWX job template: a saved preset of a project, a playbook path, an
inventory, a shard count, credentials, and extra vars. Add one in Templates. Give it a survey if you
want typed prompts at launch. Launch it with one click.

### 7. Watch the run

The run detail page paints a host-by-task matrix live as the run executes, with per-task drill-down
into stdout, stderr, return code, and diff. This is the part AWX does not do. A run is structure, not
a text scroll.

### 8. Add capacity and schedules

Point a worker at the same database to add an executor, and give it a queue name to target specific
work:

    ./switchtender worker --db switchtender.db --name worker-1

Add a schedule in Schedules with a cron expression to fire a template on a cadence.

## Where things live differently

- There is no separate "launch" wizard the size of AWX's. A template launch is one request, a survey
  renders as a small form.
- Workflows are built on the canvas at Workflows: add steps, drag them into place, wire dependencies
  by dragging from a step's edge onto another, and run the graph as a pipeline.
- Access is a global role plus optional per-object grants, rather than AWX's organization tree. Grant
  a user or a team `use` or `manage` on a specific project, template, inventory, or credential.
- Notifications are finish webhooks, email, and Slack today, not the full AWX set.

## What is not one to one yet

- The notification integration catalog is smaller than AWX's.
- Execution environments are a single pinned container image behind a flag, not a managed catalog.

If something you rely on is missing, open an issue. The gap with AWX is being closed on purpose.
