<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="160">
  </picture>
</p>

# SwitchTender CLI

One binary, a handful of commands. This page is a map; the full flag and environment reference is in
[docs/configuration.md](../docs/configuration.md).

## serve

Runs the HTTP API, the executor, the cron scheduler, the retention sweeper, and the web UI.

    switchtender serve --addr :8080 --db switchtender.db

The `--db` value is a SQLite file path, or a `postgres://` DSN for the PostgreSQL backend. The
schema carries forward on every open, so upgrading in place is just restarting with the new binary.

## worker

Leases pending runs from the shared store and executes them. Point a worker and a server at the
same database, a PostgreSQL DSN for separate machines, and they compete for work.

    switchtender worker --db postgres://user:pass@host:5432/switchtender?sslmode=disable --name worker-1

## ansible

Installs a pinned ansible-core into a Python virtual environment beside the database, checking every
file pip installs against a hash the binary carries, so runs that need Ansible have it without an
Ansible on the PATH. `list` shows what is installed and `remove` takes it away. The full page is
[managed Ansible runtime](../docs/ansible-runtime.md).

    switchtender ansible install --db switchtender.db

## import

Migrates from AWX, Semaphore, Rundeck, Jenkins, Chef, Puppet, or cron. Reports what it would create
on standard output, then writes it with `--apply`. AWX and Semaphore bring projects, inventories,
credential shells, templates, surveys, and schedules. Rundeck and Jenkins bring templates, surveys,
and schedules, against the inventory `--inventory` names, and neither brings an inventory of its
own. `import rundeck` takes either a job export or a project archive, told apart by content, and an
archive brings one project when its source control configuration names a repository this can reach.
A crontab brings schedules alone, and Chef and Puppet bring the fleet as a stored inventory and
nothing else. The per-source table is in
[what each source brings over](../docs/migration.md#what-each-source-brings-over).

    switchtender import awx export.json --db switchtender.db --apply
    switchtender import rundeck project-archive.zip --inventory prod --db switchtender.db --apply
    switchtender import jenkins /var/jenkins_home --inventory prod --db switchtender.db --apply

## token

Manages API tokens. Creating the first token turns authentication on.

    switchtender token new --name ci --db switchtender.db

## user

Manages accounts with admin, operator, and viewer roles.

    switchtender user new operator-jane --role operator --db switchtender.db

## demo

Seeds a fresh database with sample data and real runs, then serves it read-only. Safe to expose.

    switchtender demo --addr :8080

## version

Prints the build version.
