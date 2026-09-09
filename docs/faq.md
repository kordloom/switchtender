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

One caveat on the second half of that sentence. SQLite is free forever, but creating a new
PostgreSQL database is a Team feature: a Community or Pro install pointed at an empty PostgreSQL
refuses to start and says `Initializing a new PostgreSQL database requires a Team license`. Opening
a PostgreSQL database that already holds the schema is never gated, in any license state, so a
lapsed license never locks a server out of its own data.

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
run; static inventories import fully.

## What else can I migrate from?

Five sources in all. AWX is the one above; the other four are:

    switchtender import semaphore export.json
    switchtender import rundeck jobs.yaml --inventory prod
    switchtender import jenkins /var/jenkins_home --inventory prod
    switchtender import cron /etc/crontab --system --inventory prod

Semaphore brings projects, inventories, credential shells, templates, surveys, and schedules, the
same kinds AWX does apart from the dynamic inventory sources AWX alone carries. Rundeck and Jenkins
export jobs and nothing else, so they bring templates, surveys, and schedules, and `--inventory`
names the hosts those templates target. A crontab brings schedules alone, one per job line, and no
template for them to fire, so each carries its own one-step bash pipeline. That step runs on the
SwitchTender host, not on the machine the crontab came from, and the report says so on every cron
import. Rundeck, Jenkins, and cron create no credentials at all. The table is in
[what each source brings over](migration.md#what-each-source-brings-over).

Jenkins has no single export file: point the importer at a `JENKINS_HOME`, at its `jobs` directory,
at one job's `config.xml`, or at a zip of a `JENKINS_HOME` or a `jobs` directory. A job is named by
the directory holding its `config.xml`, so a zip whose only entry is a bare `config.xml` is refused
with the reason rather than imported under a name it does not have. Only freestyle jobs import,
since a Pipeline job is a Groovy program with no honest mechanical translation. The
`/v1/import/{format}` endpoint and the Migrate page in the UI take awx, semaphore, rundeck, and
jenkins; a crontab imports from the command line only.

## How do I rerun the same job on a set of hosts without re-entering everything?

Save it as a template. A template bundles the tool, the command or playbook, the inventory, the
credentials, and the variables, so a rerun is one click or one API call. Stored inventories let you
name a host set once and reuse it across templates.

## How are secrets stored?

Sealed with AES-256-GCM, the key derived from an operator passphrase through argon2id. Secrets
decrypt only at execution, into the run's environment or a temporary file created mode 0600 and
deleted when the run finishes, and never appear in API responses. Fourteen kinds cover SSH keys and
SSH passwords, vault passwords, become passwords and full become settings, network device logins,
environment bundles, API tokens, container registry logins, and typed AWS, Azure, GCP, VMware, and
OpenStack credentials. Set `SWITCHTENDER_ENCRYPTION_KEY` and `SWITCHTENDER_ENCRYPTION_SALT` to
enable them.

A credential does not have to be stored here at all. Nine managers are read natively at launch:
HashiCorp Vault, Vault dynamic secrets, AWS Secrets Manager, AWS STS, Google Secret Manager, Azure
Key Vault, CyberArk Conjur, CyberArk Central Credential Provider, and 1Password Connect. The value
lives in the manager you already run and never rests in this database.

A credential can also be a command source, for a store none of those covers. SwitchTender seals a
command, for example `vault kv get -field=password secret/prod` or a call to an internal tool, and
runs it at execution time. The command's output is the secret and is never stored. Reach for it
when your store has no native source here. The nine above need no command and no vendor CLI on the
runner.

## Are webhook triggers safe to expose?

Yes. A trigger can require a verified HMAC signature, keyed by a per-trigger secret sealed at rest and
separate from the URL, so a leaked webhook URL cannot forge a push. Set the same secret on the git
host, turn enforcement on, and an unsigned or wrong request is rejected before anything runs. Secrets
rotate at any time.

## Does it support single sign-on?

Yes, on Pro and above: OpenID Connect, SAML, LDAP, and JWT directory sign-in, with just-in-time
account provisioning and a configurable default role. Community signs in with local accounts and
API tokens. Starting a server with a directory configured but no Pro license is refused at startup,
rather than quietly serving an unauthenticated directory.

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
and notification channels. See [Extend in Go](sdk.md), which carries a complete extension you can
build and drop in, and the seams it can register.

## What about scale?

Runs stream and page by a sequence cursor, the runs list and event history paginate, and a large host
matrix renders one cell at a time. An Ansible run shards across the inventory by measured host
duration, and the server executes those shards in its own concurrent slots on any tier.

Sharding is Ansible only. It fans one playbook across inventory hosts, which is a thing Ansible
does and the other tools do not: a Bash, Python, PowerShell, Go, Terraform, or OpenTofu run, or one
using a tool an extension registered, executes once wherever it is claimed. Submitting one with a
shard count is not an error and does not warn. It simply runs as a single run, with no `kind` of
`split` and no shard count on it, so read the submitted run back if you expected a fan-out.

Going wider is Team. Separate worker processes and initializing a new PostgreSQL database for more
than one instance both need a Team license. Opening a PostgreSQL database that already holds the
schema is never gated, in any license state, so a lapsed license never locks a server out of its own
data, though starting a worker process after one lapses is refused.

## Does SwitchTender need an agent on each host?

No. It reaches the machines it manages over SSH, the same way Ansible does, and installs nothing
on them. There is no per-host daemon to deploy, patch, or account for.

What you do run is the server itself, one binary, plus optionally a few extra worker processes
against the same store when one machine is not enough throughput. Those extra processes are
distributed workers, a Team feature. A worker is a pool member that picks up queued runs, not an
agent belonging to a particular target, so their number has nothing to do with how many hosts you
manage.

## Which Semaphore is this an alternative to?

Semaphore UI, at semaphoreui.com, which was called Ansible Semaphore until it was renamed. It is
the open-source web UI for running Ansible, Terraform, and scripts, and it is the tool
`switchtender import semaphore` reads a backup from.

It is not Semaphore CI/CD by Rendered Text, at semaphoreci.com, which is a hosted continuous
integration service for building and testing code. The two share a name and nothing else.
SwitchTender does not compete with it: if you need to compile code and run test suites on every
pull request, that is a build pipeline, and this is not one.

## Can it read secrets from Vault, AWS, Azure, Google, CyberArk, or 1Password?

All of them, natively, resolved at launch rather than copied into this database. Nine external
managers ship as credential sources: Vault, Vault dynamic secrets, AWS Secrets Manager, AWS STS,
Google Secret Manager, Azure Key Vault, CyberArk Conjur, CyberArk Central Credential Provider, and
1Password Connect. Each is an HTTP call the server makes, so no vendor CLI or SDK goes on the
runner.

Vault is read over its HTTP API and handles KV v1 and v2. Vault dynamic secrets go further: a
fresh, short-lived credential is minted for each run and revoked when the run ends, so nothing
long-lived exists to leak. AWS STS is the same idea on AWS: it assumes a role and mints short-lived
role credentials for each run. AWS Secrets Manager is read over a Signature Version 4 signed
request, and credentials fall back to the standard AWS environment, so an instance role needs no
stored key. Azure Key Vault authenticates with a service principal or, on Azure, the attached
managed identity, again with no stored key. Google Secret Manager reads a secret version over the
Secret Manager API with an access token from the config or, on GCP, from the metadata server, so an
attached service account needs no stored key either. Conjur exchanges an API key for a short-lived
access token, the Central Credential Provider authenticates the application by client certificate
or allowed-machine rule, and 1Password reads a field of an item through a self-hosted Connect
server.

A store outside that set resolves through a command credential, whose standard output becomes the
secret, so an internal tool works with no integration to write.

## Can I run a Terraform plan, hold it for approval, then run Ansible?

Yes, and that sequence is the reason pipelines exist here rather than a workaround for them.

A pipeline is an ordered set of steps or a dependency graph with parallel branches, built on a
drag-and-drop canvas or posted as JSON. Steps can mix tools freely, so a Terraform plan, an
approval, and an Ansible play are three steps of one run. A failure skips exactly the steps that
depended on it, and `set_stats` output flows to dependent steps as extra vars.

The approval is not a convention someone can skip. A policy decides which runs are held, the hold
is enforced in the core, and the approval is bound to the exact plan that was reviewed, so a run
cannot be approved as one thing and executed as another.

A Community install holds one approval policy and Pro holds five. Team removes the cap and adds the
rest of the policy engine: outright denials, risk floors, actor-scoped rules, and distinct-approver
separation of duties.

## How fine-grained is access control?

Beyond the global admin, operator, and viewer roles, users group into teams and a team gets read,
use, or manage on one specific project, template, inventory, or credential, each level implying
the ones below it.

That covers the two cases a global role cannot: a read grant shows somebody one object without
making them a viewer of everything, and a manage grant lets somebody edit and delete one object
without making them an admin. Grants layer on top of the global role and default open, so an
object nobody granted falls back to the caller's global role. Turn on `--strict-grants` when you
want an ungranted object to be invisible instead.

## How mature is SwitchTender?

Young, and worth saying plainly. AWX has years of production use, a large community, and a
commercial edition behind it. Semaphore UI has an established user base. SwitchTender has neither,
and if what you need most is a tool thousands of people have already hit the edges of, that is a
real reason to choose one of them.

What offsets it is verifiability rather than reputation. The audit trail is hash-chained and
signed, and a third party can verify a run offline with a separate open-source verifier, so the
claims do not rest on trusting the vendor. The license converts to Apache 2.0 on a fixed schedule
and the source is available now, so the project outliving the company is a documented path rather
than a hope. The continuity and vendor risk pages set out what happens if this goes away.

## What is the license?

Business Source License 1.1: free to self-host and modify, with a restriction on offering it as a
competing hosted service, and it converts to Apache 2.0 two years after each release. The
Community tier is free and complete on its own. Pro adds directory sign-in and five approval
policies instead of one. Team adds the full policy engine, the period change register, distributed
workers, initializing a new PostgreSQL database, and one-click drift reconcile. Both unlock with a
signed license file the binary verifies offline, flat per organization by fleet band, and a lapsed
license takes nothing: paid features stop while your data, evidence, and every Community feature
keep working. There is no license server and nothing ever phones home.
