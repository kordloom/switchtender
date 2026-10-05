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
| `kubeconfig` | A Kubernetes kubeconfig document, written to a private file bound to `KUBECONFIG` for kubectl and helm, `K8S_AUTH_KUBECONFIG` for the kubernetes.core collection, and `KUBE_CONFIG_PATH` for Terraform's kubernetes and helm providers. Its tokens, client keys, passwords, and secret exec plugin variables are masked in run output, and its ordinary lines, such as `apiVersion: v1`, are not, so `kubectl get -o yaml` stays readable. |

## Federated kinds

Four more kinds store no secret at all. `aws_oidc`, `gcp_oidc`, `azure_oidc`, and `oidc_token` hold
only settings, and each run that carries one receives a short-lived identity token SwitchTender signs,
which the cloud verifies and exchanges for credentials that expire on their own. They need the server
to be an OpenID Connect issuer, set with `--federation-issuer`. `oidc_token`, for any other relying
party, hands the token over as a file inside the run's private directory, named in
`SWITCHTENDER_OIDC_TOKEN_FILE`, and never as a variable holding the token itself, which every process
the run starts would inherit. See
[Federated cloud credentials](federation.md) for the settings, the token's claims, and trust policy
examples for AWS, Google Cloud, and Azure.

## Sources

A credential's source decides where its value comes from at run time.

| Source | Where the value comes from |
|--------|----------------------------|
| Stored | The sealed value you pasted. This is the default. |
| Command | A command SwitchTender runs at launch, whose standard output is the secret. Works with any CLI, so Vault, AWS, GCP, or 1Password all resolve with no extra integration. |
| Vault | A Vault address, path, and field, read over Vault's HTTP API at launch. Handles KV v2 and KV v1. |
| Vault dynamic | A Vault dynamic secrets path. A fresh, short-lived credential is minted for each run and revoked when the run ends. |
| Google Secret Manager | A project, secret, and version, read at launch. On GCP it reads as the attached service account with no stored key. |
| AWS Secrets Manager | A secret id, region, and AWS credentials, read over a Signature Version 4 signed request at launch. The access key comes from the credential's config or from `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`. The instance metadata service is not read, so an EC2 instance role alone does not authenticate it. |
| AWS STS | An IAM role to assume. A fresh set of short-lived role credentials is minted for each run and injected as `AWS_*` environment variables, and they expire on their own STS lifetime. The AssumeRole call itself is signed with a base access key from the config or the AWS environment, so that key is what the install stores. |
| Azure Key Vault | A vault name and secret, read over the Key Vault REST API at launch. Authenticates with a bearer token, a service principal, or, on Azure, the attached managed identity with no stored key. |
| CyberArk Conjur | A Conjur URL, account, and variable, read over the Conjur REST API at launch. Authenticates with an access token or by exchanging an API key, so no long-lived credential is stored once a token is issued. |
| CyberArk CCP | A Central Credential Provider URL, app id, and account locator, read over the AIMWebService REST API at launch. Authenticates the application with a client certificate or a CCP allowed-machine rule, so no long-lived credential is stored. |
| 1Password | A 1Password Connect URL, token, vault, item, and field, read over the Connect REST API at launch. The vault and item may be names or ids, and the field defaults to the item's password. |

A source read over HTTP is reached through the guarded client every outbound request of the server
uses: it refuses the cloud metadata addresses and follows no redirect. With
`SWITCHTENDER_EGRESS_PROXY` set, the request leaves through that proxy after its target's address is
checked, and the ambient `HTTP_PROXY` and `HTTPS_PROXY` are never used. The token an attached managed
identity reads from its cloud's metadata service is the one request the check does not apply to,
since that fixed address is what the request is for.

A source that authenticates with an attached managed identity holds no stored key, which is the point of using one, but it does mean the host's identity is a credential reachable from anything running on that host. A containerized run with network access can read it from the cloud metadata service the same way this server does. Run with `--container-network none` where a run does not need the network, and block the link-local metadata address at the host firewall where it does. This bounds what the identity can reach as well: give it access to the secrets this install reads and nothing more.

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
looks secret, such as `ansible_password`, `ansible_become_pass`, or `api_token`, whether the
inventory is stored or named by a file path. Each known value is also redacted in the forms tools
commonly print it in: hex in either case, base64 in either alphabet whether alone or inside a longer
blob such as a basic auth header, URL escaping with upper or lowercase escapes, including the form
Ansible's `urlencode` filter writes, and JSON escaping, including the `\u` escapes Python writes for
accented letters and the escaped slashes PHP writes. A long value printed across lines is caught as
well: base64 wrapped at 76 or 64 columns, `xxd -p` hex, and an `od -An -tx1` byte dump. Inside a longer base64 blob, the few
characters at the secret's edges also carry the bytes around it and stay visible. Two things sit
outside that. A dynamic inventory source saves whatever its script emits into the inventory it
maintains, because the synced host list is the data later runs depend on, so anyone who can read
that inventory reads it as stored. And a secret under an ordinary name, say a `connection_string`
holding a password, is invisible to the name heuristic. Keep real secrets in credentials or an
external source and let inventories carry references; a credential attached to the inventory reaches
every run that targets it, sealed at rest and masked in output.

While a run executes, its working material lives in files created mode 0600 and removed when the
run ends: materialized credentials, the inventory, the extra vars file, an inline script, and, for
Ansible, the structured event sidecar the callback writes before the server masks it at read time.
The exposure window is the run and the reader is the executing user.

Everything a run stages except the event sidecar, the secrets the control node delivers to a relay
worker included, sits in a private directory for the run, mode 0700, under a root private to the
account: the systemd runtime directory when the server runs under the shipped unit, which is
memory-backed and removed by systemd when the service stops, and otherwise a private XDG runtime
directory or the temporary directory. The directory is removed as soon as the
tool exits, before the run is recorded as finished, whether it succeeded, failed, was canceled, or
timed out. A process killed mid-run removes nothing, so each run's directory carries a lock its
process holds until the run ends and the operating system releases when the process dies. Every
server and worker sweeps the directories nobody holds, at startup and about every five minutes
after, and removes one only once two sweeps five minutes apart find it unchanged, so a run another
process on the same host is still executing keeps its files. A server or worker refuses to start
with its run files on a network filesystem. [Run files](run-files.md) covers the details, the
container and Kubernetes cases, the state cloud tools keep, and swap, encryption, and backups.

## Relay workers

A relay worker started with `--server` holds no database and no encryption key, so it cannot open a
credential itself. When its pool registers a delivery key, the control node opens each claimed run's
secrets, its credentials, its inventory's credentials, its image's registry login, its custom types'
field values, its secret survey answers, a federated credential's minted token, its inventory
snapshot, and the plan file a gated apply carries out, and seals them to that key, bound to the
claim. The worker opens them in memory when the run first needs one and writes
them into the run's private directory described above, through the same code a worker with database
access uses, so every guarantee on this page holds for them too. The opened copy is dropped as soon
as it is written, the directory is removed however the run ends, a relay worker sweeps what a crash
left behind when it starts, and only ciphertext crosses the relay. The audit chain records which pool
and worker received which credential ids, never a value. A pool that registers no key receives
nothing, and a run that needs a secret fails on it with the reason. See
[Delivering secrets to relay workers](configuration.md#delivering-secrets-to-relay-workers) for
registering and rotating a key.

## Inventory snapshots and plan files

Two more things a run carries are secrets by content. The inventory a run was submitted with is
snapshotted then, and a host list can hold an `ansible_password` or a token. The plan file a gated
Terraform or OpenTofu apply carries out holds the values the configuration was planned with,
sensitive ones included. Both are sealed with the server's key while the run waits, bound to the
run's approval by a digest of the sealed form, which reveals nothing about what they hold, written
only into the run's private directory when it executes, kept out of the run log and every API
response, and wiped from the database when the run ends. The values a plan marks sensitive join the
masker before the apply prints anything. On an install with no encryption key, where inventories are
already stored in plain text, the two are stored encoded but not encrypted, and an install that
holds a key refuses to open one stored that way.

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
- Run each worker under the shipped systemd unit, or point `--runfiles-dir` at a per-worker tmpfs, so
  staged secrets live in memory and go with the process rather than accumulating.

## Encryption

Sealing needs a key. Set `SWITCHTENDER_ENCRYPTION_KEY` and a stable `SWITCHTENDER_ENCRYPTION_SALT` before
storing an externally sourced credential or inventory. The sealed value never leaves SwitchTender. The
key and salt live outside the database, in the environment of each process, so a copy of the database
alone opens no sealed value, the federation signing keys included.

Back up the key and salt the way you back up any root secret. Whoever holds them can decrypt every
stored credential, and losing them makes the sealed values unrecoverable, so every credential has to
be re-entered. Keep the salt stable for the life of an install; changing it has the same effect as
losing the key. There is no in-place key rotation yet: rotating the key means re-provisioning each
stored credential under the new key, so plan a rotation as a re-entry pass rather than a background
re-seal. For a compromised key, treat every credential it sealed as exposed, rotate those upstream
secrets at their source, and re-enter them under a fresh key and salt.

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
is treated as configuration and is not masked. When no field is marked, every value is masked. An
injector value is literal text with `{{field}}` references, so `"Bearer {{api_key}}"` becomes the
header with the key spliced in. A reference to a field the type does not declare is refused when the
type is created, not left to expand to nothing at run time. A field marked `multiline` is one whose
value spans lines, such as a certificate, and gets a text area when the credential's values are
entered.

### File injectors

Some tools read a credential from a file rather than from a variable: kubectl reads a kubeconfig, a
cloud CLI reads a config file, a TLS client reads a certificate and a key. A `file` injector renders a
file from the fields and hands its path to the tool through an env or extra var injector, the same
way AWX does:

    POST /v1/credential-types
    {
      "name": "Kubeconfig",
      "fields": [
        {"name": "kubeconfig", "label": "Kubeconfig", "secret": true, "multiline": true}
      ],
      "file": {"template": "{{ kubeconfig }}"},
      "env":  {"KUBECONFIG": "{{ tower.filename }}"}
    }

A type writes one file under `template`, or several under `template.<name>`, never both:

    "file": {
      "template.cert": "{{ cert }}",
      "template.key":  "{{ key }}"
    },
    "env": {
      "TLS_CERT_FILE": "{{ tower.filename.cert }}",
      "TLS_KEY_FILE":  "{{ tower.filename.key }}"
    }

`{{ tower.filename }}` names the single file's path and `{{ tower.filename.<name> }}` one of
several. That is the form AWX uses, so a type copied from AWX needs no edits. SwitchTender also
reads `awx.filename` as another name for the same path. AWX is not known to accept that spelling, so
use `tower.filename` in a type that has to load in AWX as well. A file template references fields
only, may span lines, and a field only a file references may hold line breaks. A field an env or
extra var injector references may not, since a line break there would become a second variable.
Every file a type defined here writes has to be referenced, as described under
[A file nothing references](#a-file-nothing-references).

At run time each file is written mode 0600 into the run's private directory, mode 0700, described
under [What masking does not cover](#what-masking-does-not-cover), and removed with it when the run
ends. A containerized run has each file mounted at the same path, so the path a variable carries
resolves inside the container too. The rendered contents never reach the API, the run's record, the
audit chain, a receipt, a dossier, or an export. Only the file holds them. A secret field written to
a file is masked like any other: every line of its value is redacted if a tool prints the file. For a
kubeconfig, the built-in `kubeconfig` kind masks only the secret values inside the document, which
keeps ordinary Kubernetes output readable.

The template language is plain substitution, nothing more. AWX's injectors are Jinja, and an AWX
type that uses a filter or a condition is refused on import with the reason rather than carried
across as text a run would receive.

Create a credential of that type:

    POST /v1/credentials
    {
      "name": "prod-datadog",
      "type_id": "ctype_...",
      "fields": {"host": "api.datadoghq.com", "api_key": "the-secret-key"}
    }

The field values are sealed together as one encrypted object, the same as any other secret. At run
time the type's injectors write its files and add the environment variables, and any extra vars go
through a private file so they never reach the process argument list. A field the type does not
declare is refused.

Set new values on an existing typed credential with its fields, which replace every stored value at
once:

    PUT /v1/credentials/{id}
    {"name": "prod-datadog", "fields": {"host": "api.datadoghq.com", "api_key": "the-new-key"}}

Leave `fields` out to rename it and keep its values. A single `secret` or `settings` is refused on a
typed credential, so the update cannot reinterpret the sealed field object as a raw value. This is
also how a credential imported from AWX with a custom type gets its values, since an export never
carries them.

A `kind` sent together with that kind's `secret` moves the credential off its custom type onto the
built-in kind, in one request, keeping its id, name, and everything that references it:

    PUT /v1/credentials/{id}
    {"name": "prod-kube", "kind": "kubeconfig", "secret": "apiVersion: v1\nkind: Config\n..."}

The field values are replaced whole by the new secret, never read as one. A `kind` without a
`secret`, or `fields` beside a `kind`, is refused with the reason. A federated kind stores no
secret, so a credential does not move to one in place: create a new credential for it. In the
interface, edit the credential, choose the built-in kind, and paste its secret.

### A file nothing references

A file a type writes lands at a new path in the run's private directory every run, so the only way a
tool finds it is an env or extra var injector that hands it the path, with `{{ tower.filename }}` or
`{{ tower.filename.<name> }}`. A file nothing references is still written, and the tool runs as if
the credential were missing. What usually follows is an authentication failure that never mentions
the file.

A type defined in SwitchTender is refused for one, when it is created and when it is edited. The
refusal names each such file and the reference that fixes it. This type writes the token to a file
and hands the tool only the endpoint:

    POST /v1/credential-types
    {
      "name": "Service Token File",
      "fields": [
        {"name": "endpoint", "label": "Endpoint"},
        {"name": "token",    "label": "Token", "secret": true}
      ],
      "file": {"template": "endpoint={{ endpoint }}\ntoken={{ token }}"},
      "env":  {"SERVICE_ENDPOINT": "{{ endpoint }}"}
    }

The answer is a 400 with this error:

    invalid credential type: file template is written but no env or extra-var injector references
    its path, so the tool would never be told where to find it. Hand the path over with
    {{ tower.filename }} in an env or extra-var injector, for example
    "env": {"CREDENTIAL_FILE": "{{ tower.filename }}"}, or delete the file injector

The fix is one more injector, under whatever name the tool reads the path from:

    POST /v1/credential-types
    {
      "name": "Service Token File",
      "fields": [
        {"name": "endpoint", "label": "Endpoint"},
        {"name": "token",    "label": "Token", "secret": true}
      ],
      "file": {"template": "endpoint={{ endpoint }}\ntoken={{ token }}"},
      "env":  {
        "SERVICE_ENDPOINT":   "{{ endpoint }}",
        "SERVICE_TOKEN_FILE": "{{ tower.filename }}"
      }
    }

A type imported from AWX may keep such a file, so a migration does not stop over a type that was in
use before the move. The file is written for each run, and it is called out in three places:

- The import report lists it under the items worth reviewing, naming the type and the file, in the
  preview, in the Migrate page, and in what `switchtender import awx` prints.
- When the import is applied through the API or the Migrate page, the server log has a warning with
  the type in `credential_type` and `credential_type_id` and the file in `file`.
- Every run that uses a credential of the type carries a warning, shown on the run's page, returned
  in its `warning` field, and printed in its dossier. It names the credential, the type, and the
  file, and the server log has the same warning with `run_id`, `credential_id`, `credential_type`,
  `credential_type_id`, `type_origin`, and `file` fields.

None of them ever carries what the file holds. To fix an imported type, open Credentials, find it
under Credential types, and add the reference, or send the corrected type with
`PUT /v1/credential-types/{id}`. Once every file is referenced, the run warning stops. An edit keeps
the files the import brought that nothing references, so a rename or a new field does not have to
fix everything at once, but an edit made here cannot add another one. A type carries its origin as
`"origin": "awx"` in the API, set by the import and never by a request.

### Kubeconfig types from AWX

An AWX custom type can hold a kubeconfig: it writes one secret field to a file and points
`KUBECONFIG`, `K8S_AUTH_KUBECONFIG`, or `KUBE_CONFIG_PATH` at it:

    {
      "name": "Kubeconfig",
      "fields": [
        {"name": "kube_config", "label": "Kubeconfig", "secret": true, "multiline": true}
      ],
      "file": {"template": "{{ kube_config }}"},
      "env":  {"K8S_AUTH_KUBECONFIG": "{{ tower.filename }}"}
    }

A type like that imports as it is, and its credentials mask the whole field. Every line of the
document is redacted wherever a tool prints it, ordinary lines such as `apiVersion: v1` and
`kind: Config` included. That is the safe side to err on, and it also blanks those lines out of
every `kubectl get -o yaml` a run prints. The built-in `kubeconfig` kind writes the same file and
points all three variables at it, and it masks only the secrets inside the document: tokens,
passwords, client keys, and secret exec plugin variables.

Nothing is converted on import, because the switch changes what a run's output shows. The import
report names each type in this shape, and gives each credential of it the one request that switches
it:

    PUT /v1/credentials/{id}
    {"name": "prod-kube", "kind": "kubeconfig", "secret": "<the kubeconfig document>"}

That is the request that enters the credential's value in the first place, with the document as the
kubeconfig kind's secret instead of a `fields` object, so switching costs nothing extra. The report
names a type only when the switch loses nothing a run receives. A type that also sets an extra var,
sets another variable, or writes more into the file than the one field keeps its line-by-line
masking and gets no switch, because moving it would change what its runs see.
