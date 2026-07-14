<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Railwarden" width="140">
  </picture>
</p>

# Run a job

You launch a run and watch a live host-by-task matrix, not a text log, and the run can drive any
tool, not only Ansible.

## Launch from the UI

1. Open Runs and select Launch run.
2. Pick the tool. For Ansible, choose a playbook and an inventory. For Bash, Terraform, OpenTofu, Python, PowerShell, or Go,
   enter the script, or the working directory for Terraform.
3. Optional: turn on Dry run to preview without making changes. Ansible runs `--check`, Terraform
   runs `plan`, and Bash and Python run a syntax check.
4. Select Launch. The run detail page paints each host and task as it happens, with drill-down into
   stdout, stderr, return code, and diff.

## Split it across hosts

Set a shard count of two or more and Railwarden splits the run across your inventory, balanced by
each host's measured duration from recent runs, then rolls the shards up into one matrix. Each shard
carries a real share of the work, sized by measured cost rather than a flat count.

## Do it from the API

    curl -s -X POST localhost:8080/runs \
      -H 'content-type: application/json' \
      -d '{"tool":"bash","command":"echo hello from railwarden"}'

For an Ansible run, send `playbook` and `inventory` instead of `command`, and add `"shards": 3` to
split it. See the [HTTP API](api.md) for every field.

Next: save this launch as a preset in [save a template](tutorial-save-a-template.md).
