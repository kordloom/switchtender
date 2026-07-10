<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Schedule a job

AWX schedules use a recurrence rule. Yardmaster uses a cron expression, and a schedule can fire a
stored template, a single run, a split, or a whole pipeline.

## Schedule a template

1. Save the work first as a [template](tutorial-save-a-template.md), so the schedule stays a one-line
   reference instead of a copy of every field.
2. Open Schedules and add a schedule.
3. Give it a cron expression for the cadence, for example `0 2 * * *` for 02:00 every day, and point
   it at the template.
4. Save. It fires on the schedule and each firing shows up in Runs like any other.

## From the API

    curl -s -X POST localhost:8080/schedules \
      -H 'content-type: application/json' \
      -d '{"cron":"0 2 * * *","template_id":"tpl_abc123"}'

To schedule without a template, send `playbook` and `inventory` inline, add `shards` for a split, or
`steps` for a pipeline. A worker or the server must be running for a schedule to fire; add capacity
with `yardmaster worker`.

Next: give the job its secrets with [set a secret](tutorial-set-a-secret.md).
