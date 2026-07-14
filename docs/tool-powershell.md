<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Railwarden" width="140">
  </picture>
</p>

# PowerShell runs

A PowerShell run executes a script with `pwsh`, PowerShell 7. The script is the run's command. It
suits Windows-leaning fleets and any task already written in PowerShell: service management, cloud
module calls, or reporting.

## What runs

The script is written to a temporary file and run with `pwsh -NoProfile -NonInteractive -File`, so
multi-line scripts work unchanged and nothing prompts. The working directory is the project checkout
when the run sources a project. A dry run parses the script into a script block without invoking it,
so syntax is checked and nothing executes.

## How values reach the script

- Extra vars, including survey answers and template vars, arrive as `RAILWARDEN_VARS`, a JSON
  object:

        $vars = $env:RAILWARDEN_VARS | ConvertFrom-Json
        $region = if ($vars.region) { $vars.region } else { "us-east-1" }

- Credentials arrive in the environment. An `env` credential lands its `KEY=VALUE` lines directly,
  and a `token` credential arrives as `RAILWARDEN_TOKEN`.
- Anything the script prints that matches a materialized secret is masked to `***` in the stored and
  streamed log, the same masking every tool gets.

## Requirements

The `pwsh` binary must be on the PATH of the process executing runs, the server or a worker. On
Linux and macOS that is [PowerShell 7](https://github.com/PowerShell/PowerShell); on Windows it
ships with PowerShell 7 installations. Nothing else is configured.

## Example

    $vars = $env:RAILWARDEN_VARS | ConvertFrom-Json
    Write-Output "deploying to $($vars.region)"
    exit 0

Save it as a template with the PowerShell tool and a survey field `region`, and every launch prompts
for the region and runs the script with it.

See also [Bash runs](tool-bash.md), [Python runs](tool-python.md), and the
[features overview](features.md).
