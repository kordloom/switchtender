<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Schedule a job

A schedule uses a cron expression, and it can fire a stored template, a single run, a split, or a
whole pipeline.

## Schedule a template

1. Save the work first as a [template](tutorial-save-a-template.md), so the schedule stays a one-line
   reference instead of a copy of every field.
2. Open Schedules and add a schedule.
3. Give it a cron expression for the cadence, for example `0 2 * * *` for 02:00 every day, and point
   it at the template.
4. Save. It fires on the schedule and each firing shows up in Runs like any other.

## From the API

    curl -s -X POST localhost:8080/v1/schedules \
      -H "Authorization: Bearer $ST_TOKEN" \
      -H 'content-type: application/json' \
      -d '{"cron":"0 2 * * *","template_id":"tpl_abc123"}'

Use a real template id: the API stores the schedule either way, and one naming a template that does
not exist fires nothing. The Doctor panel on the overview, and `GET /v1/doctor`, report a schedule in that state by id. A `name` is optional and worth setting, since it is what that report
and the schedules list call it.

To schedule without a template, send `playbook` and `inventory` inline, add `shards` for a split, or
`steps` for a pipeline. A worker or the server must be running for a schedule to fire. Add capacity
with `switchtender worker`.

By default the cron expression reads in the server's local time. Add a `timezone`, an IANA name such
as `America/New_York`, to pin it to a zone and let it follow that zone's daylight-saving shifts, so a
nightly window stays put across the year.

    curl -s -X POST localhost:8080/v1/schedules \
      -H "Authorization: Bearer $ST_TOKEN" \
      -H 'content-type: application/json' \
      -d '{"cron":"0 2 * * *","timezone":"America/New_York","template_id":"tpl_abc123"}'

Next: give the job its secrets with [set a secret](tutorial-set-a-secret.md).
