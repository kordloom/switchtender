<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Migrate your setup

You do not rebuild your automation by hand. SwitchTender reads an AWX or Semaphore export and creates
the equivalent projects, inventories, templates, surveys, schedules, and credentials in one pass.

## From the UI

1. Export from your current tool. `awx export` produces a JSON document. Semaphore exports its
   project the same way.
2. Open Migrate from the top of the overview, or go to `/ui/migrate`.
3. Paste the export and select Preview. Nothing is written yet. You get a report of exactly what
   would be created, with every warning.
4. Select Import to apply it.

## From the CLI

    switchtender import awx awx-export.json --db switchtender.db            # preview
    switchtender import awx awx-export.json --db switchtender.db --apply    # write

Use `import semaphore` for a Semaphore export. Preview first, always. `--apply` is the only step that
writes.

## Finish by setting secrets

Exports never contain secret values, so credentials arrive as named shells. The report lists which
ones need a secret. Fill them in once with [set a secret](tutorial-set-a-secret.md), and everything
else is already in place.

The full field-by-field mapping and its current limits are in the [migration guide](migration.md).
