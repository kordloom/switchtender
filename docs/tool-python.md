<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Python runs

A Python run executes a script with `python3`. The script is the run's command. It suits a task that
is more comfortable in Python than a shell: calling an API, reconciling state, or shaping data.

## What runs

The script is written to a temporary file and run with `python3`, so multi-line scripts work
unchanged. The working directory is the project checkout when the run sources a project. A dry run
runs `python3 -m py_compile`, which checks syntax without executing the script.

## How values reach the script

- Extra vars, including survey answers and template vars, arrive as `YARDMASTER_VARS`, a JSON object:

        import json, os
        vars = json.loads(os.environ.get("YARDMASTER_VARS", "{}"))
        region = vars.get("region", "us-east-1")

- An `env` credential's `KEY=VALUE` lines are set in the environment, read with `os.environ`.
- A `token` credential is set as `YARDMASTER_TOKEN`, ready to send as a bearer token.
- Credentials attached to the run's inventory arrive the same way.

## Example

    import json, os, urllib.request
    vars = json.loads(os.environ.get("YARDMASTER_VARS", "{}"))
    token = os.environ["YARDMASTER_TOKEN"]
    req = urllib.request.Request(
        f"https://api.example.com/deploy/{vars['service']}",
        headers={"Authorization": f"Bearer {token}"}, method="POST")
    print(urllib.request.urlopen(req).status)

See also [Bash runs](tool-bash.md), [Terraform runs](tool-terraform.md),
[Go runs](tool-go.md), and the [tutorials](tutorials.md).
