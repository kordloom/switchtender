<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Concepts

## Runs

A run is one execution of a playbook against an inventory. Yardmaster shells out to
`ansible-playbook` and captures both a human log and a structured event stream through an embedded
callback plugin, so a run is queryable data, not just scrollback. Every run records its status,
timing, exit code, the extra vars going in, and the `set_stats` outputs coming out.

## Splits

A split shards one inventory across parallel slices that run the same playbook, each limited to its
hosts, with the parent rolling up the result into one merged host matrix. Hosts are packed into
shards by their measured average duration in recent runs, so each shard carries a similar amount of
work. A finished split can retry only its failed shards.

## Pipelines

A pipeline runs playbook steps in order, or as a dependency graph when steps declare what they
depend on. Each step is itself a run, so it gets the full matrix, events, and history. A step can
retry on failure, and values a step publishes with `set_stats` flow to the steps that depend on it.

## Projects

A project sources playbooks from a git repository. Every run records the exact commit it executed,
so history answers what version ran, not just what file name. A project can install its
`requirements.yml` roles and collections on each sync, and can pin a container image its runs
execute inside.

## Templates

A template is a saved launch preset bundling a project, playbook, inventory, shard count,
credentials, and extra vars. One click in the UI or one POST launches it. A template can also
declare a survey.

## Inventories and sources

A stored inventory is inventory content referenced by id and materialized on whichever executor
runs the play. A dynamic inventory source runs an inventory plugin or script and refreshes the
result into a stored inventory, with cloud authentication supplied by a credential.

## Triggers

A webhook trigger is a URL that launches a template on an inbound git push. The project syncs
fresh first, so the run deploys the commit that was just pushed.

## Credentials

A credential is a secret sealed with AES-256-GCM, decrypted only at execution into a temporary file
and wiped afterward. Kinds are SSH keys, vault passwords, environment bundles for cloud SDKs,
become passwords, and container registry logins. Secrets never appear in API responses.

## Teams and grants

The global roles, admin, operator, and viewer, decide the broad strokes. On top of them, a grant
gives a user or a team the right to use or manage a specific project, template, inventory, or
credential. Grants are additive: an object with no grants defers to the global role, so nothing
changes on upgrade until grants are added.

## Queues and workers

A worker is any process running the executor against the shared store. Every process, the server
included, competes for pending runs through the store. A run can target a named queue, and only
workers serving that queue run it, which places work across a mixed fleet. A lease keeps a run
attributable, and a janitor requeues work whose holder went away.
