<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Railwarden" width="140">
  </picture>
</p>

# Save a template

A template is a saved preset that launches a run in one action instead of a hand-built request. It
bundles the tool, the project and playbook or command, an inventory, a shard count, credentials, and
extra vars.

## Create one

1. Open Templates and add a template.
2. Give it a name, then set the same fields you would fill in at launch: the tool, the playbook or
   command, an inventory, and any credentials.
3. Optional: set a shard count to split every launch, or a queue to pin launches to specific workers.
4. Save. The template now launches from its row, or from the API.

## Add a survey

A survey is a set of typed questions asked at launch, whose answers become extra vars. Add fields of
type text, integer, boolean, or choice, and mark the ones that are required. Ansible receives them as
extra vars. Bash and Python receive them as environment values. Terraform receives
them as `TF_VAR_` variables.

## Launch it

From the UI, select the template and confirm the survey if it has one. From the API:

    curl -s -X POST localhost:8080/templates/{id}/launch \
      -H 'content-type: application/json' \
      -d '{"environment":"staging"}'

The body is the survey answers themselves, one key per field. Send an empty body when the template
has no required survey fields.

Next: run it on a cadence with [schedule a job](tutorial-schedule-a-job.md).
