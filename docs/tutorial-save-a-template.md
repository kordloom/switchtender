<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
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
4. Optional: check "Always confirm before launching" for a risky template, such as a production
   deploy or a destroy. Its Launch button then opens the overrides dialog for review instead of
   firing on one click. The API field is `confirm_on_launch`.
5. Save. The template now launches from its row, or from the API.

## Add a survey

A survey is a set of typed questions asked at launch, whose answers become extra vars. Add fields of
type text, integer, boolean, or choice, and mark the ones that are required. Ansible receives them as
extra vars. Bash and Python receive them as environment values. Terraform receives
them as `TF_VAR_` variables.

## Launch it

From the UI, select the template and confirm the survey if it has one. From the API:

    curl -s -X POST localhost:8080/v1/templates/{id}/launch \
      -H 'content-type: application/json' \
      -d '{"answers":{"environment":"staging"},"credential_ids":["cred_prod_key"]}'

The body carries the survey answers under `answers`, one key per field, and an optional
`credential_ids` list choosing from the template's selectable credentials. Both are optional, so an
empty body launches a template that has no required survey fields and no selectable credentials. A
chosen credential must be in the template's selectable set, and it applies on top of the credentials
the template always uses.

Next: run it on a cadence with [schedule a job](tutorial-schedule-a-job.md).
