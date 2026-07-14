<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Railwarden" width="140">
  </picture>
</p>

# Secrets

Secrets live in credentials. Each is sealed at rest with AES-256-GCM, is never returned by the API,
and reaches a run only while it executes, in a temporary file or environment that is wiped afterward.
If a tool prints a secret, Railwarden redacts it from the run's log, live stream, and events, so the
output shows `***` instead of the value.

## Kinds

A credential's kind decides how its value reaches a run.

| Kind | What it is |
|------|------------|
| `ssh_key` | An SSH private key, to reach hosts and clone private git projects. |
| `vault_password` | An Ansible Vault password. |
| `env` | `KEY=VALUE` lines injected into the environment, how cloud SDK tokens reach a tool. |
| `token` | A single API token or JWT, exposed to the run as `RAILWARDEN_TOKEN`. |
| `become_password` | A privilege escalation password, kept off the command line. |
| `registry` | A container registry login, to pull a pinned execution image. |

## Sources

A credential's source decides where its value comes from at run time.

| Source | Where the value comes from |
|--------|----------------------------|
| Stored | The sealed value you pasted. This is the default. |
| Command | A command Railwarden runs at launch, whose standard output is the secret. Works with any CLI, so Vault, AWS, GCP, or 1Password all resolve with no extra integration. |
| Vault | A Vault address, path, and field, read over Vault's HTTP API at launch. Handles KV v2 and KV v1. |
| Vault dynamic | A Vault dynamic secrets path. A fresh, short-lived credential is minted for each run and revoked when the run ends. |
| Google Secret Manager | A project, secret, and version, read at launch. On GCP it reads as the attached service account with no stored key. |

## Ephemeral secrets

A Vault dynamic source mints a new credential for each run and revokes it the moment the run ends, so
a leaked value is useless minutes later. If the process dies before it can revoke, the credential
still expires on the lease's own TTL. This is a control-plane capability neither incumbent offers.

## Scope

Attach a credential to a run, a template, a project, or a stored inventory. A credential attached to an
inventory reaches every run that targets it, so a fleet carries its own secret variables in one place.

## Encryption

Sealing needs a key. Set `RAILWARDEN_ENCRYPTION_KEY` and a stable `RAILWARDEN_ENCRYPTION_SALT` before
storing an externally sourced credential or inventory. The sealed value never leaves Railwarden.

See [Set a secret](tutorial-set-a-secret.md) for the step-by-step, including the API calls.
