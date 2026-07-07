<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Migration

Yardmaster imports an AWX or Semaphore export and creates the equivalent objects, so moving over is
one command rather than a rebuild.

## Get an export

- AWX: `awx export` produces a JSON document of organizations, projects, inventories, job
  templates, credentials without secrets, schedules, and surveys.
- Semaphore: export the project's repositories, inventories, keys, templates, and schedules as
  JSON.

## Preview, then apply

Run the import without `--apply` first to see exactly what it would create, along with every
warning:

    yardmaster import awx awx-export.json --db yardmaster.db

The report lists the projects, inventories, credentials, templates, and schedules that will be
created, and calls out anything that could not be mapped cleanly. Apply it when the report looks
right:

    yardmaster import awx awx-export.json --db yardmaster.db --apply

Semaphore works the same way with `import semaphore`.

## What maps to what

| Source | Becomes |
|--------|---------|
| AWX git project | Project.|
| AWX inventory | Stored inventory, rendered as INI from its hosts and groups.|
| AWX job template | Template, with job slicing becoming shard count.|
| AWX survey | Template survey, field for field, with the field types translated.|
| AWX schedule | Schedule, with the recurrence rule converted to cron.|
| AWX credential | Credential shell with its kind mapped, secret omitted.|
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
