<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Migrate off AWX, Semaphore, Rundeck, Jenkins, or cron

SwitchTender imports an AWX, Semaphore, Rundeck, or Jenkins export, or a plain crontab, and creates
the equivalent objects, so moving over is one command rather than a rebuild.

## What each source brings over

There are five importers, and they do not all carry the same objects. AWX and Semaphore export a
whole control plane, so they bring projects, inventories, credential shells, templates, surveys, and
schedules. Jenkins exports jobs and nothing else, so it brings templates, surveys, and schedules
only, and you name the inventory yourself. Rundeck brings the same three, plus one project when you
hand it a project archive whose source control configuration names a repository this can reach. A
crontab is a list of timed commands, so it brings schedules alone.

| Command | Projects | Inventories | Credential shells | Templates | Surveys | Schedules |
|---------|----------|-------------|-------------------|-----------|---------|-----------|
| `import awx` | Yes | Yes, static and dynamic | Yes | Yes, job templates and workflows | Yes | Yes|
| `import semaphore` | Yes | Yes | Yes | Yes | Yes | Yes|
| `import rundeck` | From a project archive whose source control names a reachable repository | No, name one with `--inventory` | No | Yes | Yes | Yes|
| `import jenkins` | No | No, name one with `--inventory` | No | Yes | Yes | Yes|
| `import cron` | No | No, name one with `--inventory` | No | No | No | Yes|

A Rundeck, Jenkins, or crontab import therefore creates no credentials at all. A Rundeck or Jenkins
job that needed a login needs a credential built by hand and attached to its template afterward. A
crontab import creates no template either, so an imported cron line has nothing to attach one to.

## Get an export

- AWX: `awx export` produces a JSON document of organizations, projects, inventories, inventory
  sources, job templates, credentials without secrets, schedules, and surveys. Organizations, teams,
  and notification templates are counted in the report rather than imported.
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
only its name, and calls out anything that could not be mapped cleanly. Apply it when the report
looks right:

    switchtender import awx awx-export.json --db switchtender.db --apply

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

Semaphore has no equivalent. An inventory of any type other than static imports holding whatever its
export's inventory field held, and the report names the type it was. For a file inventory that field
is a path rather than a host list, so read each one the report names before relying on it.

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

Two details are worth knowing before you run it. A Rundeck schedule is a Quartz expression, which
counts Sunday as one where cron counts Sunday as zero, so the weekday is renumbered rather than
copied; a Quartz-only form such as the third Friday of the month has no cron equivalent and is
reported instead of converted to a day it would fire wrongly on. And a secure option is never
imported as a survey field, because a survey answer is stored in plain text on the run; the report
names each one so you can store it as a credential instead.

## Import Jenkins jobs

Jenkins keeps each job's definition in a `config.xml` under `JENKINS_HOME/jobs`, so there is no
single export file to fetch. Point the importer at the directory and it reads the tree.

    switchtender import jenkins /var/jenkins_home --inventory prod --db switchtender.db

A `JENKINS_HOME`, its `jobs` directory, one job's `config.xml`, or a zip of a `JENKINS_HOME` or a
`jobs` directory all work. A job is named by the directory holding its `config.xml`, so a zip whose
only entry is a bare `config.xml` is refused with the reason rather than imported unnamed.
Folders are followed and each job keeps its full name, so a job in the `platform` folder imports as
`platform/db-vacuum`. To import from the web page instead, zip the `jobs` directory and upload it;
zip that directory rather than the whole `JENKINS_HOME`, which also holds every build log and
workspace.

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

**Secrets are never imported.** A password parameter is stored encrypted by Jenkins, and a survey
answer is stored in plain text on the run, so importing one would quietly downgrade it. The same
goes for a job's remote trigger token. Both are named in the report and left out.

Two smaller ones: a Windows batch step is skipped, since the rest of the job imports as Bash, and a
job whose source control checkout mattered has its repository named rather than attached, because
attaching a project changes the directory every relative path in the script resolves against.

## Import a crontab

Fleets that still schedule work from a crontab can bring it under governance in one step. Point the
importer at a crontab file and it turns each cron line into a governed schedule, so every firing is
approved, recorded, and provable like any other run instead of running unseen on one box.

    switchtender import cron /var/spool/cron/crontabs/deploy --inventory prod --db switchtender.db

A crontab names no target host, so `--inventory` records which inventory the jobs belong to. It does
not move where they run: each line imports as a shell step, and a shell step runs on the SwitchTender
host, not on the machine the crontab came from. The report says so on every cron import. Change a step
to Ansible when you want it to run against the inventory. When the name matches a stored inventory, the
import wires the object by id; anything else is used as a path on the server and the report says which. Add `--system` to read
`/etc/crontab` and the system tabs, which carry a user column the report calls out. Comments and
environment lines are noted and skipped. As with the other imports, leave off `--apply` to preview
the schedules first, then re-run with `--apply` to create them.

No template is created by a crontab import. Each imported schedule carries its own one-step bash
pipeline, so the plan shows schedules and nothing else. This importer is command line only: the
`/v1/import/{format}` endpoint and the Migrate page in the UI take awx, semaphore, rundeck, and
jenkins, not cron.

## What maps to what

| Source | Becomes |
|--------|---------|
| AWX git project | Project.|
| AWX inventory | Stored inventory, rendered as INI from its hosts and groups.|
| AWX inventory source | Dynamic inventory source, plus the stored inventory it maintains, named `<source> (dynamic)`. A file source keeps its path; a cloud plugin source imports carrying the plugin name and is reported, since it needs a config file before it can refresh.|
| AWX job template | Template, with job slicing becoming shard count.|
| AWX survey | Template survey, field for field, with the field types translated. A password prompt is refused, not downgraded to plain text.|
| Semaphore survey | Template survey, field for field. A secret variable is refused, not downgraded to plain text: store its value as a credential.|
| AWX schedule that stops | Refused and named. A rule with `COUNT` or `UNTIL` bounds itself, a cron entry never stops, and importing one would leave a job firing forever.|
| AWX workflow job template | Workflow template carrying the graph, with each node's job template inlined as a step and the success and always edges becoming dependencies. Imported whole or reported and skipped, never partially.|
| AWX job template schedule | Schedule, with the recurrence rule converted to cron and its timezone kept.|
| AWX workflow schedule | Schedule on the imported workflow template, read from whichever place the export carried it. A workflow that was refused has no template to fire, so its schedules are named in the report as not imported rather than dropped silently.|
| AWX credential | Credential shell with its kind mapped from its type and its configured inputs, secret omitted.|
| AWX organization, team, or notification template | Counted in the report, not imported. The report names what to create by hand in their place.|
| Semaphore repository | Project.|
| Semaphore static inventory | Stored inventory.|
| Semaphore inventory of any other type | Stored inventory holding whatever the export's inventory field carried, which for a file inventory is a path rather than hosts, and reported. Only static content travels in the export.|
| Semaphore template | Template, with survey variables mapped.|
| Semaphore key | Credential shell.|
| Semaphore schedule | Schedule.|
| Rundeck job | Template running the job's step sequence as one Bash script. The same job from a job export and from a project archive produces the same template.|
| Rundeck option | Survey field. An enforced value list becomes a choice; a secure option is refused, not downgraded.|
| Rundeck script step naming an interpreter | Kept when the interpreter is a shell. Anything else, a Python interpreter or a command such as `sudo -u deploy /bin/bash`, is refused and named: a template runs one Bash script, so the step body would not be run by what the job ran it with.|
| Rundeck project archive `resources.source.N` | Reported, never imported. The archive carries the node source's configuration and no node definitions, so the report names the file path or the endpoint and you attach an inventory of your own.|
| Rundeck project archive SCM configuration | Project, when it names a repository this can reach, carrying the repository and its branch. A `file:///` URL is refused and named, since it is a path on the Rundeck server. The export's path template, format, and committer identity have no equivalent and do not carry.|
| Rundeck project archive ACL policy, webhook, execution, or report | Not imported. Access here comes from policies and grants, a webhook is a trigger created against this server, and the execution history is not a source of hosts.|
| Rundeck schedule | Schedule, with the Quartz expression converted and its weekday renumbered.|
| Rundeck job timeout | Template timeout, converted to whole seconds from a duration such as `30m` or from a plain number of seconds. One that cannot be read is reported and the template imports with no timeout.|
| Rundeck dispatch thread count | Recorded on the template as its fork count. It paces Ansible runs; a Bash template, which is what a Rundeck job imports as, does not read it. The report does not name it, so check it yourself if it mattered.|
| Jenkins freestyle job | Template running the job's shell steps as one Bash script, in order.|
| Jenkins folder | Nothing of its own. Its jobs keep the folder in their names, so `platform/db-vacuum` stays that.|
| Jenkins Pipeline job | Refused and named. Groovy has no mechanical translation into a template.|
| Jenkins parameter | Survey field. A choice becomes a choice and a boolean a toggle; a password parameter is refused, not downgraded.|
| Jenkins build trigger | One schedule per line, with `H` resolved to concrete times, the `@daily` family expanded, and Sunday renumbered from 7 to 0.|
| Jenkins poll trigger | Refused and named. It builds only on a change, so importing it as a schedule would run the job unconditionally.|
| Jenkins build timeout | Template timeout, converted from minutes to seconds.|
| Jenkins agent label | Reported, not imported. SwitchTender targets an inventory, so check the one you attached covers the same machines.|
| Crontab job line | Schedule carrying its own one-step bash pipeline. No template is created.|
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
  node pointing at a job template the export does not carry has no work to do. A partial graph would
  keep the workflow's name and run a subset of it, which is worse than not importing it.
- A workflow's own schedules import onto the workflow template, so a graph that fired nightly in AWX
  keeps firing nightly here. The one case that does not carry is a workflow the import refused: with
  no template to fire, its schedules cannot import either, and the report says so by name and count
  rather than leaving the loss to be discovered later.
- An AWX export's organizations, teams, and notification templates are counted in the report and not
  imported. The report names what to create in their place.
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
- A credential type without an exact match maps to the environment kind and is flagged for review.
- A survey field that prompts for a secret, an AWX password question, a Semaphore secret variable, a
  Rundeck secure option, or a Jenkins password parameter, is reported and left out rather than
  imported. A survey answer is stored in plain text on the run, so importing one would quietly make
  the secret less protected than it was in the tool you left. Store those values as credentials and
  reference them from the template.
- Secrets are never in an export, so every credential is created as a shell and its secret has to be
  re-entered. The non-secret settings AWX did export, such as the user to connect as and how to
  become root, are stored on the credential itself and take effect at injection, so a machine
  credential arrives knowing its connection user and only the secret needs entering. Only known
  non-secret fields are stored, so a custom credential type cannot spill a secret into the plan.
- An AWX machine credential covers both key and password login under one type. Which one it is comes
  from whether the export configured a key or a password, so a password credential does not arrive as
  a key credential that fails the first time it runs.
