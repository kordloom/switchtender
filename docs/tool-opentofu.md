<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# OpenTofu runs

An OpenTofu run provisions infrastructure from a working directory of `.tf` files with the `tofu`
binary. It behaves exactly like a [Terraform run](tool-terraform.md): the run's command names the
directory, relative to the project checkout. Pick the tool that matches the binary your
configurations are written for; the two are configuration-compatible for most modules.

## What runs

`tofu init` runs first. If it fails the run stops there with init's result. Then `tofu apply
-auto-approve` applies the configuration. A dry run runs `tofu plan` instead, so it previews the
change without touching infrastructure. All three run with `-input=false -no-color`, so a run never
blocks on a prompt.

## How values reach the configuration

- Extra vars, including survey answers and template vars, arrive as `TF_VAR_` environment entries,
  which OpenTofu reads the same way Terraform does. Scalars pass through as strings, lists and maps
  pass as JSON.
- Credentials arrive in the environment, so an `env` credential of cloud keys or a `token`
  credential as `YARDMASTER_TOKEN` authenticates the provider. A command-source credential resolves
  fresh each run.
- The command directory cannot escape the project with `..`, so a run stays inside its checkout.

## Requirements

The `tofu` binary must be on the PATH of the process executing runs, the server or a worker. Nothing
else is configured; selecting the OpenTofu tool routes the run to it.

## Example

A directory `infra/network` holding:

    variable "region" { type = string }
    output "vpc" { value = "vpc-${var.region}" }

Launch an OpenTofu run with the command set to `infra/network` and a survey field `region`. A dry
run shows the plan. A real run applies it.

See also [Terraform runs](tool-terraform.md), [Bash runs](tool-bash.md), and the
[features overview](features.md).
