<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Ansible runs

An Ansible run executes `ansible-playbook` against an inventory. It is the default tool. A run
that names no tool is an Ansible run. It is also the most instrumented one, because the embedded
callback plugin reports every task on every host as a structured event, and those events paint the
live host-by-task matrix, feed fleet memory, and drive drift detection.

The server and each worker need ansible-core, which the binary does not carry. They take the
`ansible-playbook` and `ansible-inventory` from `--ansible-bin` when it is set, then from the pinned
ansible-core `switchtender ansible install` sets up, then from the PATH, as
[which Ansible a run uses](ansible-runtime.md#which-ansible-a-run-uses) describes. A run in a
container image uses the image's own.

## What runs

The playbook path is the run's target, resolved inside the project checkout when the run sources a
project, so relative paths and roles behave the way they do in the repository. A project's
`requirements.yml` roles and collections install on sync before the play starts. A dry run passes
`--check`, which reports what would change without changing it; drift detection is built from
exactly those check runs. Ansible still runs any play, block, task, role, or include that sets
`check_mode: false` for real under `--check`, and a `pipe` lookup runs its command on the controller
while a template renders, under `--check` too. The gate reads for both before it treats a dry run as
a preview, and a dry run that forces real work or runs a `pipe` lookup is held by a rule that
excludes dry runs as though it were a real run. The hold names the task and the two clean fixes:
rework it so check mode is safe, or drop `exclude_dry_run` from that rule. Other lookups and plugins
run code on the controller too, and the scan does not judge them, so a rule that excludes dry runs
trusts the playbooks it lets through. An agent's check run waits for a person whatever the scan
finds, unless an exemption covers it. See [dry runs and
`exclude_dry_run`](concepts.md#dry-runs-and-exclude-dry-run), which covers the common read-only
command and a role the project does not hold.

Ansible runs are the ones that split. A split run shards the inventory across parallel slices,
packs hosts onto shards by their measured durations from past runs, and merges every slice back
into one matrix. Failed shards retry alone, and `relaunch-failed` re-runs only the hosts a finished
run left failed or unreachable.

A run against a smart inventory the native engine resolved is checked once more just before the play
starts: the executor's own `ansible-inventory`, with the run's environment, project `ansible.cfg`,
and image, reads the inventory, and the run is refused with the exact difference if it reads anything
other than what the native engine resolved. See
[which engine resolves an inventory](inventories.md#which-engine-resolves-an-inventory).

A run or template may carry the usual Ansible controls without a hand-built command: `tags` and
`skip_tags` select or exclude tagged plays and tasks, `forks` sets how many hosts run at once,
`verbosity` from one to four raises logging as `-v` through `-vvvv`, and `diff_mode` shows the
before and after of every changed file. They are Ansible-only and the other tools ignore them.

## How values reach the play

- Extra vars, including survey answers and template vars, arrive as Ansible extra vars, so
  `{{ region }}` in a play reads a survey answer named `region`.
- An `ssh_key` credential is decrypted to a temp file created mode 0600 for the connection and
  deleted when the run ends. A passphrase protected key is unlocked in memory first, so the passphrase never
  reaches disk or a command line and no prompt blocks the run.
- A `vault_password` credential unlocks `ansible-vault` content for the duration of the run.
- A `become_password` credential supplies privilege escalation.
- An `env` credential's `KEY=VALUE` lines are set in the process environment.
- Credentials attached to the run's inventory arrive the same way, so a fleet's secrets reach the
  play without naming them on the run.

## Fact cache

A template with `use_fact_cache` on keeps the facts each launch gathers and serves them to the next
launch, the way AWX's setting of the same name does. Before the play starts, the facts the
template's stored inventory holds for its hosts are written into a private directory, and Ansible's
`jsonfile` cache plugin is pointed at it. Each host is written in both layouts the plugin has used:
a file named after the host, which ansible-core 2.18 and earlier read, and the `s1_` file
ansible-core 2.19 and later read, so the cache works whichever release executes the run. A play with `gather_facts: false` then
reads `ansible_facts` from the cache, and a play that gathers rewrites the files. When the run ends,
the files Ansible rewrote are stored as those hosts' new facts, and a host whose file a play removed
with `meta: clear_facts` loses its cached facts. The directory is removed when the run is over. It
sits in a locked run directory beside the run's credential files, so a process killed mid-run leaves
it to the same sweep every server and worker on the host runs, as [Run files](run-files.md)
describes.

- `fact_cache_timeout` is how many seconds cached facts stay fresh enough to serve, counted from
  when they were gathered, the time Ansible wrote the host's cache file, rather than from when the
  run that gathered them ended. A host whose facts are older is left out of the cache, so the play
  gathers it again. Zero serves cached facts however old they are.
- When two launches gather the same host, the cache keeps the later gather, whichever launch
  finishes last.
- Facts are kept per host of a stored inventory, so the template needs `inventory_id`. Deleting the
  inventory deletes its cached facts, and a run still going when it is deleted keeps none.
- A run keeps facts only for hosts its inventory names or that its recap shows it reached, so a play
  cannot plant facts for a host the inventory does not hold.
- A host's fact document larger than one mebibyte is not kept, and the run says so in its warning.
- Facts are runtime data. They are never written to the audit chain, a receipt, an evidence bundle,
  a log, or a backup.
- A relay worker reaches the control node without a database, so a run it executes uses no cache
  and says so on the run. A worker that opens the database directly uses the cache like the server.

### Cached facts and approvals

A cached fact stands in for one the play would have gathered. A play that reads its facts from the
cache acts on each host as it was when the facts were cached, not as it is now: a `when:` condition
on the distribution version, the package manager a task picks, or an interface address a template
renders all read the cached value, so a host rebuilt or reconfigured since then is handled as it
was. That makes the setting part of what an approver decides on, so it is bound like the rest of
the run's spec.

- With the cache on, the run's spec records it and its timeout, as
  `"fact_cache": {"timeout_seconds": 3600}`, or `0` for no timeout. An approval binds to that spec,
  and the executor refuses an approved run whose fact cache setting changed after the approval.
- A held run shows "uses cached facts (timeout 3600s)", or "uses cached facts (no timeout)", where
  the approver decides, and the run's dossier states it in the same words. Its receipt discloses
  the spec, so the setting travels with the evidence.
- With the cache off, the spec carries no fact cache field at all. A run that does not use the
  cache keeps exactly the digest it always had, so receipts and approvals issued before the setting
  was bound still match their runs.

### Reading cached facts

The cached facts of an inventory are readable at `GET /v1/inventories/{id}/facts` and per host at
`GET /v1/inventories/{id}/facts/{host}`, and on the Inventories page. Reading them takes the
operator role and read on the inventory, and a host is listed only when the caller may also read
the run that gathered its facts. Values whose keys look like secrets are masked for anyone below
admin. A server started with `--fact-cache-admin-only` restricts reading cached facts to admins,
for an install where even masked facts are more than operators should see. Clearing a host's cached
facts, `DELETE /v1/inventories/{id}/facts/{host}`, takes the admin role and manage on the inventory
whatever that setting says. A container run mounts the cache directory writable, so an execution
environment reads and writes it the same way.

## Provisioning callbacks

A template with `allow_callbacks` on lets a host in its stored inventory launch it against itself,
which is how a machine configures itself at boot. It is AWX's provisioning callback.

1. Turn on `allow_callbacks` on the template and mint a key on the template page, or with
   `POST /v1/templates/{id}/callback-key`. The key is shown once. Minting again rotates it, and
   turning callbacks off revokes it.
2. Give the key to the hosts, usually in their boot script or cloud-init user data.
3. The host posts the key to the template's callback URL:

```bash
curl -k -f -i -H 'Content-Type: application/json' -X POST \
  -d '{"host_config_key": "hck_..."}' \
  https://switchtender.example.com/v1/templates/tpl_9d41c2/callback
```

An AWX boot script that posts `host_config_key` as JSON or as a form works once its URL points
here, since a template's id changes when it is imported.

The server matches the caller to one host of the template's inventory by AWX's rules. The caller's
names are the address the request came from and what that address reverse resolves to. A host
matches when its `ansible_host`, or its inventory name when it sets none, is one of those. When that
does not single out one host, every inventory name is resolved forward, and a host whose name
resolves to the caller's address matches. Behind a reverse proxy the caller's address is read from
`X-Forwarded-For` or `--client-ip-header` only when the proxy is listed with `--trusted-proxy`,
exactly as the sign-in limiter reads it, so a stranger cannot claim another host's address.

- A caller that matches no host, or more than one, is refused, and the run is never created.
- The run is limited to the matched host by its inventory name and runs the template as it is
  saved. A callback cannot pass `extra_vars`. Every survey question takes its default, a secret
  question's sealed default included, the same as on a schedule or a webhook, and a template whose
  survey has a required question with no default is refused, because nobody is there to answer it.
- While a callback run for the same host and template is pending, held for approval, or running, a
  second callback is refused with 409 rather than stacked, which is AWX's replay protection. AWX
  answers 400 in the same case, so a boot script that checks for exactly 400 needs a change.
- Callbacks are rate limited per address, and wrong keys spend a smaller budget of their own. See
  [rate limits](#callback-rate-limits).
- The launch goes through the approval policies and deny rules like any other launch. A policy can
  hold callback runs for approval, and a deny rule refuses them.
- The callback is written to the audit chain before the run is created, as actor
  `host <name> from <address>` with actor type `host`. The run carries the same actor, source
  `callback`, and the template id, so its evidence shows it was host initiated and from where.
- The key is sealed with the server encryption key at rest and is never returned after it is
  minted. An install without an encryption key cannot mint one.
- The inventory must be stored or synced. An inventory resolved from Vault, a secret manager, or a
  command when a run starts holds no host list to match against, so its callbacks are refused.
- The host list is read by the native inventory engine exactly as Ansible reads the document,
  ranges, ports, and quoting included. A document only Ansible can read, such as a plugin
  configuration, is refused for callbacks with the reason.

### The template's own limit

A template's `limit` still applies to a callback unless the template says otherwise, in its
`callback_limit` setting:

- `intersect`, the default: the calling host must also be one the limit selects. A host outside it
  is refused with 403 and a reason naming the setting, nothing is launched, and the server log
  names the template, the host, and the limit.
- `replace`: the calling host replaces the limit, the way AWX does.

A template with no limit launches for the matched host either way. Whether a host falls within a
limit is decided by `ansible-inventory` on the server, over the stored inventory, so groups and
their children, exclusions, intersections, and patterns read exactly as they do when the template
launches. The answer is reused for thirty seconds per inventory and limit, so a fleet booting at
once runs `ansible-inventory` once rather than once per host. A server without `ansible-inventory`
refuses a callback that needs the check with 409 and says so, and one whose evaluation fails answers
503 rather than guessing. An imported AWX template gets the default, and the import report names
each one that had both a limit and callbacks.

### Callback rate limits

Callbacks are counted per client address, on the native and the AWX-compatible address together,
so alternating between them gains nothing.

- `--callback-rate-limit`, 30 by default, is how many callbacks one address may make in a minute.
- `--callback-key-failure-limit`, 10 by default, is how many wrong keys one address may present in
  a minute. Past it, every callback from that address is refused for the rest of the minute, the
  right key included, so a key cannot be guessed at any useful rate.

A refusal answers 429 with `Retry-After: 60`, names the limit and the flag that sets it, and is
logged once per address and limit a minute rather than once per request.

Both limits count the address the server sees, and so does host matching. Behind a NAT every host
reaches the server from the NAT's address, so the hosts share one budget, and none of them can be
matched to an inventory host by address either. Route callbacks so they arrive from each host's own
address, or through a reverse proxy that passes the original address in a header and is listed with
`--trusted-proxy` (and `--client-ip-header` when the header is not `X-Forwarded-For`). That fixes
the matching and gives each host its own budget. Raise the limits only for a fleet that still meets
them once the address path is right.

### The AWX-compatible callback address

A template imported from an AWX job template that accepted callbacks also answers at the address AWX
gave it, `POST /api/v2/job_templates/{awx id}/callback/`, when the import knew its AWX id. A boot
script written for AWX keeps working once the AWX hostname resolves to this server. It is the only
AWX path served. A callback there passes the same key check, host matching, limits, gate, and
evidence as one to the template's own address, and the evidence records which address a host used.
The switch is per template, on by default after the import, and the template page shows when a host
last called through it, which is how to tell when it is safe to turn off. Move images to the
template's own address when you can. The [migration
guide](migration.md#the-awx-compatible-callback-address) covers how the id is bound and what to
check before the cutover.

## Example

    - name: Site deploy
      hosts: all
      tasks:
        - name: Apply configuration
          ansible.builtin.template:
            src: app.conf.j2
            dest: "/etc/app/{{ region }}.conf"

Launched from a template with a `region` survey answer, every host paints its own cell in the
matrix as the task lands.

See also [Bash runs](tool-bash.md), [Terraform runs](tool-terraform.md),
[drift detection](drift.md), and the [tutorials](tutorials.md).
