<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Migrate off AWX, Semaphore, or Rundeck

SwitchTender imports an AWX, Semaphore, or Rundeck export, or a plain crontab, and creates the
equivalent objects, so moving over is one command rather than a rebuild.

## Get an export

- AWX: `awx export` produces a JSON document of organizations, projects, inventories, job
  templates, credentials without secrets, schedules, and surveys.
- Semaphore: export the project's repositories, inventories, keys, templates, and schedules as
  JSON.
- Rundeck: export a project's jobs as YAML or JSON, from the project's job list or the API.

## Preview, then apply

Run the import without `--apply` first to see exactly what it would create, along with every
warning:

    switchtender import awx awx-export.json --db switchtender.db

The report lists the projects, inventories, credentials, templates, and schedules that will be
created, and calls out anything that could not be mapped cleanly. Apply it when the report looks
right:

    switchtender import awx awx-export.json --db switchtender.db --apply

Semaphore works the same way with `import semaphore`.

## Import Rundeck jobs

Export the jobs from a Rundeck project, in YAML or JSON, and point the importer at the file.

    switchtender import rundeck jobs.yaml --inventory prod --db switchtender.db

Each job becomes a Bash template carrying its step sequence in order, its options become a survey,
and its schedule becomes a cron schedule. Rundeck dispatches by node filter rather than by inventory
file, so `--inventory` records which hosts the jobs are meant for and the report names any job whose
node filter you should check against it.

Know what that inventory does and does not do. A Rundeck job imports as a Bash template, and the Bash
tool runs the script where the worker runs it; the inventory is carried on the template for you to
act on, not used to fan the script out across those hosts. Rewrite the job as an Ansible template
when you want it to run against the inventory.

Two details are worth knowing before you run it. A Rundeck schedule is a Quartz expression, which
counts Sunday as one where cron counts Sunday as zero, so the weekday is renumbered rather than
copied; a Quartz-only form such as the third Friday of the month has no cron equivalent and is
reported instead of converted to a day it would fire wrongly on. And a secure option is never
imported as a survey field, because a survey answer is stored in plain text on the run; the report
names each one so you can store it as a credential instead.

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
| AWX survey | Template survey, field for field, with the field types translated. A password prompt is refused, not downgraded to plain text.|
| AWX workflow job template | Workflow template carrying the graph, with each node's job template inlined as a step and the success and always edges becoming dependencies. Imported whole or reported and skipped, never partially. *Next release.*|
| AWX schedule | Schedule, with the recurrence rule converted to cron.|
| AWX credential | Credential shell with its kind mapped from its type and its configured inputs, secret omitted.|
| Semaphore repository | Project.|
| Semaphore static inventory | Stored inventory.|
| Semaphore template | Template, with survey variables mapped.|
| Semaphore key | Credential shell.|
| Semaphore schedule | Schedule.|
| Rundeck job | Template running the job's step sequence as one Bash script.|
| Rundeck option | Survey field. An enforced value list becomes a choice; a secure option is refused, not downgraded.|
| Rundeck schedule | Schedule, with the Quartz expression converted and its weekday renumbered.|
| Rundeck dispatch thread count | Recorded on the template, and reported. It paces Ansible runs; a Bash template, which is what a Rundeck job imports as, does not read it.|

## Re-enter secrets

Exports never contain secrets by design, so credentials import as named shells with their kind set
but no material. The report lists which secrets to re-enter. Everything else, projects,
inventories, templates, and schedules, is in place and ready to run once the secrets are restored.

## Limits to expect

- A schedule whose cadence a cron expression cannot represent, such as every third day, is reported
  and skipped rather than converted to a wrong cadence.
- A workflow whose graph cannot be expressed whole is reported and skipped rather than reduced. A
  failure edge, which runs work precisely because something failed, has no pipeline equivalent, and a
  node pointing at a job template the export does not carry has no work to do. A partial graph would
  keep the workflow's name and run a subset of it, which is worse than not importing it.
- Non-git projects are skipped, since there is no repository to source playbooks from.
- A credential type without an exact match maps to the environment kind and is flagged for review.
- A survey field that prompts for a secret, an AWX password question or a Rundeck secure option, is
  reported and left out rather than imported. A survey answer is stored in plain text on the run, so
  importing one would quietly make the secret less protected than it was in the tool you left. Store
  those values as credentials and reference them from the template.
- Secrets are never in an export, so every credential is created as a shell and its secret has to be
  re-entered. The non-secret settings AWX did export, such as the user to connect as and how to
  become root, are stored on the credential itself and take effect at injection, so a machine
  credential arrives knowing its connection user and only the secret needs entering. Only known
  non-secret fields are stored, so a custom credential type cannot spill a secret into the plan.
- An AWX machine credential covers both key and password login under one type. Which one it is comes
  from whether the export configured a key or a password, so a password credential does not arrive as
  a key credential that fails the first time it runs.
