<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Migrate off AWX or Semaphore

SwitchTender imports an AWX or Semaphore export and creates the equivalent objects, so moving over is
one command rather than a rebuild.

## Get an export

- AWX: `awx export` produces a JSON document of organizations, projects, inventories, job
  templates, credentials without secrets, schedules, and surveys.
- Semaphore: export the project's repositories, inventories, keys, templates, and schedules as
  JSON.

## Preview, then apply

Run the import without `--apply` first to see exactly what it would create, along with every
warning:

    switchtender import awx awx-export.json --db switchtender.db

The report lists the projects, inventories, credentials, templates, and schedules that will be
created, and calls out anything that could not be mapped cleanly. Apply it when the report looks
right:

    switchtender import awx awx-export.json --db switchtender.db --apply

Semaphore works the same way with `import semaphore`.

## Import a crontab

Fleets that still schedule work from a crontab can bring it under governance in one step. Point the
importer at a crontab file and it turns each cron line into a governed schedule, so every firing is
approved, recorded, and provable like any other run instead of running unseen on one box.

    switchtender import cron /var/spool/cron/crontabs/deploy --inventory prod --db switchtender.db

A crontab names no target host, so `--inventory` says where the jobs run. Add `--system` to read
`/etc/crontab` and the system tabs, which carry a user column the report calls out. Comments and
environment lines are noted and skipped. As with the other imports, leave off `--apply` to preview
the schedules first, then re-run with `--apply` to create them.

## What maps to what

| Source | Becomes |
|--------|---------|
| AWX git project | Project.|
| AWX inventory | Stored inventory, rendered as INI from its hosts and groups.|
| AWX job template | Template, with job slicing becoming shard count.|
| AWX survey | Template survey, field for field, with the field types translated.|
| AWX schedule | Schedule, with the recurrence rule converted to cron.|
| AWX credential | Credential shell with its kind mapped from its type and its configured inputs, secret omitted.|
| Semaphore repository | Project.|
| Semaphore static inventory | Stored inventory.|
| Semaphore template | Template, with survey variables mapped.|
| Semaphore key | Credential shell.|
| Semaphore schedule | Schedule.|

## Re-enter secrets

Exports never contain secrets by design, so credentials import as named shells with their kind set
but no material. The report lists which secrets to re-enter. Everything else, projects,
inventories, templates, and schedules, is in place and ready to run once the secrets are restored.

## Limits to expect

- A schedule whose cadence a cron expression cannot represent, such as every third day, is reported
  and skipped rather than converted to a wrong cadence.
- Non-git projects are skipped, since there is no repository to source playbooks from.
- A credential type without an exact match maps to the environment kind and is flagged for review.
- Secrets are never in an export, so every credential is created as a shell and its secret has to be
  re-entered. The non-secret settings AWX did export, such as the user to connect as and how to
  become root, are stored on the credential itself and take effect at injection, so a machine
  credential arrives knowing its connection user and only the secret needs entering. Only known
  non-secret fields are stored, so a custom credential type cannot spill a secret into the plan.
- An AWX machine credential covers both key and password login under one type. Which one it is comes
  from whether the export configured a key or a password, so a password credential does not arrive as
  a key credential that fails the first time it runs.
