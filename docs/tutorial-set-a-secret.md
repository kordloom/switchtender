<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Set a secret

Secrets live in credentials, the same as AWX. Each is sealed at rest and never returned by the API.
A run gets a credential's value only while it executes, in a temporary file or environment that is
wiped afterward.

## Store a credential

1. Open Credentials and add one.
2. Pick the kind:
   - `ssh_key`: an SSH private key, to reach hosts and clone private git projects.
   - `vault_password`: an Ansible Vault password.
   - `env`: `KEY=VALUE` lines injected into the run, how cloud SDK tokens reach a tool.
   - `token`: a single API token or JWT, exposed to the run as the `YARDMASTER_TOKEN` environment variable.
   - `become_password`: a privilege escalation password, kept off the command line.
   - `registry`: a container registry login, to pull a pinned execution image.
3. Paste the secret and save. Attach it to a project, a template, or a run.

## Resolve it from an external store

You do not have to paste the secret at all. Set the credential's source to a command, and Yardmaster
runs that command at launch and uses its standard output as the secret. That keeps the value in your
existing store, fetched fresh each run:

    vault kv get -field=token secret/ci

Any CLI works the same way, so a credential can pull from Vault, AWS Secrets Manager, GCP Secret
Manager, or 1Password with no extra integration.

## From the API

    curl -s -X POST localhost:8080/credentials \
      -H 'content-type: application/json' \
      -d '{"name":"ci-token","kind":"env","source":"command","secret":"vault kv get -field=token secret/ci"}'

For a pasted value, omit `source` and put the value in `secret`. Imported credentials arrive without
secrets, since exports never contain them; this is the one-time step to fill them in.

## Scope a secret to an inventory

Attach a credential to a stored inventory and every run that targets that inventory receives it, so
a fleet can carry its own secret variables in one place. Open Inventories, edit the inventory, and
pick the credentials under Credentials. An `env` credential becomes that inventory's secret
variables; a `token` credential its bearer token. You need use access on a credential to attach it.
