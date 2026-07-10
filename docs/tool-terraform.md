<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Terraform runs

A Terraform run provisions infrastructure from a working directory of `.tf` files. The run's command
names that directory, relative to the project checkout.

## What runs

`terraform init` runs first; if it fails the run stops there with init's result. Then `terraform
apply -auto-approve` applies the configuration. A dry run runs `terraform plan` instead, so it
previews the change without touching infrastructure. All three run with `-input=false -no-color`, so
a run never blocks on a prompt.

## How values reach the configuration

- Extra vars, including survey answers and template vars, arrive as `TF_VAR_` environment entries, so
  a variable named `region` becomes `TF_VAR_region`. Scalars pass through as strings; lists and maps
  pass as JSON, which Terraform accepts.
- Credentials arrive in the environment, so an `env` credential of cloud keys or a `token` credential
  as `YARDMASTER_TOKEN` authenticates the provider. A command-source credential resolves fresh each
  run.
- The command directory cannot escape the project with `..`, so a run stays inside its checkout.

## Example

A directory `infra/network` holding:

    variable "region" { type = string }
    output "vpc" { value = "vpc-${var.region}" }

Launch a Terraform run with the command set to `infra/network` and a survey field `region`. A dry run
shows the plan; a real run applies it.

See also [Bash runs](tool-bash.md), [Python runs](tool-python.md), [Go runs](tool-go.md), and the
[tutorials](tutorials.md).
