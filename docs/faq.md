# FAQ

Short answers to the questions teams ask when evaluating or adopting Yardmaster. New here? Start with
the [quickstart](quickstart.md), or [switching from AWX](switching-from-awx.md) if you are migrating.

## What can Yardmaster run?

Ansible playbooks, Bash scripts, Terraform, Python, and Go. A run, a saved template, or a single step of
a pipeline picks its tool. A pipeline can mix them: Terraform to build infrastructure, Ansible to
configure it, Bash to smoke-test it, as one dependency graph. Bash shells out to anything on the
host, so kubectl, the cloud CLIs, and your own scripts all work.

## Do I need Kubernetes?

No. Yardmaster is one binary that is the API, the executor, the scheduler, and the UI. State is one
database: a SQLite file to start, or PostgreSQL when you want more than one instance. No Redis, no
operator, no separate task engine.

## What is a dry run?

A run that makes no changes. Ansible runs in check mode, Terraform runs plan instead of apply, and
Bash and Python are syntax checked without executing. A dry run set on a sharded run is carried to
every shard, so a preview is always a preview.

## How do I migrate from AWX?

From the command line:

    yardmaster import awx export.json           # preview the plan, changes nothing
    yardmaster import awx export.json --apply    # import

It maps projects, inventories, job templates with their surveys and schedules and job slicing, and
credentials. See [switching from AWX](switching-from-awx.md) for the full mapping.

Two things to know. AWX omits secret values on export for security, so credentials import without
their secrets and you set those once after importing. And AWX dynamic inventory sources are not
mapped yet. Static inventories import fully. A Semaphore importer exists too, with
`yardmaster import semaphore export.json`.

## How do I rerun the same job on a set of hosts without re-entering everything?

Save it as a template. A template bundles the tool, the command or playbook, the inventory, the
credentials, and the variables, so a rerun is one click or one API call. Stored inventories let you
name a host set once and reuse it across templates.

## How are secrets stored?

Sealed with AES-256-GCM, the key derived from an operator passphrase through argon2id. Secrets
decrypt only at execution, into a temporary file wiped when the run finishes, and never appear in API
responses. Kinds include SSH keys, vault passwords, become passwords, container registry logins, and
environment bundles, which is where API tokens and other KEY=VALUE secrets go. Set
`YARDMASTER_ENCRYPTION_KEY` and `YARDMASTER_ENCRYPTION_SALT` to enable them.

A credential can also be a command source, so the value lives in an external store instead of in
Yardmaster. Yardmaster seals a command, for example `vault kv get -field=password secret/prod` or an
`aws secretsmanager get-secret-value` call, and runs it at execution time. The command's output is
the secret and is never stored. This works with any secret manager reachable from a command: Vault,
AWS, GCP, 1Password, or your own script.

## Are webhook triggers safe to expose?

Yes. A trigger can require a verified HMAC signature, keyed by a per-trigger secret sealed at rest and
separate from the URL, so a leaked webhook URL cannot forge a push. Set the same secret on the git
host, turn enforcement on, and an unsigned or wrong request is rejected before anything runs. Secrets
rotate at any time.

## Does it support single sign-on?

Yes, OpenID Connect, with just-in-time account provisioning and a configurable default role.

## What about scale?

Runs stream and page by a sequence cursor, the runs list and event history paginate, and a large host
matrix renders one cell at a time. Split a run across workers by measured host duration, and run more
than one instance against PostgreSQL when a single binary is not enough.

## What is the license?

Business Source License 1.1: free to self-host and modify, with a restriction on offering it as a
competing hosted service, and it converts to Apache 2.0 after four years.
