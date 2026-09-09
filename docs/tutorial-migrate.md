<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Migrate your setup

You do not rebuild your automation by hand. SwitchTender reads an AWX, Semaphore, Rundeck, or
Jenkins export, or a plain crontab, and creates the equivalent objects in one pass.

The five sources do not carry the same things. AWX and Semaphore export a whole control plane, so
they bring projects, inventories, credential shells, templates, surveys, and schedules. Rundeck and
Jenkins export jobs and nothing else, so they bring templates, surveys, and schedules, against an
inventory you name. A crontab brings schedules alone. The table is in
[what each source brings over](migration.md#what-each-source-brings-over).

## From the UI

1. Export from your current tool. `awx export` produces a JSON document. Semaphore has no single
   export command, so gather the project's repositories, inventories, keys, templates, and schedules
   from its API into one JSON document. Rundeck exports a project's jobs as YAML or JSON. Jenkins
   has no export file at all, so zip its `jobs` directory and upload that.
2. Open Migrate from the top of the overview, or go to `/ui/migrate`.
3. Choose the format: AWX, Semaphore, Rundeck, or Jenkins. Rundeck and Jenkins ask for the inventory
   their templates should target, since neither names hosts of its own.
4. Paste the export, or choose the file, and select Preview. Nothing is written yet. You get a
   report of exactly what would be created, with every warning.
5. Select Import to apply it.

A crontab imports from the command line only. The Migrate page and the `/v1/import/{format}`
endpoint take the other four.

## From the CLI

    switchtender import awx awx-export.json --db switchtender.db            # preview
    switchtender import awx awx-export.json --db switchtender.db --apply    # write

`import semaphore` reads a Semaphore export the same way. `import rundeck jobs.yaml`,
`import jenkins /var/jenkins_home`, and `import cron /etc/crontab --system` each take
`--inventory <name>`, since none of those three names hosts of its own. A cron line imports as a
shell step, and a shell step runs on the SwitchTender host rather than on the machine the crontab
came from, so naming an inventory does not move it; the report says so on every cron import. Preview
first, always. `--apply` is the only step that writes.

## Finish by setting secrets

Exports never contain secret values, so an AWX or Semaphore credential arrives as a named shell. The
report lists which ones need a secret. Fill them in once with
[set a secret](tutorial-set-a-secret.md), and everything else is already in place.

A Rundeck, Jenkins, or crontab import creates no credentials at all, because those exports hold
none. A job that needed a login needs a credential built by hand and attached to its template
afterward. A secure Rundeck option or a Jenkins password parameter is refused rather than imported
as a survey field, and the report names each one, because a survey answer is stored in plain text on
every run.

The full field-by-field mapping and its current limits are in the [migration guide](migration.md).
