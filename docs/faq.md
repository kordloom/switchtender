# FAQ

Short answers to the questions teams ask when evaluating or adopting SwitchTender. New here? Start with
the [quickstart](quickstart.md), or [switching from AWX](switching-from-awx.md) if you are migrating.

## What can SwitchTender run?

Ansible playbooks, Bash scripts, Terraform, OpenTofu, Python, PowerShell, and Go, plus any tool
you add through the [Go SDK](sdk.md). A run, a saved template, or a single step of a pipeline
picks its tool. A pipeline can mix them: Terraform to build infrastructure, Ansible to configure
it, Bash to smoke-test it, as one dependency graph. Bash shells out to anything on the host, so
kubectl, the cloud CLIs, and your own scripts all work.

## Do I need Kubernetes?

No. SwitchTender is one binary that is the API, the executor, the scheduler, and the UI. State is one
database: a SQLite file to start, or PostgreSQL when you want more than one instance. No Redis, no
operator, no separate task engine.

## What is a dry run?

A run that makes no changes. Ansible runs in check mode, Terraform runs plan instead of apply, and
Bash and Python are syntax checked without executing. A dry run set on a sharded run is carried to
every shard, so a preview is always a preview.

## How do I migrate from AWX?

From the command line:

    switchtender import awx export.json           # preview the plan, changes nothing
    switchtender import awx export.json --apply    # import

It maps projects, inventories, job templates with their surveys and schedules and job slicing, and
credentials. See [switching from AWX](switching-from-awx.md) for the full mapping.

The preview prints the inventory content it would write, not just the names, because an export is a
document somebody else produced and the inventory decides which machines a play reaches. Read it
before you pass `--apply`. Anything the import refuses is listed as a warning with the reason.

Two things to know. These exports omit secret values for security, so credentials import without
their secrets and you set those once after importing. Dynamic inventory sources import too, as
sources that run their plugin and refresh the hosts into a stored inventory on a schedule or before a
run; static inventories import fully. A Semaphore importer exists too, with
`switchtender import semaphore export.json`.

## How do I rerun the same job on a set of hosts without re-entering everything?

Save it as a template. A template bundles the tool, the command or playbook, the inventory, the
credentials, and the variables, so a rerun is one click or one API call. Stored inventories let you
name a host set once and reuse it across templates.

## How are secrets stored?

Sealed with AES-256-GCM, the key derived from an operator passphrase through argon2id. Secrets
decrypt only at execution, into the run's environment or a temporary file created mode 0600 and
deleted when the run finishes, and never appear in API responses. Fourteen kinds cover SSH keys and SSH passwords, vault passwords, become passwords and
full become settings, network device logins, environment bundles, API tokens, container registry
logins, and typed AWS, Azure, GCP, VMware, and OpenStack credentials. Set
`SWITCHTENDER_ENCRYPTION_KEY` and `SWITCHTENDER_ENCRYPTION_SALT` to enable them.

A credential can also be a command source, so the value lives in an external store instead of in
SwitchTender. SwitchTender seals a command, for example `vault kv get -field=password secret/prod` or an
`aws secretsmanager get-secret-value` call, and runs it at execution time. The command's output is
the secret and is never stored. This works with any secret manager reachable from a command: Vault,
AWS, GCP, 1Password, or your own script.

## Are webhook triggers safe to expose?

Yes. A trigger can require a verified HMAC signature, keyed by a per-trigger secret sealed at rest and
separate from the URL, so a leaked webhook URL cannot forge a push. Set the same secret on the git
host, turn enforcement on, and an unsigned or wrong request is rejected before anything runs. Secrets
rotate at any time.

## Does it support single sign-on?

Yes: OpenID Connect, SAML, and LDAP directory sign-in, with just-in-time account provisioning
and a configurable default role.

## Does SwitchTender send my data to an AI model?

Only if you turn AI on, and only to the provider you chose. With local Ollama nothing leaves
your machines. With a cloud provider, prompts carry automation content such as commands,
playbook names, masked failed-run logs, and drift summaries. Credential values are masked before
any prompt is built, and fleet questions send metadata only. AI is off by default. See the
[AI guide](ai.md) for exactly what each feature sends.

## Can the built-in advisory AI change my infrastructure?

No. The [advisory AI](ai.md) only produces text a human reads or a proposal a human releases.
Anything it drafts that could become a run is born held at the same approval gate an operator
faces, an admin reviews the generated command before it moves, and the request and decision both
land in the audit trail.

## Can an AI agent operate SwitchTender?

Yes, through the API, holding one credential: a token bound to an operator account. The agent
submits and manages runs like any operator, every mutation it makes is chained before it executes,
and a run held for approval waits for a human admin, since an operator token cannot approve.
[Run an AI agent through the gate](agents.md) covers the setup.

## Can I extend SwitchTender?

Yes, in Go, two ways: compile an extension into the binary, or drop a plugin binary into
`--plugins-dir` on a stock release. Both register execution tools, AI providers, secret engines,
and notification channels. See [Extend in Go](sdk.md) and the official
[switchtender-plugins](https://github.com/kordloom/switchtender-plugins) repo for a working example.

## What about scale?

Runs stream and page by a sequence cursor, the runs list and event history paginate, and a large host
matrix renders one cell at a time. Split a run across workers by measured host duration, and run more
than one instance against PostgreSQL when a single binary is not enough.

## What is the license?

Business Source License 1.1: free to self-host and modify, with a restriction on offering it as a
competing hosted service, and it converts to Apache 2.0 after four years.
