<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Migrate off AWX, Semaphore, Rundeck, Jenkins, Chef, Puppet, or cron

SwitchTender imports an AWX, Semaphore, Rundeck, Jenkins, Chef, or Puppet export, or a plain crontab,
and creates the equivalent objects, so moving over is one command rather than a rebuild.

## The migration summary

Every preview prints a summary before the itemized plan, and the API returns the same thing as
`report` on an import response. The plan answers what an import did. The summary answers whether to
attempt one. This is the summary a preview of a small AWX export prints:

    Migration summary:
      Comes across:       13 objects
          credentials          4
          inventories          3
          inventory sources    2
          schedules            2
          projects             1
          templates            1
      Needs a secret:     4 credential shells, because an export never carries secret values
      Does not come across: 1
          - project "Manual" skipped: only git projects import (scm_type="")
      Worth reviewing:    7
          - credential "prod-ssh" needs its secret re-entered; an export never carries secret values. Its non-secret settings (become_method=sudo, become_user=root, user=deploy) were stored on the credential
          - credential "vault-pw" needs its secret re-entered; an export never carries secret values
          - credential "aws-keys" needs its secret re-entered; an export never carries secret values. Its non-secret settings (region=us-east-1, username=AKIAEXAMPLE) were stored on the credential
          - credential "gh-token" needs its secret re-entered; an export never carries secret values
          - inventory source "Legacy EC2" imports the "ec2" plugin as its source; point it at a plugin config file before refreshing
          - schedule "Nightly" from this AWX export fires template "Deploy Web", whose survey has no usable default for the required question "region", so each fire stops with that reason and starts no run. Give the question a default on the template
          - schedule "Every 3 days" from this AWX export fires template "Deploy Web", whose survey has no usable default for the required question "region", so each fire stops with that reason and starts no run. Give the question a default on the template

Two numbers are worth understanding before relying on them.

**Needs a secret is not a limitation of the importer.** An export never carries secret values, from
any of these systems, so a credential arrives as a named shell whoever wrote the tool. The count is
how many credentials need a secret set once before anything uses them. The plan says which of them
held a secret in the source and which held none, such as an AWX machine credential that only names a
user. A run refuses a credential with no secret and says which one, so the choice for the second kind
is to set a secret or to detach the credential from the templates that use it.

**Does not come across is itemized, never summarized.** Each entry names the object and the reason,
because a count with no names cannot be acted on. Nothing is dropped without an entry: a test reads
every warning the importers can raise and fails when one describes a dropped object in wording the
summary would file as merely worth reviewing.

When a very long export pushes the warning list past its cap, the summary says how many were not
listed. A truncated report that looks complete is how somebody concludes an import was clean when it
was only long.

## What each source brings over

There are seven importers, and they do not all carry the same objects. AWX and Semaphore export a
whole control plane, so they bring projects, inventories, credential shells, templates, surveys, and
schedules. Jenkins exports jobs and nothing else, so it brings templates, surveys, and schedules
only, and you name the inventory yourself. Rundeck brings the same three, plus one project when you
hand it a project archive whose source control configuration names a repository this can reach. A
crontab is a list of timed commands, so it brings schedules alone.

Chef and Puppet sit the other way round from all of those. They are not control planes with job
definitions to carry; they are desired-state systems whose work lives in cookbooks and manifests.
So they bring the fleet: every node, grouped by its environment and, for Chef, by every role in its
run list, carrying the facts that identify a machine and the address a play connects on. The
cookbooks and manifests do not come across, and that is not a gap left for later. A recipe is a
program in another language against another model, and a converter that half-translated one would
produce something that reads like the original and does not do what it does. The fleet is the part
that transfers honestly, and it is what you need on day one to put governed runs in front of those
machines while the recipes are dealt with separately.

| Command | Projects | Inventories | Credential shells | Templates | Surveys | Schedules |
|---------|----------|-------------|-------------------|-----------|---------|-----------|
| `import awx` | Yes | Yes, static and dynamic | Yes | Yes, job templates and workflows | Yes | Yes|
| `import semaphore` | Yes | Yes | Yes | Yes | Yes | Yes|
| `import rundeck` | From a project archive whose source control names a reachable repository | No, name one with `--inventory` | No | Yes | Yes | Yes|
| `import jenkins` | No | No, name one with `--inventory` | No | Yes | Yes | Yes|
| `import chef` | No | Yes, the fleet grouped by environment and role | No | No | No | No|
| `import puppet` | No | Yes, the fleet grouped by environment | No | No | No | No|
| `import cron` | No | No, name one with `--inventory` | No | No | No | Yes|

A Rundeck, Jenkins, or crontab import therefore creates no credentials at all. A Rundeck or Jenkins
job that needed a login needs a credential built by hand and attached to its template afterward. A
crontab import creates no template either, so an imported cron line has nothing to attach one to.

## Get an export

- AWX: `awx export` produces a JSON document of organizations, projects, inventories, inventory
  sources, job templates, credentials without secrets, schedules, surveys, and notification
  templates. Teams, and organizations that own no smart inventory and hold no notification
  attachments of their own, are counted in the report rather than imported.
- Semaphore: export the project's repositories, inventories, keys, templates, and schedules as
  JSON. Semaphore's own single-project backup file and the multi-project wrapper are both read.
- Rundeck: either export a project's jobs as YAML or JSON from the project's job list or the API, or
  export the whole project as an archive, which is the zip Project Settings hands back. The importer
  tells the two apart by content, so there is no flag to set and nothing to get wrong.
- Jenkins: there is no export file to fetch. Point the importer at a `JENKINS_HOME`, at its `jobs`
  directory, at one job's `config.xml`, or at a zip of any of those.
- cron: the crontab file itself, either a user tab or, with `--system`, `/etc/crontab` and the
  system tabs.

## Preview, then apply

Run the import without `--apply` first to see exactly what it would create, along with every
warning:

    switchtender import awx awx-export.json --db switchtender.db

The report lists the projects, inventories, dynamic inventory sources, credentials, templates, and
schedules that will be created, prints the content of every inventory it would write rather than
only its name, and calls out anything that could not be mapped cleanly. It goes to standard output,
so redirecting it to a file keeps a copy to review. Apply it when the report looks right:

    switchtender import awx awx-export.json --db switchtender.db --apply

Apply an export once. A second apply is refused before it creates any object when the install already
holds a project, inventory, credential, inventory source, template, or schedule of the same name, and
the refusal names each one. The attempt is still recorded in the audit chain, as every apply is, and
a refused apply against a SQLite path that did not exist leaves the new file behind and says so.
Importing again would create a second copy of each object, and a second copy of a schedule fires as
well. To import a revised export, delete what the first one created, or import into a fresh database.

Semaphore works the same way with `import semaphore`. The importer is proven against a real
backup taken from a live current Semaphore release, unknown newer fields included, not only
against hand-built samples.

## AWX dynamic inventory comes across too

An AWX inventory source imports as well, whether the export carries it at the top level or nests it
under the inventory it belongs to. Each one becomes a dynamic inventory source plus the stored
inventory it maintains, named after the source with `(dynamic)` appended. Its project and credential
are wired by id when the same export carried them, and the report names either one it could not
resolve.

A source pointing at a file in a project keeps that path. A cloud plugin source such as `ec2` has no
file to point at, so it imports carrying the plugin name and the report tells you to point it at a
plugin config file before it can refresh. The backing inventory arrives empty either way, since its
hosts come from running the plugin rather than from the export.

An AWX smart inventory imports with its host filter, and a constructed inventory imports with its
inputs, options, and limit, wired by id to the inventories the same export carried. Both resolve at
each launch, as they did in AWX. The [inventories guide](inventories.md) describes what each reads.

When the export carries a smart inventory's organization, the smart inventory and every inventory
imported from that organization are placed in an organization of the same name, created on apply
or matched to the one already here when exactly one has that name, so its filter reads the same
inventories it read in AWX. Like AWX, a smart inventory carries its hosts and their variables and
none of their groups. A play written for a group needs a constructed inventory instead.

One AWX organization becomes one organization here, whatever brings it across: a smart inventory,
or notification templates attached to the organization itself, whose templates are then placed in
it too. It is matched by the AWX id or name the export references it by, uses the one organization
already here with its name when exactly one has it, and when more than one does, the import does
not guess: what would have been placed in it arrives with no organization, and the report says so.
The import plan and result name the organization every placed template and inventory lands in.
Under `--strict-grants`, an object in an organization is visible only to that organization's
members, and AWX memberships are not imported, so grant access in the organization to everyone who
needs what was placed there.

Semaphore has no equivalent. An inventory of any type other than static imports holding whatever its
export's inventory field held, and the report names the type it was. For a file inventory that field
is a path rather than a host list, so read each one the report names before relying on it.

## AWX fact cache and provisioning callbacks

A job template's `use_fact_cache` and `allow_callbacks` come across as they are, so a template that
kept facts in AWX keeps them here and a template hosts called back to still accepts callbacks. The
report says, for each template, what needs a person afterward:

- A host's boot script calls an address on the AWX server. A template whose AWX id the import knew
  also answers at that address here, as the next section describes. Otherwise point the script at
  the template's own callback URL on this server, `/v1/templates/{id}/callback`. The curl itself,
  posting `host_config_key`, stays the same either way.
- The `host_config_key` an export carries is sealed with this install's encryption key as the
  import applies, so hosts keep the key they hold. On an install with no encryption key, or when
  the export wrote the key as `$encrypted$`, the template arrives with callbacks on and no key, and
  refuses every callback until a key is minted on its page.
- A template that had both a limit and callbacks is named. AWX launches a callback for the calling
  host whatever the limit says. Here the template keeps its limit on callbacks, `callback_limit`
  `intersect`, so a calling host the limit does not select gets no run. Set `callback_limit` to
  `replace` on the template, in its dialog or with `PUT /v1/templates/{id}`, to match AWX.
- A second callback while a run for that host is pending or running answers 409 here, the accurate
  code for a conflict. AWX answers 400 in the same case, so a boot script that checks for exactly
  400, rather than for any 4xx, needs a change.

Facts AWX had already cached do not come across, since an export does not carry them. The first run
of each template gathers them again. With the cache on, the setting is part of what an approval
binds and what a receipt discloses, as [the Ansible
guide](tool-ansible.md#cached-facts-and-approvals) explains.

See [the Ansible guide](tool-ansible.md#fact-cache) for how both behave.

### The AWX-compatible callback address

Machine images, cloud-init user data, launch templates, and kickstart files carry the AWX callback
address, `https://<awx host>/api/v2/job_templates/<awx id>/callback/`, and changing all of them is
often the slowest part of a move. So a template imported from an AWX job template that accepted
callbacks also answers at that address, once the AWX hostname resolves to this server.

The import binds a template to its AWX id when it knows the id. An export that carries each job
template's `id` binds it directly, and `awx export` strips ids, so a plain export carries none.
When yours does not, save the job template list the AWX API serves at `/api/v2/job_templates/` as
JSON and give it to the import:

    switchtender import awx awx-export.json --awx-template-ids job-templates.json --apply

Give `--awx-template-ids` once per page when the list spans pages. An id is taken only for an exact
match of organization and name that the list names once, so a template is never bound to another's
id by a guess, and the report says which templates answer at their AWX address and which do not,
with the reason. An import through the API, `POST /v1/import/awx`, binds the ids the export itself
carries, and takes no list.

How the address behaves:

- It is `POST /api/v2/job_templates/<awx id>/callback/`, with or without the trailing slash, with
  `host_config_key` as JSON or as a form. A callback there passes the same key check, host
  matching, template limit, rate limits, approval gate, and evidence as the template's own address.
  The chain entry records that it arrived on the AWX-compatible address, and the dossier says so.
- It is on by default for every template the import bound, and off is a switch on the template
  page, `awx_callback` in the API. The template page shows when a host last called through it,
  which is how to tell when nothing uses it anymore. There is no sunset date, but moving images to
  the template's own address is the recommendation: it does not depend on the old hostname.
- A binding is for good. Importing the same AWX object again, after its template was deleted,
  points the id at the new template. A different AWX object claiming the same id, judged by
  organization and name, fails the import before anything is written, so a boot script calling an
  id never reaches something else. A template created here never gets an AWX id.
- When the bound template is deleted, the address answers 410 Gone and never reaches any other
  template.
- It says nothing about which ids exist. The rate limit is spent before anything is looked up, and
  an unknown id, a wrong key, and a template with callbacks or this address off all get the same
  403.
- POST is the only method served, and SwitchTender never returns a key. A callback that sends
  `extra_vars` is refused with 400, since a callback launches the template as it is saved.
- The status codes are SwitchTender's, documented in
  [the API reference](api.md#provisioning-callbacks), rather than a copy of AWX's. 409 for a
  second callback while one is pending is the case most likely to differ from what a script expects.
- No other AWX API path is served. This address is the single exception.

Before the cutover, check what the images expect of the old hostname:

- **Certificates.** A boot script that verifies TLS checks the certificate against the AWX hostname.
  The certificate this server presents must carry that name as a subject alternative name, or the
  call fails before it reaches anything.
- **Pinned certificates and CAs.** An image that pins AWX's certificate, or trusts only the CA that
  issued it, refuses a different one even with the right name. Find those images first, since a
  failed callback is silent at boot.
- **Load balancers and proxies.** Host matching and the rate limits read the caller's address. A
  load balancer in front of this server must pass the original address in a header, and be listed
  with `--trusted-proxy`, or every host appears to call from the balancer: none can be matched, and
  all share one rate limit.
- **DNS caching.** Hosts and resolvers keep the old record until its TTL runs out, so lower the TTL
  well before the switch, and expect callbacks to reach AWX for a while after it.

## Import Rundeck jobs or a project archive

Two artifacts import and the importer tells them apart by content, so point it at whichever one you
have.

    switchtender import rundeck jobs.yaml --inventory prod --db switchtender.db
    switchtender import rundeck rundeck-payments-batch.zip --inventory prod --db switchtender.db

The first is a job export, the YAML or JSON list Rundeck writes from a project's job list or from
its API. The second is a project archive, the zip Project Settings hands back for a whole project,
which is what most people leaving Rundeck reach for. The same job produces the same template either
way. The archive writes jobs as XML and the export writes them as YAML, and both are mapped through
one path, so the two artifacts cannot drift into importing the same job differently.

Each job becomes a Bash template carrying its step sequence in order, its options become a survey,
and its schedule becomes a cron schedule. Rundeck dispatches by node filter rather than by inventory
file, so `--inventory` records which hosts the jobs are meant for and the report names any job whose
node filter you should check against it.

Know what that inventory does and does not do. A Rundeck job imports as a Bash template, and the Bash
tool runs the script where the worker runs it; the inventory is carried on the template for you to
act on, not used to fan the script out across those hosts. Rewrite the job as an Ansible template
when you want it to run against the inventory.

**A project archive brings no inventory, and none can be built from it.** The archive carries the
configuration of where Rundeck fetched its nodes from and not one node definition, at any path. A
`file` source names a path on the Rundeck server, a `url` source names an endpoint, and neither
travels in the zip. The report names each source it saw, with its path or its URL, so you can fetch
the same list and create the inventory yourself. Rebuilding hosts out of the archive's execution
history would be worse than not trying: it holds bare node names, only for the nodes that happened
to run, with no address, login, or connection attributes, so the result would be a fleet of machines
nobody could reach. A path carrying `%PROJECT_BASEDIR%` is left as it stands, because that token is
the project's own directory on the server that exported it and expanding it here would be a guess.

**A project archive brings one project only when its source control configuration names a repository
this can reach.** A Rundeck project is a job store rather than a git repository, so there is nothing
to make a project out of unless the SCM plugin was mirroring the jobs into one. Where it was, the
repository and its branch come across. A `file:///` URL does not: it names a directory on the
Rundeck server, which is a different machine, so a project built from it either fails on its sync or
finds an unrelated directory of that name on this host and clones that instead. The report names the
path so you can push that repository somewhere reachable and create the project yourself. Where a
project is created, know what is in it: the repository holds the job definitions Rundeck exported
into it, not Ansible playbooks, so point your templates at your own playbook repository if that is a
different one.

An archive also carries execution logs, run state, reports, ACL policies, webhooks, and the project
readme. None of them import. Approvals and access here come from policies and grants you write, not
from a Rundeck ACL, and a webhook is a trigger you create against this server.

That freight is why a busy project's archive can be too large to upload. `/v1/import/{format}` and
the Migrate page cap a body at 25 MiB and answer a larger one with 413. The command line applies no
such cap: it reads the file whole, and only the archive reader's own ceilings bound it at 20,000
archive members, 4 MiB for any one member it reads, and 64 MiB of definitions read in total. So an
archive over 25 MiB imports with `switchtender import rundeck` rather than through the page.

Two details are worth knowing before you run it. A Rundeck schedule is a Quartz expression, which
counts Sunday as one where cron counts Sunday as zero, so the weekday is renumbered rather than
copied. A Quartz-only form that cron has no reading for, such as the third Friday of the month
(`6#3`), the last Friday (`6L`), the last day of the month (`L`), or the last weekday of the month
(`LW`), comes across as an RFC 5545 recurrence that fires on the same days rather than as a cron
expression. The nearest-weekday form (`15W`) is the one a single recurrence cannot say exactly, so
it is reported instead of converted to a day it would fire wrongly on. And a secure option becomes a
secret survey field, whose answer is sealed rather than stored on the run as text. Its default lives
in Rundeck's key storage and does not come across, so the report names any option that had one.

## Import Jenkins jobs

Jenkins keeps each job's definition in a `config.xml` under `JENKINS_HOME/jobs`, so there is no
single export file to fetch. Point the importer at the directory and it reads the tree.

    switchtender import jenkins /var/jenkins_home --inventory prod --db switchtender.db

A `JENKINS_HOME`, its `jobs` directory, one job's directory, one job's `config.xml`, or a zip of
the `jobs` directory all work. A job is named by the directory holding its `config.xml`, so a zip
made inside one job's directory, with that `config.xml` at its top, is refused with the reason
rather than imported unnamed.
Folders are followed and each job keeps its full name, so a job in the `platform` folder imports as
`platform/db-vacuum`. To import from the web page instead, zip the `jobs` directory and upload it.
Each job keeps its build history and workspace under its own directory, which is far more files
than the definitions and is what pushes a zip past the 20,000 entries and 25 MiB an upload reads,
so leave those out:

    zip -r jobs.zip jobs -x '*/builds/*' '*/workspace/*'

With 7-Zip on Windows, `7z a jobs.zip jobs -xr!builds -xr!workspace` does the same. Both also
leave out a job that is itself named `builds` or `workspace`, so add that job's definition
afterward by its own path, for example `zip jobs.zip jobs/builds/config.xml`. The zip reads a job's
`config.xml` only where the directory walk would, so an archived artifact, a workspace file, a
multibranch branch, or a promotion that happens to be named `config.xml` is passed over rather
than reported as a job that does not come across.

Each job's shell steps become one Bash template, in order, opening with `set -e` so it stops at the
first failure the way the build did. Parameters become a survey. Every line of the build trigger
becomes a schedule.

**Only freestyle jobs are imported.** A Pipeline job is a Groovy program, and there is no honest
mechanical translation from one into a template, so Pipeline, multibranch, matrix, and Maven jobs are
each named in the report and skipped rather than half-imported into something that would not do what
the job did. Rebuild those as pipelines here, or leave them in Jenkins.

Four things behave differently enough to know about before you run it.

**Jenkins `H` notation is resolved to real times.** Jenkins writes `H` where a number would go and
hashes the job name to spread load, so `H 2 * * *` means "some minute past two, the same one every
time." A cron parser rejects the letter outright, so these are not approximated, they are resolved:
the job keeps its cadence and its window, and the report names the expression it became. The minute
inside that window may differ from the one Jenkins chose, because the hash is not Jenkins' own.

**A poll trigger is not imported as a schedule.** `Poll SCM` asks the repository whether anything
changed and builds only if it did, so a job polling every five minutes usually does nothing.
Imported as a plain schedule it would instead run for real every five minutes. It is reported and
skipped; trigger those from a webhook instead.

**Jenkins build variables are not set here.** A step reading `$WORKSPACE`, `$BUILD_NUMBER`, or
`$JOB_NAME` gets an empty string, so the report names every one it found. Supply them as survey
fields or extra vars, or rewrite the step.

**Password parameters become secret fields.** Jenkins stores a password parameter encrypted, and a
secret survey field seals its answer the same way, so it arrives as a secret field. Its default is
Jenkins ciphertext and does not come across, and the report names it. A job's remote trigger token
is never imported, since it would grant a launch to anybody holding it, and it is named and left
out.

Two smaller ones: a Windows batch step is skipped, since the rest of the job imports as Bash, and a
job whose source control checkout mattered has its repository named rather than attached, because
attaching a project changes the directory every relative path in the script resolves against.

## Import a Chef or Puppet fleet

Both of these answer the same question: the machines are managed by something whose open source line
has no supported road ahead, and the fleet needs to be somewhere else before that matters.

Chef reads what the server stores about its nodes. Any of the three shapes the tooling emits works:
an array of node documents, a single node, or an object keyed by node name.

    knife node list | xargs -I{} knife node show {} -l -F json > chef-nodes.json
    switchtender import chef chef-nodes.json --db switchtender.db

Each node becomes a host. Its `chef_environment` becomes a group, and every `role[...]` in its run
list becomes a group as well, so a play can target `[webserver]` on the day of the import. The ohai
attributes that identify a machine come across as host variables, and `ipaddress` also becomes
`ansible_host`, because a Chef estate addresses machines by certname and has no reason to have kept
DNS in step. The rest of a node's automatic attributes are left out on purpose: they run to hundreds
of keys per host, and copying them produces an inventory nobody can read.

Puppet reads PuppetDB queries, or the plain certname list when PuppetDB is not reachable. Export the
nodes and their facts, put both in one document, and import it once:

    curl -s "$PUPPETDB/pdb/query/v4/nodes" > puppet-nodes.json
    curl -s "$PUPPETDB/pdb/query/v4/facts" > puppet-facts.json
    jq -s add puppet-nodes.json puppet-facts.json > puppet.json
    switchtender import puppet puppet.json --db switchtender.db

Nodes group by environment. Deactivated and expired nodes are left out and counted, because Puppet
itself stopped managing them and a play targeting everything would otherwise reach for machines
nothing owns. The facts carry each host's address. The two go in one document because only the nodes
query says which nodes are deactivated: facts imported alone bring those back, which the report says,
and a second import of the same fleet is refused as a copy of the first. Without facts the hosts
import as names with no address, and the preview says so rather than letting an inventory that
reaches nothing look complete.

## Import a crontab

Fleets that still schedule work from a crontab can bring it under governance in one step. Point the
importer at a crontab file and it turns each cron line into a governed schedule, so every firing is
approved, recorded, and provable like any other run instead of running unseen on one box.

    switchtender import cron /var/spool/cron/crontabs/deploy --inventory prod --db switchtender.db

A crontab names no target host, so `--inventory` records which inventory the jobs belong to. It does
not move where they run: each line imports as a shell step, and a shell step runs on the SwitchTender
host, not on the machine the crontab came from. The report says so on every cron import. Change a step
to Ansible when you want it to run against the inventory. The name is kept on each schedule as a path on the
server, since a schedule's own steps cannot name a stored inventory, and the report says so. To run a
line against a stored inventory, make it a template that targets that inventory and schedule the
template. Add `--system` to read
`/etc/crontab` and the system tabs, which carry a user column the report calls out. Comments and
environment lines are noted and skipped. As with the other imports, leave off `--apply` to preview
the schedules first, then re-run with `--apply` to create them.

No template is created by a crontab import. Each imported schedule carries its own one-step bash
pipeline, so the plan shows schedules and nothing else. This importer is command line only: the
`/v1/import/{format}` endpoint and the Migrate page in the UI take awx, semaphore, chef, puppet,
rundeck, and jenkins, not cron. Both cap a body at 25 MiB, which the command line does not, so a Rundeck project
archive over that size imports from the CLI alone.

## What maps to what

| Source | Becomes |
|--------|---------|
| AWX git project | Project, with its source control credential attached when that credential is an SSH key. A username and password credential is reported, since a project here syncs a private repository over SSH with a key.|
| AWX inventory | Stored inventory, rendered as INI from its hosts and groups. A host entry that does not read, such as one whose enabled field is a word, is skipped and named in the report, and the rest of the inventory imports. The host is left out of every group that lists it too, so no play reaches it.|
| AWX inventory source | Dynamic inventory source, plus the stored inventory it maintains, named `<source> (dynamic)`. A file source keeps its path; a cloud plugin source imports carrying the plugin name and is reported, since it needs a config file before it can refresh.|
| AWX job template | Template, with job slicing becoming shard count. Privilege escalation arrives as `ansible_become: true` in its extra vars, which the report notes, since an extra var also outranks a play that sets `become: false`.|
| AWX survey | Template survey, field for field, with the field types translated. A password prompt becomes a secret field, sealed rather than downgraded to plain text. A survey switched off in AWX is not imported, since AWX never asks it.|
| Semaphore survey | Template survey, field for field. A secret variable becomes a secret field, sealed rather than downgraded to plain text.|
| AWX schedule that stops | Schedule carrying its recurrence rule, which stops after its `COUNT` or on its `UNTIL` the way AWX stops it. A rule that has already fired its last time is reported and not imported, since it would never run.|
| AWX workflow job template | Workflow template carrying the graph, with each node's job template inlined as a step and the success and always edges becoming dependencies. Imported whole or reported and skipped, never partially.|
| AWX job template schedule | Schedule, with its timezone kept. A rule that a cron expression says exactly, checked against the rule's own next fires, becomes cron. Every other rule, such as every third day, the last Friday of the quarter, more than one `RRULE`, or an `EXRULE` or `EXDATE` that takes a holiday out, comes across as the RFC 5545 recurrence it is. A schedule that answers its template's survey fires a copy of the template whose questions default to those answers. A secret answer does not come across, so a schedule that gave one arrives switched off and the report names the question to give a default. A schedule firing a template whose required secret question has no default arrives switched off the same way, since no export carries that default readably and every fire would stop until one is set.|
| AWX workflow schedule | Schedule on the imported workflow template, read from whichever place the export carried it. A workflow that was refused has no template to fire, so its schedules are named in the report as not imported rather than dropped silently.|
| AWX credential | Credential shell with its kind mapped from its type and its configured inputs, secret omitted. A become password beside the connection secret arrives as a second shell, attached wherever the first one is.|
| AWX notification template | Notification target of the same channel, its address sealed at rest, and attached for the same events to the templates and workflows the export attaches it to. A secret AWX exports only as `$encrypted$`, a Slack bot token, a PagerDuty token, or a Grafana key, arrives waiting to be entered, and the report says which. A target waiting for its secret keeps every other part the export carried, a Grafana instance's address included, so finishing it asks for the secret alone. A Slack template arrives waiting for an incoming webhook address, since a Slack target here posts to one. A Twilio template that texts several numbers becomes a target per number. An IRC template has no equivalent and is reported.|
| AWX notification attachment on an organization | Attached to the organization, so every template in it is covered, one created later included. The organization is created from the same export, matched by its AWX id or name, with the templates imported from it placed in it. When this install already holds one organization of that name, that one is used instead of a second. When it holds several, the import does not guess, and the attachments fall back to each template imported from the organization, never both. The report states which happened: imported, matched existing, fell back to N templates, or unresolved when there was no template to fall back to either.|
| AWX notification attachment on a project | Reported, not imported. AWX tells those targets about project updates, and a run here syncs its project itself.|
| AWX organization or team | A team is counted in the report, not imported. An organization is imported when the export places something in it: a smart inventory, or notification templates attached to the organization itself. Any other is counted in the report, not imported. The report names what to create by hand in place of what was not imported.|
| Semaphore repository | Project, cloned with its access key when that key is an SSH key. A login and password key is reported, since a project here clones a private repository over SSH with a key.|
| Semaphore static inventory | Stored inventory, carrying its SSH key and become key, so every run against it has them.|
| Semaphore inventory of any other type | Stored inventory holding whatever the export's inventory field carried, which for a file inventory is a path rather than hosts, and reported. Only static content travels in the export.|
| Semaphore template | Template, with survey variables mapped and run by the tool Semaphore ran it with. A Bash, Python, or PowerShell template runs its script from the project checkout with its arguments. A Terraform or OpenTofu template imports as a plan, since a run here applies without asking, and the workspace its inventory picked is reported. A template for a tool with no equivalent here, such as Pulumi, is refused and named. Its vault keys arrive as vault passwords it unlocks.|
| Semaphore key | Credential shell of the kind its use makes it: an SSH key, the login an inventory reaches its hosts with, a become password, or a vault password. A key of type none holds nothing and is not imported.|
| Semaphore schedule | Schedule.|
| Rundeck job | Template running the job's step sequence as one Bash script. The same job from a job export and from a project archive produces the same template.|
| Rundeck option | Survey field. An enforced value list becomes a choice, and a secure option becomes a secret field, sealed rather than downgraded. The script sets `RD_OPTION_<NAME>` from the answer, or the option's default, and `@option.name@` and `${option.name}` in a step read the same value. A name with a dash or a dot becomes a survey variable with an underscore in its place. An option's `regex` becomes the field's pattern, checked against the whole answer as Rundeck checks it.|
| Rundeck script step naming an interpreter | Kept when the interpreter is a shell. Anything else, a Python interpreter or a command such as `sudo -u deploy /bin/bash`, is refused and named: a template runs one Bash script, so the step body would not be run by what the job ran it with. A script that names no interpreter is judged by its `#!` line the same way.|
| Rundeck step arguments, error handler, retries, and notifications | Reported as left out. A script runs with no arguments, a failed step ends the job or is passed over when the job keeps going, a failed run is not retried, and the template's notifications are set by hand.|
| Rundeck project archive `resources.source.N` | Reported, never imported. The archive carries the node source's configuration and no node definitions, so the report names the file path or the endpoint and you attach an inventory of your own.|
| Rundeck project archive SCM configuration | Project, when it names a repository this can reach, carrying the repository and its branch. A `file:///` URL is refused and named, since it is a path on the Rundeck server. The export's path template, format, and committer identity have no equivalent and do not carry.|
| Rundeck project archive ACL policy, webhook, execution, or report | Not imported. Access here comes from policies and grants, a webhook is a trigger created against this server, and the execution history is not a source of hosts.|
| Rundeck schedule | Schedule, with the Quartz expression converted and its weekday renumbered.|
| Rundeck job timeout | Template timeout, converted to whole seconds from a duration such as `30m` or from a plain number of seconds. One that cannot be read is reported and the template imports with no timeout.|
| Rundeck dispatch thread count | Recorded on the template as its fork count. It paces Ansible runs; a Bash template, which is what a Rundeck job imports as, does not read it. The report does not name it, so check it yourself if it mattered.|
| Jenkins freestyle job | Template running the job's shell steps as one Bash script, in order.|
| Jenkins folder | Nothing of its own. Its jobs keep the folder in their names, so `platform/db-vacuum` stays that.|
| Jenkins Pipeline job | Refused and named. Groovy has no mechanical translation into a template.|
| Jenkins parameter | Survey field. A choice becomes a choice and a boolean a toggle, and a password parameter becomes a secret field, sealed rather than downgraded. The script sets the parameter under its own name from the answer, or its default, as Jenkins did.|
| Jenkins build trigger | One schedule per line, with `H` resolved to concrete times, the `@daily` family expanded, and Sunday renumbered from 7 to 0. A `TZ=` line sets the timezone of every line after it, as in Jenkins.|
| Jenkins poll trigger | Refused and named. It builds only on a change, so importing it as a schedule would run the job unconditionally.|
| Jenkins build timeout | Template timeout, converted from minutes to seconds.|
| Jenkins agent label | Reported, not imported. SwitchTender targets an inventory, so check the one you attached covers the same machines.|
| Crontab job line | Schedule carrying its own one-step bash pipeline. No template is created. The command runs as cron would run it: `\%` is a literal percent sign, and what follows the first unescaped `%` is fed to the command as its input.|
| Crontab `@reboot` line | Refused and named. It has no time-based cadence to convert.|
| Crontab comment or environment assignment | Skipped. A comment goes quietly; an environment line is named, since an imported schedule does not carry it.|
| `/etc/crontab` user column | Reported, not imported. The schedule runs under the server's execution account, so confirm that is equivalent.|

## Re-enter secrets

Exports never contain secrets by design, so credentials import as named shells with their kind set
but no material. The report lists which secrets to re-enter. Everything else, projects, inventories,
dynamic inventory sources, templates, and schedules, is in place and ready to run once the secrets
are restored. Only AWX and Semaphore carry credentials at all, so a Rundeck, Jenkins, or crontab
import has none to re-enter and none to attach.

## Limits to expect

- A schedule whose cadence a cron expression cannot represent, such as every third day, is reported
  and skipped rather than converted to a wrong cadence.
- A workflow whose graph cannot be expressed whole is reported and skipped rather than reduced. A
  failure edge, which runs work precisely because something failed, has no pipeline equivalent, and a
  node pointing at a job template the export does not carry has no work to do. A node that runs a
  nested workflow, a project or inventory sync, or a system job is refused the same way, since a step
  runs a playbook, even when that object shares its name with a job template in the export. So is a
  workflow whose job templates come from more than one project, since a workflow template sources
  every step from one, and a node that runs one node always and another only on success, since a
  step here lets everything after it continue or nothing. A partial graph would keep the workflow's
  name and run a subset of it, which is worse than not importing it.
- A workflow with an approval node imports with the node as an approval step. Its name and
  description become what the approver is shown, its timeout carries, the nodes after it wait for
  the approval, and its failure nodes become its deny path, which runs on a denial or a timeout as
  AWX runs a failure path. The assessment names each workflow whose gate comes across. One shape is
  refused: an approval node with an always edge, since that edge runs the next node whatever the
  approver decides. That workflow is reported and named in the governance section as a gate that
  does not come across, and a workflow holding only approval nodes is reported as having no work for
  them to release. Who may approve follows your roles and approval policies, not the AWX approval
  role.
- A workflow's own schedules import onto the workflow template, so a graph that fired nightly in AWX
  keeps firing nightly here. The one case that does not carry is a workflow the import refused: with
  no template to fire, its schedules cannot import either, and the report says so by name and count
  rather than leaving the loss to be discovered later.
- An AWX export's teams, and its organizations that own no smart inventory and hold no
  notification attachments of their own, are counted in the report and not imported. The report names what to create in their place.
- Non-git projects are skipped, since there is no repository to source playbooks from.
- A Rundeck project archive never produces an inventory. It carries no node definitions, so the node
  sources it named are reported and you attach an inventory yourself. Every imported template lands
  against the inventory you named with `--inventory`, the same as from a job export.
- A Rundeck archive whose source control points at a `file:///` path produces no project, since the
  path is on the Rundeck server rather than on a machine this can clone from. It is reported by
  path, so you can push the repository somewhere reachable and create the project yourself.
- An archive whose members would escape where they are unpacked, by an absolute path, a parent
  segment, or a symbolic link, is refused whole rather than read down to the members that looked
  reasonable. No Rundeck export writes one.
- An AWX custom credential type in the export imports as a custom type, with its fields and its
  env, extra var, and file injectors, and each credential of it arrives as a credential of that type
  waiting for its field values. A type whose injectors use Jinja beyond a field or a file path, such
  as a filter or a condition, is refused with the reason, since a run here would receive the
  expression as literal text, and its credentials fall back to the kind mapping below.
- A custom type that writes a file no env or extra var injector references comes across as it was,
  marked as imported from AWX. The report names the type and the file among the items worth
  reviewing, the server logs a warning naming both when the import is applied through it, and every
  run of a credential of the type carries a warning naming the file. A type defined in SwitchTender
  is refused for the same thing. See
  [a file nothing references](secrets.md#a-file-nothing-references).
- A custom type that does nothing but write a kubeconfig to a file comes across unchanged, so its
  credentials keep masking every line of the document. The report names the type and gives each of
  its credentials the one request that switches it to the built-in `kubeconfig` kind, which masks
  only the secrets inside the document. See
  [kubeconfig types from AWX](secrets.md#kubeconfig-types-from-awx).
- A credential type without an exact match maps to the environment kind and is flagged for review.
- A survey field that prompts for a secret, an AWX password question, a Semaphore secret variable, a
  Rundeck secure option, or a Jenkins password parameter, imports as a secret survey field. Its
  answer is sealed with the credential key and never stored on the run as text, so it is as
  protected as it was in the tool you left. A default for one does not come across, because an
  export carries it only as a placeholder or in the source tool's own encryption, and the report
  names each field that had one so you can set it on the template. A schedule firing a template
  whose required secret question has no default arrives switched off, since every fire would stop
  until one is set, and the report names the question beside each such schedule.
- Secrets are never in an export, so every credential is created as a shell and its secret has to be
  re-entered. The non-secret settings AWX did export, such as the user to connect as and how to
  become root, are stored on the credential itself and take effect at injection, so a machine
  credential arrives knowing its connection user and only the secret needs entering. Only known
  non-secret fields are stored, so a custom credential type cannot spill a secret into the plan.
- An AWX machine credential covers both key and password login under one type. Which one it is comes
  from whether the export configured a key or a password, so a password credential does not arrive as
  a key credential that fails the first time it runs.
