# Inventories

An inventory is the list of hosts a run targets. SwitchTender keeps three kinds.

| Kind | Where its hosts come from |
|------|---------------------------|
| Static (the default) | Its own content: an INI or YAML inventory document, or the same document written as JSON, which is the form a dynamic inventory source refreshes into. The content can also be fetched at launch from a command, Vault, Google Secret Manager, AWS Secrets Manager, or Azure Key Vault. |
| Smart | A host filter over the hosts of the other inventories, evaluated at every launch. |
| Constructed | The Ansible `constructed` plugin run over a list of input inventories at every launch, building groups and variables from the hosts' own variables. |

Smart and constructed inventories hold no hosts of their own. They are composed from the others each
time a run launches, so they always reflect what their inputs hold now.

## Smart inventories

A smart inventory is a host filter. The syntax is the AWX `host_filter` syntax, so a filter copied
out of AWX means the same thing here.

    groups__name=web and not name__icontains=canary
    variables__env=prod or inventory__name="db fleet"
    ansible_facts__ansible_distribution=Ubuntu

A filter is a set of `key=value` terms joined by `and`, `or`, and `not`, grouped with parentheses.
Two terms side by side mean `and`. A value with a space in it is quoted.

| Key | Matches |
|-----|---------|
| `name` | The host's inventory name. |
| `groups__name` | A group the host is a direct member of in its inventory. |
| `inventory`, `inventory__name` | The id or name of the inventory the host comes from. |
| `variables__<key>` | A host variable, as Ansible resolves it with its group variables. Nested keys are joined with `__`, and `[]` reads a list, as in `variables__tags[]=edge`. A bare `variables__icontains=text` searches the host's variables as text. |
| `ansible_facts__<fact>` | A fact SwitchTender gathered for the host: distribution, distribution version, kernel, architecture, processor count, memory, default address, fully qualified name, Python version, service manager, and virtualization type. Write it with the `ansible_` prefix as AWX does, or without. A host never gathered does not match. |
| `enabled` | Every host. A host AWX had switched off is left out at import, so `enabled=true` keeps them all. |
| `search` | The host name, ignoring case. |

After the field, a lookup changes the comparison: `exact` (the default), `iexact`, `contains`,
`icontains`, `startswith`, `istartswith`, `endswith`, `iendswith`, `regex`, `iregex`, `gt`, `gte`,
`lt`, `lte`, `in` (a comma separated list), and `isnull`. A fact is compared exactly, as AWX
compares one. A key or lookup the filter cannot read is refused when the inventory is saved, rather
than read as matching nothing.

The filter runs over every static inventory the person launching may use. A smart inventory placed
in an organization reads only that organization's inventories, which is the reach an AWX smart
inventory has. A host name that appears in more than one inventory is taken once, from the first
inventory whose record matches.

### Groups stay behind

A smart inventory carries the hosts its filter matches and each host's variables. It carries none
of the groups those hosts belong to in their own inventories, which is how AWX builds a smart
inventory too. A play written for `hosts: all` reaches every host in it. A play written for a group,
such as `hosts: web`, reaches none of them, because the smart inventory has no group named web. The
editor and the host preview say the same.

When a play needs groups, use a constructed inventory instead. With no options it keeps every group
its inputs define, a limit narrows it to the hosts a pattern matches, and `groups` or
`keyed_groups` build new groups from the hosts' variables:

    keyed_groups:
      - key: role
        prefix: role

The constructed inventory section below covers the options.

## Constructed inventories

A constructed inventory names its input inventories in order and the plugin options AWX keeps as
`source_vars`.

    strict: false
    compose:
      resolved_state: state | default('running')
    groups:
      is_shutdown: resolved_state == 'shutdown'
    keyed_groups:
      - key: account_alias
        prefix: acct

A limit, such as `is_shutdown:&acct_product_dev`, is applied after the groups are built, so it may
name a group the options construct. The options accepted are the ones AWX documents for a
constructed inventory: `strict`, `compose`, `groups`, `keyed_groups`, `leading_separator`, and
`use_vars_plugins`. Any other option is refused, and so is an expression that calls a lookup,
because a lookup reads files or runs commands on the server and a constructed expression only needs
the hosts' variables.

A constructed inventory is evaluated by `ansible-inventory` with the same plugin AWX runs, so its
expressions behave exactly as they do there. SwitchTender never evaluates the Jinja in these options
itself. The server needs Ansible installed to resolve one, which the container image includes. A
later input's variables win over an earlier one's for the same host. An input must be a static
inventory, not another smart or constructed one, and it cannot be deleted while a constructed
inventory reads it.

## Which engine resolves an inventory

SwitchTender needs Ansible only for what is Ansible's to do. Everything else is resolved by a native
engine in the binary, which reads an inventory document exactly the way Ansible's own INI and YAML
plugins read it.

| Engine | What it resolves |
|--------|------------------|
| Native | A static inventory document stored here, INI, YAML, or JSON, and the host filter of a smart inventory over such documents. No Ansible is needed, so a smart inventory works on a server without it. |
| Ansible | A constructed inventory, always. An input whose document only Ansible can read: an inventory plugin configuration, a vault-encrypted value, or one of the rare Python values listed below. A dynamic inventory source, which runs its plugin. An inventory named by a path inside a project, whose `group_vars` and `host_vars` directories, vars plugins, inventory directories, and `ansible.cfg` are Ansible's. |

What an inventory is decides its engine, never whether Ansible happens to be installed, so the same
inventory resolves the same way on every server. When the engine is Ansible and Ansible is not
installed, the launch and the preview are refused with the reason it is needed and the one line that
installs it:

    pipx install ansible-core

Ansible is always run as a separate program, `ansible-inventory`, and never linked into the binary.
The container image ships it.

### What the native engine reproduces

The native engine reproduces Ansible's behavior, quirks included:

- A value on an INI host line is typed the way Python's `ast.literal_eval` types it, after the line
  is split the way `shlex` splits it: `port=22` is the number 22, `port="22"` is also the number 22
  because the quotes belong to the line, `on=yes` is the text yes, `flag=True` is true, and
  `list=[1, 2]` is a list. A `#` outside quotes ends the line, even in the middle of a word.
- A value under `[group:vars]` is everything after the first `=`, and is typed the same way.
  Ansible's documentation says these values are strings, but ansible-core types them like host
  values, so `5` is a number and `"5"` is text. The engine does what ansible-core does, and the
  corpus pins it.
- Host ranges: `web[01:03]` with its zero padding, `db-[a:c]`, strides such as `app[1:10:3]`,
  several ranges in one name, a range with a port, and a range whose start is after its end, which
  names no hosts.
- `host:port` sets `ansible_port`, for a name, an IPv4 address, or a bracketed IPv6 address.
- `[group:children]`, including children named before they are declared, nesting at any depth, and
  the error Ansible gives for a group that is never declared or for a loop.
- Which group variable wins: `all` first, then every other group ordered by depth, then by
  `ansible_group_priority`, then by name, and the host's own variable last.
- One host in many groups, with the variables of each. A host that shares its name with a group has
  its host-line variables set on the group, as Ansible does.
- YAML read with YAML 1.1 rules, as PyYAML reads it: `yes`, `no`, `on`, and `off` are booleans,
  `010` is octal, `1:20` is the number 80, `1e3` is text while `1.0e+3` is a number, and a date is
  a date, which the listing prints in ISO form. Anchors, aliases, merge keys, and `!unsafe` text are
  read the same way. A document whose aliases expand to far more values than its size could hold,
  a few short anchors nested so each copies the last many times over, is refused as invalid and
  names the bound. Neither PyYAML nor Ansible bounds that expansion, and both hang on such a
  document, so it is not handed to Ansible either. Replace the repeated anchors with explicit
  values.
- JSON is read before YAML, as Ansible reads it, with `{"__ansible_unsafe": ...}` values intact.

Variables Ansible sets itself when a play runs, such as `inventory_hostname` or `group_names`, are
left out of a resolved inventory, as `ansible-inventory --list` leaves them out.

A few values are left to Ansible rather than imitated, because Ansible releases disagree about them
or the JSON a composed inventory is rendered as cannot carry them. They are Python complex numbers,
sets, bytes, the ellipsis, named Unicode escapes, and lone surrogates, infinite floats, a mapping
key that is not text or a whole number, `!unsafe` on something other than text, and a host range
with a negative step. An inventory holding one is resolved by Ansible.

A document Ansible would not read whole is refused with the reason, such as an INI section of an
unknown type, a child group never declared, or the JSON `ansible-inventory --list` prints, which no
Ansible plugin reads back as an inventory. Store the output of `ansible-inventory --list --yaml`
instead.

### How the agreement is kept

A conformance corpus of inventory documents, each one a quirk listed above or an edge of one, runs
in CI through the native engine and through one pinned release of every ansible-core minor from
2.16 to 2.21, and any difference in hosts, groups, or variables fails the build. The Doctor page,
at `/ui/doctor`, and `GET /v1/doctor` report the server's ansible-core version and warn when it is
outside that range.

Every Ansible run against an inventory the native engine resolved is checked once more just before
the play starts. The executor has its own `ansible-inventory`, with the run's environment, the
project's `ansible.cfg`, and the run's container image when it has one, read every input the native
engine read and the inventory handed to the play, and compares each reading with the native one. On
any difference the run is refused, and its error and its evidence name each difference: the host,
the group, or the variable, with its value and type, except that a secret variable is named and its
value withheld. An executor without Ansible refuses such a run too, rather than running it
unchecked. A run with another tool, such as a script reading the inventory file, is not checked.

### What the evidence records

The resolution a run records names the engine that produced it, `native` or `ansible`, the
ansible-core version when Ansible did, a digest of the inputs, and a digest of the resolved
inventory: its hosts, groups, and variables. Both digests are SHA-256 over the secret-masked form,
the one the API serves, so they identify what was resolved without committing to a password. The
check before an Ansible run records the ansible-core that made it and the digests it read, which
match the resolution's unless an input changed while the run waited for approval. The resolution is
part of the approved spec, the check is part of the outcome record, and the dossier and receipt
carry both.

## Access

Composing an inventory never reaches a host its launcher could not reach directly. At launch, every
input the person launching may not use is dropped before anything is read, for a smart inventory
and a constructed one alike. Naming an inventory as a constructed inventory's input requires use of
it, the same as targeting it in a run. A run started by a schedule or a webhook has no person
behind it and reads every input in the inventory's scope, the same as it reads the rest of its
template.

## What a run records

A static inventory is read once, when the run is submitted, and the run executes that snapshot. The
snapshot is part of the run's spec: the hosts it names, a digest of its content with secret values
masked, and the digest of the content as sealed on the run. An approval covers all three, and an
edit to the inventory after the run was submitted, before or after an approval, does not reach it.
The credentials the inventory attached at submission come with it, and one attached later does not.
Because inventory content can carry an `ansible_password` or a token, the snapshot is sealed with
the server's key while the run waits and wiped when the run ends. A run whose snapshot is missing,
does not open, or no longer matches what was bound is refused rather than run against whatever the
inventory holds now. A source that refreshes on launch refreshes at submission, so the snapshot
holds the refreshed hosts. Content fetched from a command, Vault, or a secret manager is fetched at
submission too.

A dynamic source is different by nature: its definition, such as an inventory plugin configuration,
names no hosts until Ansible asks the live system it describes. The approval binds the definition,
and the hosts it resolves to are known only at execution. The run resolves it once, hands the play
exactly that resolution, and records the hosts it reached with the outcome, so the evidence names
them. The approval view says plainly that such a source resolves at execution.

A composed inventory is resolved once, when the run is launched. The run records which kind it
was, the inputs its hosts came from, and every host it resolved to, and execution is held to that
set: a host that joins an input afterward is not reached, and a shard, a pipeline step, or a retry
of failed shards reaches only hosts in it. A rerun is a new launch and resolves again. A launch that
resolves to no host is never started against nothing: a launch by a person is refused, and a
schedule or webhook fire is recorded as skipped, as [When nothing matches](#when-nothing-matches)
describes.

The resolved set is part of the run's spec, so the digest an approver's decision commits and the
digest the outcome entry commits both cover it, and a receipt discloses it. The run's evidence
dossier lists the hosts under its own heading. A rule scoped to an inventory governs a run whose
composed inventory drew hosts from that inventory, so a smart inventory over production hosts is
held by the rules written for production.

## When nothing matches

A launch by a person, from the interface, `POST /v1/templates/{id}/launch`, `POST /v1/runs`, or a
rerun, is refused with `400`, the reason, and a link to the inventory's host preview, since the
person launching can widen the filter or the inputs right away. The response carries the link as
`preview_url` as well, `/ui/inventories?preview=<inventory id>`, which opens that inventory's
preview.

A schedule or a webhook trigger has nobody to answer, and an inventory over hosts that come and go,
such as the hosts waiting for a patch, can match nothing on an ordinary day. So a fire that resolves
to no host is recorded as `skipped: no hosts matched` and never as a failure. Each skip is its own
entry on the audit chain. A webhook delivery is answered `200` with
`{"trigger": "<id>", "skipped": "no hosts matched"}`, so the sender does not deliver it again. A
schedule records the skip as its last fire, counts the skips in a row, and tells the notification
targets attached to it. The
[schedule tutorial](tutorial-schedule-a-job.md#when-a-fire-matches-no-hosts) shows where a skipping
schedule surfaces.

## Preview

The Inventories page previews the hosts a smart or constructed inventory resolves to before it is
saved, and a saved one can be previewed from its row. A preview resolves exactly as a launch by the
same person would, and lists host names and inputs without any host variables. It also names the
engine that resolved it, and the ansible-core version when Ansible did.

    POST /v1/inventories/preview          {"kind": "smart", "host_filter": "groups__name=web"}
    POST /v1/inventories/{id}/preview

## Creating one through the API

    POST /v1/inventories
    {"name": "web everywhere", "kind": "smart", "host_filter": "groups__name=web"}

    POST /v1/inventories
    {"name": "shut down", "kind": "constructed",
     "input_inventory_ids": ["inv_web", "inv_db"],
     "source_vars": "groups:\n  is_shutdown: state == 'shutdown'\n",
     "limit": "is_shutdown"}

## Importing from AWX

`switchtender import awx` and `switchtender assess awx` read smart and constructed inventories.
A smart inventory arrives with its filter. A constructed inventory arrives with its inputs wired by
id, whether the export lists them on the inventory or under `related`, and with the options and
limit of the constructed source AWX keeps behind it. A filter or option this cannot evaluate is
reported and the inventory is not imported, rather than created to fail at its first launch. The
assessment names every composed inventory that comes across.

An AWX smart inventory filters the inventories of its own organization, so the import places it the
same way. When the export carries the smart inventory's organization, the smart inventory and every
inventory imported from that organization, dynamic source inventories included, are placed in an
organization of the same name, and its filter reads those inventories and no others. Applying the
import creates the organization, or uses the one already here when exactly one has that name, in
which case the filter also reads the inventories that organization held before the import. The
plan and the import result name the organization each placed inventory lands in, and the assessment
names it beside each smart inventory. AWX memberships do not come across, so add the organization's
members afterward.

A smart inventory arrives with no organization, and the report says so, when the export names its
organization without carrying it, or when more than one organization here has that name and the
import cannot tell which one the export means. With no organization it filters every inventory the
person launching may use, so place it and its inventories in one organization to keep the reach it
had in AWX. Either way it arrives without the groups its hosts had, as described above.

## What is not carried over

- A constructed inventory's `update_cache_timeout` and `verbosity`. It is resolved at every launch,
  so there is no cache to time out.
- AWX host fields with no counterpart here, such as `description` and `instance_id`, in a filter.
- Facts beyond the set SwitchTender gathers, and nested fact paths other than the default address.
- The credentials and queue of an input inventory. A composed inventory carries its own.
- Refreshing an input's dynamic source before a composed launch. Each input is read as its source
  last left it.
