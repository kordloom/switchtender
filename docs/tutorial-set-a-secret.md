<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Set a secret

Secrets live in credentials. Each is sealed at rest and never returned by the API.
A run gets a credential's value only while it executes, in a temporary file or environment that is
wiped afterward. If a tool prints a credential's value, Yardmaster redacts it from the run's log,
live stream, and events, so the output shows `***` instead of the secret.

## Store a credential

1. Open Credentials and add one.
2. Pick the kind:

   | Kind | What it is |
   |------|------------|
   | `ssh_key` | an SSH private key, to reach hosts and clone private git projects. |
   | `vault_password` | an Ansible Vault password. |
   | `env` | `KEY=VALUE` lines injected into the run, how cloud SDK tokens reach a tool. |
   | `token` | a single API token or JWT, exposed to the run as the `YARDMASTER_TOKEN` environment variable. |
   | `become_password` | a privilege escalation password, kept off the command line. |
   | `registry` | a container registry login, to pull a pinned execution image. |

3. Paste the secret and save. Attach it to a project, a template, or a run.

## Resolve it from an external store

You do not have to paste the secret at all. Set the credential's source to a command, and Yardmaster
runs that command at launch and uses its standard output as the secret. That keeps the value in your
existing store, fetched fresh each run:

    vault kv get -field=token secret/ci

Any CLI works the same way, so a credential can pull from Vault, AWS Secrets Manager, GCP Secret
Manager, or 1Password with no extra integration.

For Vault there is also a native source that needs no `vault` CLI on the runner: set the source to
Vault and give it the address, path, and field as JSON. Yardmaster reads the secret over Vault's HTTP
API at launch, handling both KV v2 and KV v1. The token comes from the config or the `VAULT_TOKEN`
environment:

    {"addr":"https://vault:8200","path":"secret/data/ci","field":"token"}

Google Secret Manager has a native source too: set the source to Google Secret Manager and give it
the project, secret, and version as JSON. When Yardmaster runs on GCP it reads as its attached service
account through the metadata server, so no key is stored. Off GCP, put an access token in the config.

    {"project":"my-project","secret":"ci-token","version":"latest"}

## Mint a short-lived secret per run

For the strongest secret hygiene, Yardmaster can mint a fresh credential for each run and revoke it
when the run ends, so a leaked value is useless minutes later. Set the source to Vault dynamic and
give it a dynamic secrets path, such as a database or cloud role, as JSON:

    {"addr":"https://vault:8200","path":"database/creds/app","field":"password"}

At launch Yardmaster reads that path, which mints a new credential, injects the chosen field into the
run, and records the Vault lease. When the run reaches a terminal state the lease is revoked, so the
credential lives only as long as the run. If the process dies before it can revoke, the credential
still expires on the lease's own TTL, so nothing is left behind for long. The minted value is masked
in the run's output like any other secret.

## From the API

    curl -s -X POST localhost:8080/credentials \
      -H 'content-type: application/json' \
      -d '{"name":"ci-token","kind":"env","source":"command","secret":"vault kv get -field=token secret/ci"}'

For a pasted value, omit `source` and put the value in `secret`. Imported credentials arrive without
secrets, since exports never contain them. This is the one-time step to fill them in.

## Scope a secret to an inventory

Attach a credential to a stored inventory and every run that targets that inventory receives it, so
a fleet can carry its own secret variables in one place. Open Inventories, edit the inventory, and
pick the credentials under Credentials. An `env` credential becomes that inventory's secret
variables. A `token` credential its bearer token. You need use access on a credential to attach it.

## Source an inventory's hosts

The host list itself can live in an external store, the same way a secret does. When you add or edit
an inventory, pick a content source:

- `Stored`: paste the inventory text. This is the default.
- `Command`: a command Yardmaster runs at launch, whose standard output is the inventory.
- `Vault`: a Vault address, path, and field, read over HTTP at launch.
- `Google Secret Manager`: a project, secret, and version, read at launch.

The form takes the address, path, and field for Vault, or the project and secret for Google Secret
Manager, and assembles and seals the source configuration for you, so the host list stays in your
existing store and is fetched fresh for every run. A command source keeps a generated inventory
current without a dedicated dynamic source plugin:

    ./bin/hosts --env prod

Content sources need encryption, so set `YARDMASTER_ENCRYPTION_KEY` and a stable
`YARDMASTER_ENCRYPTION_SALT` first. The sealed configuration is never returned by the API. When you
edit an inventory, leave the source fields blank to keep the stored one.

### From the API

    curl -s -X POST localhost:8080/inventories \
      -H 'content-type: application/json' \
      -d '{"name":"prod-fleet","content_source":"command","content_config":"./bin/hosts --env prod"}'

For a Vault or Google Secret Manager source, send the same JSON configuration the matching credential
source uses as `content_config`.
