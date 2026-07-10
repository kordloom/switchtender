<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Bash runs

A Bash run executes a shell script with `bash -c`. The script is the run's command, so it can drive
any tool on the host: `kubectl`, `aws`, `terraform`, `make`, and so on. It is the general-purpose
escape hatch when a task is not an Ansible play.

## What runs

The command is passed to `bash -c`. The working directory is the project checkout when the run
sources a project, so the script reaches the repository's files. A dry run passes `bash -n`, which
parses the script and reports syntax errors without executing it.

## How values reach the script

- Extra vars, including survey answers and template vars, arrive as `YARDMASTER_VARS`, a JSON object.
  Read a value with `jq`:

        region=$(printf '%s' "$YARDMASTER_VARS" | jq -r .region)

- An `env` credential's `KEY=VALUE` lines are set in the environment directly.
- A `token` credential is set as `YARDMASTER_TOKEN`.
- Credentials attached to the run's [inventory](tutorial-set-a-secret.md) arrive the same way, so a
  fleet's secret variables reach the script without naming them on the run.

## Example

    set -euo pipefail
    region=$(printf '%s' "$YARDMASTER_VARS" | jq -r '.region // "us-east-1"')
    echo "Draining $region"
    kubectl --context "$region" drain node-1 --ignore-daemonsets

See also [Terraform runs](tool-terraform.md), [Python runs](tool-python.md), and the
[tutorials](tutorials.md).
