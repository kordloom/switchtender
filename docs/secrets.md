<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Secrets

Secrets live in credentials. Each is sealed at rest with AES-256-GCM, is never returned by the API,
and reaches a run only while it executes, in the environment or a temporary file created mode 0600
and deleted when the run ends.
If a tool prints a secret, SwitchTender redacts it from the run's log, live stream, and events, so the
output shows `***` instead of the value.

## Kinds

A credential's kind decides how its value reaches a run.

| Kind | What it is |
|------|------------|
| `ssh_key` | An SSH private key, to reach hosts and clone private git projects. A passphrase protected key is unlocked in memory at run time from a passphrase sealed alongside it, so no prompt blocks the run. |
| `ssh_password` | A machine login, injected as `ansible_user` and `ansible_password` through a file, so the password stays off the command line. |
| `vault_password` | An Ansible Vault password. An optional vault ID label passes it as `--vault-id label@file`, so several vault credentials on one run each unlock the secrets encrypted for their label; without a label it is the classic `--vault-password-file`. |
| `become_password` | A privilege escalation password, kept off the command line. |
| `become` | Privilege escalation with an optional method and user, injected as the `ansible_become_*` variables through a file. |
| `network` | A network device login, injected as the `ansible_user`, `ansible_password`, `ansible_network_os`, and `ansible_connection` variables. |
| `env` | `KEY=VALUE` lines injected into the environment, how cloud SDK tokens reach a tool. |
| `token` | A single API token or JWT, exposed to the run as `SWITCHTENDER_TOKEN`. |
| `registry` | A container registry login, to pull a pinned execution image. |
| `aws` | An AWS access key, injected as the standard `AWS_*` environment variables. |
| `azure` | An Azure service principal, injected as the `ARM_*` variables Terraform reads and the `AZURE_*` variables the Ansible azure collection reads. |
| `gcp` | A Google Cloud service account JSON, written to a private file bound to `GOOGLE_APPLICATION_CREDENTIALS`. |
| `vmware` | A vCenter login, injected as the `VMWARE_*` environment variables the community.vmware modules read. |
| `openstack` | An OpenStack login, injected as the `OS_*` environment variables openstacksdk and the openstack.cloud collection read. Fields: `auth_url`, `username`, `password`, `project_name` required; `user_domain_name` and `project_domain_name` default to `Default`; `region_name` optional. |

## Sources

A credential's source decides where its value comes from at run time.

| Source | Where the value comes from |
|--------|----------------------------|
| Stored | The sealed value you pasted. This is the default. |
| Command | A command SwitchTender runs at launch, whose standard output is the secret. Works with any CLI, so Vault, AWS, GCP, or 1Password all resolve with no extra integration. |
| Vault | A Vault address, path, and field, read over Vault's HTTP API at launch. Handles KV v2 and KV v1. |
| Vault dynamic | A Vault dynamic secrets path. A fresh, short-lived credential is minted for each run and revoked when the run ends. |
| Google Secret Manager | A project, secret, and version, read at launch. On GCP it reads as the attached service account with no stored key. |
| AWS Secrets Manager | A secret id, region, and AWS credentials, read over a Signature Version 4 signed request at launch. Credentials fall back to the standard AWS environment, so an instance role needs no stored key. |
| AWS STS | An IAM role to assume. A fresh set of short-lived role credentials is minted for each run and injected as `AWS_*` environment variables. The credentials expire on their own STS lifetime, so nothing long-lived is stored. |
| Azure Key Vault | A vault name and secret, read over the Key Vault REST API at launch. Authenticates with a bearer token, a service principal, or, on Azure, the attached managed identity with no stored key. |
| CyberArk Conjur | A Conjur URL, account, and variable, read over the Conjur REST API at launch. Authenticates with an access token or by exchanging an API key, so no long-lived credential is stored once a token is issued. |
| CyberArk CCP | A Central Credential Provider URL, app id, and account locator, read over the AIMWebService REST API at launch. Authenticates the application with a client certificate or a CCP allowed-machine rule, so no long-lived credential is stored. |
| 1Password | A 1Password Connect URL, token, vault, item, and field, read over the Connect REST API at launch. The vault and item may be names or ids, and the field defaults to the item's password. |

## Ephemeral secrets

A Vault dynamic source mints a new credential for each run and revokes it the moment the run ends, so
a leaked value is useless minutes later. If the process dies before it can revoke, the credential
still expires on the lease's own TTL. This is a control-plane capability neither incumbent offers.

## Scope

Attach a credential to a run, a template, a project, or a stored inventory. A credential attached to an
inventory reaches every run that targets it, so a fleet carries its own secret variables in one place.

## Settings

A credential can carry non-secret fields beside its sealed secret: the user to connect as, a become
method, a region. Unlike the secret, settings return from the API and show in the interface, so an
operator can see and edit them and an AWX import lands them automatically. On the connection kinds,
`ssh_key`, `ssh_password`, `become`, and `network`, settings inject as the matching Ansible
variables, merged beneath the sealed fields so a sealed value wins on a shared name; a machine
credential whose settings carry `become_method` or `become_user` injects the matching
`ansible_become_*` variables. On every other kind, including `env`, settings are reference metadata
only and are not injected, so a non-secret pair can never shadow another credential's sealed value.
Settings values are never added to the run's mask list, which is the point: masking a username like
`deploy` would black out ordinary output everywhere it appears.

## What masking does not cover

Masking redacts known values: everything a credential injects, and any inventory variable whose name
looks secret, such as `ansible_password`, `ansible_become_pass`, or `api_token`. Two things sit
outside that. A dynamic inventory source saves whatever its script emits into the inventory it
maintains, because the synced host list is the data later runs depend on, so anyone who can read
that inventory reads it as stored. And a secret under an ordinary name, say a `connection_string`
holding a password, is invisible to the name heuristic. Keep real secrets in credentials or an
external source and let inventories carry references; a credential attached to the inventory reaches
every run that targets it, sealed at rest and masked in output.

While a run executes, its working material lives in files created mode 0600 and removed when the
run ends: materialized credentials, the inventory, and, for Ansible, the structured event sidecar
the callback writes before the server masks it at read time. The exposure window is the run and the
reader is the executing user, but a hard crash can leave those files until the temp directory
clears. On a shared or long-lived executor, point `TMPDIR` at a tmpfs so run scratch lives in
memory and dies with the machine instead of landing on disk.

## What a run can reach on the host

A run without an execution image runs as the SwitchTender server's own user. That user owns the
scratch files of every other run on that host, so the 0600 mode above stops other accounts, not other
runs: while two runs overlap, either one's code can read the other's materialized credentials, and a
run can write to the directory the Ansible callback plugin lives in. SwitchTender checks that plugin
against its embedded copy before every Ansible run and restores it when it differs, so nothing a run
leaves behind survives to be imported by later runs, but that is repair, not isolation.

Treat a run's content as trusted code, at the level of the host it runs on. Where that is not true,
separate them:

- Pin an execution image on the template. A containerized run gets its own filesystem, the plugin
  directory is mounted read-only, and the run's environment is sourced from a file the container
  cannot reach outside its own mount.
- Give each trust domain its own worker. Workers are separate processes with their own queue, so
  production and a team's ad-hoc work need not share a user or a temp directory.
- Point `TMPDIR` at a per-worker tmpfs, so scratch dies with the process rather than accumulating.

## Encryption

Sealing needs a key. Set `SWITCHTENDER_ENCRYPTION_KEY` and a stable `SWITCHTENDER_ENCRYPTION_SALT` before
storing an externally sourced credential or inventory. The sealed value never leaves SwitchTender.

See [Set a secret](tutorial-set-a-secret.md) for the step-by-step, including the API calls.

## Custom credential types

The built-in kinds cover the common providers. For one they do not, define a custom type: the fields
it collects and how those fields are injected into a run. This is what AWX calls a custom credential
type, and it needs no code change.

A type is data, not code. An injector splices a field's value into a string literally, and nothing
in it is executed, so a type cannot become a way to run something on the executor.

Define a type, admin only:

    POST /v1/credential-types
    {
      "name": "Datadog API",
      "fields": [
        {"name": "host",    "label": "API host"},
        {"name": "api_key", "label": "API key", "secret": true}
      ],
      "env": {
        "DD_HOST":    "{{host}}",
        "DD_API_KEY": "{{api_key}}"
      },
      "extra_vars": {
        "datadog_host": "{{host}}"
      }
    }

A field marked `secret` is masked out of run output; a field left plain, such as a host or a region,
is treated as configuration and is not masked. An injector value is literal text with `{{field}}`
references, so `"Bearer {{api_key}}"` becomes the header with the key spliced in. A reference to a
field the type does not declare is refused when the type is created, not left to expand to nothing at
run time.

Create a credential of that type:

    POST /v1/credentials
    {
      "name": "prod-datadog",
      "type_id": "ctype_...",
      "fields": {"host": "api.datadoghq.com", "api_key": "the-secret-key"}
    }

The field values are sealed together as one encrypted object, the same as any other secret. At run
time the type's injectors add the environment variables, and any extra vars go through a private file
so they never reach the process argument list. A field the type does not declare is refused.

A typed credential is recreated rather than updated, so its fields are changed by deleting it and
creating a new one. This keeps the update path, which speaks a single secret, from reinterpreting the
sealed field object.
