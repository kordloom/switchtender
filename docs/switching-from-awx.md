<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Switching from AWX

This guide assumes you know AWX and have never run SwitchTender. It gets you from an AWX setup to a
working SwitchTender run two ways: import what you already have, or build it from scratch to learn the
pieces. If a term is unfamiliar, the [concepts](concepts.md) page defines it.

## What is different, in one paragraph

SwitchTender runs the same playbooks against the same inventories, and drives Bash, Terraform, and
Python besides, but there is no Kubernetes, no Redis, and no separate task engine to operate. One
binary is the API, the executor, the scheduler,
and the UI. State is one database: a SQLite file to start, or PostgreSQL when you want more than one
instance. You still have projects, inventories, templates, surveys, schedules, and credentials. They
just live behind a smaller, faster surface.

## The mental model

| In AWX | In SwitchTender |
|--------|---------------|
| Organization | Organization. A project, template, inventory, or credential can name its owning `org_id`. Access adds a global role and optional per-object grants.|
| Project (git) | Project.|
| Inventory | Stored inventory, or a dynamic inventory source that refreshes into one.|
| Smart or constructed inventory | Smart or constructed inventory, resolved at each launch. A smart inventory imports into its AWX organization and, as in AWX, carries hosts and their variables but no groups. See [inventories](inventories.md).|
| Job template | Template.|
| Survey | Template survey, the same typed questions.|
| Schedule | Schedule, with the same recurrence rule, or a cron expression when one says the same thing.|
| Notification template | Notification target, attached to templates, workflows, schedules, projects, and organizations for the same events.|
| Credential | Credential, secret sealed at rest.|
| Job | Run.|
| Job slicing | Split, balanced by measured host duration.|
| Workflow | Pipeline, ordered steps or a dependency graph.|
| Instance group | Worker queue.|
| Execution node on a Receptor mesh | Relay worker, which dials out to the control node and receives each run's credentials sealed to its pool's delivery key. See [Delivering secrets to relay workers](configuration.md#delivering-secrets-to-relay-workers).|
| Execution environment | A container image pinned on a template, a run, or a project, once `--allow-container-ee` is on and the host has Docker or Podman. Without one, a run uses the server's own ansible-core: the one on the PATH, or a pinned one `switchtender ansible install` sets up beside the database. See [managed Ansible runtime](ansible-runtime.md).|
| Fact cache, `use_fact_cache` | The template's `use_fact_cache`, the same jsonfile cache with a timeout per template.|
| Provisioning callback | The template's `allow_callbacks` and host config key, at `/v1/templates/{id}/callback`, and for a template imported from AWX also at its old AWX address, `/api/v2/job_templates/{awx id}/callback/`.|

## Path A: import your AWX

The fast path. It reads an AWX export and creates the equivalent SwitchTender objects.

1. Export from AWX with its own CLI: `awx export` produces a JSON document of your projects, inventories, job
   templates, credentials without secrets, schedules, and surveys.

2. Preview the import. Nothing is written yet. You get a report of exactly what would be created and
   every warning.

        switchtender import awx awx-export.json --db switchtender.db

3. Apply it.

        switchtender import awx awx-export.json --db switchtender.db --apply

4. Re-enter secrets. Exports never contain secrets, so credentials arrive as named shells. The
   report lists which ones to fill in. Open the UI, go to Credentials, and set each secret. The
   non-secret settings AWX exported, the connection user and become method, are already stored on
   each credential, so the secret is the only thing left to type. Until then, everything else is
   already in place.

5. Launch a template and watch it run.

The full mapping and its limits are in the [migration guide](migration.md).

## Path B: set it up from scratch

Do this once to understand the pieces, even if you imported. It mirrors the order you would build a
job template in AWX.

### 1. Start the server

    export SWITCHTENDER_ENCRYPTION_KEY=$(openssl rand -hex 32)
    export SWITCHTENDER_ENCRYPTION_SALT=$(openssl rand -hex 16)
    ./switchtender serve --addr :8080 --db switchtender.db

The key and salt seal credentials at rest. Keep both: a server started with a different pair cannot
open the credentials sealed under this one. Open
http://localhost:8080 for the UI. On an empty database the first start mints an initial admin token
and prints it once, so copy it before moving on.

### 2. Create your first account

    SWITCHTENDER_PASSWORD=secret ./switchtender user new admin-you --role admin --db switchtender.db

Sign in through the UI with that username and password. Roles are admin, operator, and viewer: admins
manage configuration, operators launch and cancel runs, viewers read.

### 3. Add a project

A project is a git repository your playbooks live in. In the UI, open Projects and add one with its
repository URL and branch. For a private repository, first add an SSH key credential (next step) and
select it on the project. Every run records the exact commit it executed.

### 4. Add credentials

Open Credentials and add what your runs need. Kinds:

- `ssh_key`: an SSH private key, used to reach hosts and to clone private git projects.
- `ssh_password`: a machine login, injected as the `ansible_user` and `ansible_password` variables
  through a file, so the password stays off the command line.
- `vault_password`: an Ansible Vault password.
- `become_password`: a privilege escalation password, delivered without touching the command line.
- `become`: privilege escalation with an optional method and user, injected as the
  `ansible_become_*` variables through a file.
- `network`: a network device login, injected as the `ansible_user`, `ansible_password`,
  `ansible_network_os`, and `ansible_connection` variables.
- `env`: `KEY=VALUE` lines injected into the run, how cloud SDK credentials reach plugins.
- `token`: a single API token or JWT, exposed to the run as the `SWITCHTENDER_TOKEN` environment variable.
- `registry`: a container registry login, for pulling a pinned execution image.
- `aws`: an AWS access key, injected as the standard `AWS_*` environment variables.
- `azure`: an Azure service principal, injected as the `ARM_*` variables Terraform reads and the
  `AZURE_*` variables the Ansible azure collection reads.
- `gcp`: a Google Cloud service account JSON, bound to `GOOGLE_APPLICATION_CREDENTIALS`.
- `vmware`: a vCenter login, injected as the `VMWARE_*` environment variables the
  community.vmware modules read.
- `openstack`: an OpenStack login, injected as the `OS_*` environment variables
  openstacksdk and the openstack.cloud collection read.
- `kubeconfig`: a Kubernetes kubeconfig, written to a private file bound to `KUBECONFIG`,
  `K8S_AUTH_KUBECONFIG`, and `KUBE_CONFIG_PATH`.

A custom credential type from AWX imports as a custom type here, file injectors included, when its
injectors are plain field and file path substitution. See
[custom credential types](secrets.md#custom-credential-types). A type that only writes a kubeconfig
to a file keeps masking every line of it until you switch its credentials to the built-in
`kubeconfig` kind, one request each, as described in
[kubeconfig types from AWX](secrets.md#kubeconfig-types-from-awx). A type that writes a file nothing
references comes across with a warning on every run that uses it, as described in
[a file nothing references](secrets.md#a-file-nothing-references).

Secrets are encrypted at rest and never returned by the API.

### 5. Add an inventory

Open Inventories and paste an inventory, or point a dynamic source at an inventory plugin or script
that refreshes into one. An inventory is referenced by id, so any run or template can target it.

### 6. Create a template

A template is the equivalent of an AWX job template: a saved preset of a project, a playbook path, an
inventory, a shard count, credentials, and extra vars. Add one in Templates. Give it a survey if you
want typed prompts at launch. Launch it with one click.

### 7. Watch the run

The run detail page paints a host-by-task matrix live as the run executes, with per-task drill-down
into stdout, stderr, return code, and diff. A run is structure, not
a text scroll.

### 8. Add capacity and schedules

On a Team license, point a worker at the same database to add an executor, and give it a queue name
to target specific work:

    ./switchtender worker --db switchtender.db --name worker-1

Add a schedule in Schedules with a cron expression or an RFC 5545 recurrence rule to fire a template
on a cadence. A schedule, a webhook trigger, and a provisioning callback fire a template with each
survey question's default, a password question's sealed default included. A template whose survey
has a required question with no default is refused with the question named, on the schedule and in
the audit trail, rather than run without the answer, so give such a question a default before you
schedule its template.

## Where things live differently

- There is no separate "launch" wizard. A template launch is one request, a survey
  renders as a small form.
- Workflows are built on the canvas at Workflows: add steps, drag them into place, wire dependencies
  by dragging from a step's edge onto another, and run the graph as a pipeline.
- Organizations exist and objects belong to them: create one with `POST /v1/orgs`, add members with
  a role, and a project, template, inventory, or credential can name its owning `org_id`. On top of
  that, access is a global role plus optional per-object grants, which AWX's tree does not have.
  Grant a user or a team `use` or `manage` on one specific object.
- Notifications are configured on the server with `--notify-*` flags and cover eleven channels:
  webhook, Slack, Mattermost, Rocket.Chat, Discord, Microsoft Teams, ntfy, PagerDuty, Grafana,
  Twilio SMS, and email. Every finished run reaches every channel configured that way, and a run
  held for approval reaches the chat channels and webhooks, and email when `--notify-on` is
  `finish`, so the person who decides is told. A template can additionally name its own targets in
  the template dialog, for all eleven channels: a PagerDuty target names its own routing key, a
  Grafana target its own instance and token, and a Twilio or email target names only a recipient and
  sends through the server-held account. A notification target defined once can be attached to many
  templates, workflows, schedules, projects, and organizations for when a run starts, succeeds,
  fails, or is held, the way a notification template is attached in AWX, and managed on the
  Notifications page. A target hears each run's events in the run's order, a failed message is
  retried and then kept as failed on the run and the target, and the next one still goes. See
  [notification targets](api.md#notification-targets).

## What is not one to one yet

- Execution environments are a single pinned container image behind a flag, not a managed catalog.
- A workflow approval node imports as an approval step, with its timeout and its failure path. An
  approval node with an always edge is the exception: it would run the next node whatever the
  approver decides, so that workflow is reported and not imported rather than imported without its
  gate. Who may approve follows your roles and approval policies rather than the AWX approval role.
- Import places a template or an inventory in an organization only when the export brings that
  organization across, through a smart inventory it owns or notification templates attached to it,
  as the [migration guide](migration.md) describes. Everything else belongs to no organization.
  Under the default access model that leaves it usable by every operator, which matches how a
  single-team install already works. If you run with strict grants, imported objects have no
  grants yet and AWX memberships are not imported, so assign access after importing.
- A provisioning callback keeps the template's own limit unless the template's `callback_limit` is
  `replace`: a calling host the limit does not select gets no run, where AWX launches for it. The
  import report names every template this applies to.
- A second provisioning callback while one for the same host is pending or running answers 409
  here, where AWX answers 400. A boot script that checks for exactly 400 needs a change. The
  [migration guide](migration.md#the-awx-compatible-callback-address) lists every callback answer
  and what to check before pointing the old AWX hostname here.

If something you rely on is missing, open an issue. Import coverage is widened on purpose.
