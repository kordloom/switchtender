<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Approval policies in YAML and Rego

An approval policy decides what happens to a run before it executes: it runs, it waits for a person,
or it is refused. Policies live in one file named by `--policy-file`, held in git, so a change to
what needs approval is a reviewed diff. [Concepts](concepts.md#policy-as-code) covers why the file is
the source of truth and how a change deploys.

The file holds two kinds of policy. YAML rules match on criteria. Rego policies are written in the
Open Policy Agent language and decide in code, so an existing OPA or Conftest rule ports without
being rewritten as criteria. Both are loaded from the same file, reloaded on the same edit, enforced
at the same points, and recorded in the same evidence.

## Agent runs are held by default

A run an AI agent asked for waits for a person before it executes, with no policy written first.
The hold is built in. It is named `requested by an agent, held by default` on the held run, in the
approval queue, and in the run's dossier, and the outcome the audit chain commits carries the same
words as a note, so a receipt for the run says why it waited. A person's run is unaffected: the
built-in hold looks only at who asked, and the rules below decide a person's run exactly as before.

Who asked is read from the token, never guessed from the request. The hold applies to agent
tokens, the ones minted with `switchtender token new --user <account> --agent`. A token minted
without `--agent` is recorded as a person's and gets no agent hold, so mint every agent's token
with it. A run counts as an agent's when an agent token submitted it, and when it derives from an
agent's request: the apply an agent's plan proposes, a shard of an agent's split, a step of its
workflow, and a retry, a rerun, or a relaunch of failed shards the agent asks for. The derived run
carries the agent's identity, so it faces the same hold. The receipt for the run proves who
approved it or which rule exempted it.

A dry run waits like any other run an agent asks for. Check mode and a plan are not inert: Ansible
runs lookups, vars files, and plugins on the controller under `--check`, and a Terraform or OpenTofu
plan runs provider code and data sources, all with this server's credentials, so a preview can read
a secret and send it anywhere, even with a request that only reads. Whether a dry run changes
anything is the wrong question for an agent, and no scan can prove a plan safe. The gate still scans
the dry run and records what it read on the run, as evidence, and the hold note says why the run
waits: what the agent asked for runs code with this server's credentials, so it waits for a person
or for an exemption that covers it. A person's dry run is decided exactly as before, under
`exclude_dry_run` and every other rule.

An agent's Terraform or OpenTofu apply takes two approvals. It is held where it was submitted,
before anything plans, since planning runs provider code with this server's credentials. When a
person releases it, it is planned, and the apply its plan proposes is held again, carrying the saved
plan, so the second approval binds the exact plan that applies. A released request is never applied
without that plan, whatever other rules are in force: a `max_destroy` limit, a risk or
reversibility floor, or a Rego `plan_gate`. A rerun, a retry, or a relaunch the agent asks for takes
the same path. An apply an exemption covers plans and applies as the rules allow for a person's.

An agent cannot carry an apply inside a workflow. A workflow's approval binds the workflow as
submitted and never shows the plan a step applies, so an agent's workflow, or a saved workflow an
agent launches, is refused at submission with a 403 when it holds a Terraform or OpenTofu step that
is not a dry run, unless an exemption covers that step. The step is judged as the run it would
become, under the agent's label and account. The refusal names the step, is recorded on the chain
under the name `an agent's workflow may not apply Terraform or OpenTofu`, and says what to do
instead: ask for the apply as its own run, which plans first and waits for approval of the saved
plan, or write an exemption that covers the step. An exemption that covers the step lifts the
refusal and nothing more: the workflow is still held for a person as a whole, unless an exemption
also covers the workflow run itself. This holds however the workflow arrives: submitted
whole, launched from a saved workflow, or launched through an MCP tool. A finished workflow is never
rerun as such, by anybody, only launched again from its saved workflow, which faces the same check.
An agent's workflow whose infrastructure steps are all plans, or that has none, is held as a whole as
before. A person's workflow is unchanged.

SwitchTender knows an agent by its agent token, and only by that. A caller that signs in with a
federated JWT is treated as the person or pipeline its claims map to: it is recorded as a session,
with the role its group claim maps to, and gets no agent hold. An AI agent that reaches SwitchTender
through a workload JWT is therefore not held by default, and if its claims map to admin, it can
approve. Give every AI agent an agent token, and never a JWT mapped to a role that can approve.

The built-in hold sits beside the stored rules and replaces none of them. A deny rule still refuses
an agent's run outright, and a stored rule that holds the run is the one the hold names, with its
distinct-approver and reason requirements. Releasing an agent's run works as it always did: a
person with the admin role approves it, and an agent never can.

### Exempting an agent's routine work

An exemption is a stored rule with `effect: exempt`. The agent runs it matches go ahead without the
built-in hold:

    policies:
      - name: release-agent-smoke-tests
        effect: exempt
        actor: release-agent
        account: release-bot-owner
        tool: bash
        queue: staging
        command_contains: ./smoke.sh

An exemption matches on `tool`, `command_contains`, `inventory_id`, `queue`, `actor`, and
`account`, and `actor_kind` may be left out or set to `agent`. `account` is the username of the
account the agent's token is bound to, the one named by `--user` when the token was minted. An
exemption that names `actor` must also name `account`, and one that does not is refused with a 400
through the API and fails to load from the policy file: an `actor` is a token's label, chosen by
whoever mints the token and not unique across accounts, so the label alone would also exempt a
token minted for another account under the same name. An exemption may name `account` alone, which
covers every agent token bound to that account. It is refused with `min_risk`, `reversibility`,
`exclude_dry_run`, `require_distinct_approver`, `require_reason`, or `max_destroy`, which belong on
a rule that holds. It lifts the built-in hold and nothing else: a stored rule that holds or refuses
the same run still does.

The account travels with every run derived from the agent's request: the apply its plan proposes,
the shards of its split, the steps of its workflow, and a retry, a rerun, or a relaunch it asks
for. A run whose account cannot be read is never exempt.

An exemption is a rule like any other. `GET /v1/policies` lists it, account included. The rule set
every run records as in force covers it and describes it as "lets an agent's run proceed without the
default hold, for agents bound to account "release-bot-owner"". A run it lets through records the
note `requested by an agent bound to account "release-bot-owner", exempt from the default hold by
policy "release-agent-smoke-tests"` in its outcome, so the receipt and the dossier name the account
and the exemption. Write one through the API or the policy file, the same as any rule.

The built-in hold is not a stored policy, so it counts against no license's policy limit and cannot
be deleted. An exemption is a Community rule, one that names an `actor` and an `account` included,
and it counts as one policy, so a Community install's one policy can be the exemption.

What an exemption risks is everything it matches, which then runs with no person looking. Keep it
narrow.

- `command_contains` matches text anywhere in the command, ignoring case, so an exemption for
  `smoke` also matches `smoke; rm -rf /`. Pair it with `tool`, `queue`, or `inventory_id`.
- An admin can mint a token for any account, so the exemption trusts whoever can mint tokens. Bind
  each agent to an account of its own, and an exemption naming that account covers that agent
  alone.
- An exemption with no criteria exempts every agent run, which turns the default off.

Prefer a named `actor` and its `account` together with a queue or an inventory that reaches only
what the agent's routine work needs.

### Nothing an agent writes runs later as someone else

An agent's token is capped at the operator role, and creating or editing a schedule, a trigger and
its webhook, a template, a workflow, an inventory, a project, a credential, or a policy is admin
work. A manage grant does not open any of it to an agent either, since a grant is delegation between
people. So nothing an agent writes fires a run later under a schedule's or a webhook's name.

Everything an agent can launch faces the hold under the agent's own name: a run, a split, a
workflow, a template launch, a drift reconcile, a rerun, a retry, a relaunch of failed shards, a
tool call through MCP, and the apply a plan proposes, on the control node or on a relay worker. A
pull request comment acts for the person whose forge account is linked, and an agent cannot link
one.

### Upgrading

An existing install gets the built-in hold when it upgrades. There is no migration and no setting.
An agent whose runs went ahead unattended before the upgrade now waits for a person, its dry runs
included, and its Terraform or OpenTofu applies take two approvals. Write an exemption for the
routine work that should keep running unattended, and leave everything else held.

## YAML rules

    policies:
      - name: prod-terraform-destroy
        tool: terraform
        command_contains: destroy
      - name: large-teardown
        tool: opentofu
        max_destroy: 5
      - name: agents-never-drop-databases
        actor_kind: agent
        command_contains: drop database
        effect: deny

A rule matches on `tool`, `command_contains` (ignoring case), `inventory_id`, `queue`, `actor_kind`,
`actor`, `account`, `min_risk`, `reversibility`, and `exclude_dry_run`. `account` is the username of
the account the requesting credential is bound to: the person behind a token or a session, or the
account an agent's token acts for. A run whose account cannot be read is matched by a rule that
holds or refuses, and never by an exemption. A match holds the run, refuses it with
`effect: deny`, or, with `effect: exempt`, lets an agent's run go ahead without the
[built-in hold](#agent-runs-are-held-by-default). `require_distinct_approver` makes the release need
someone other than the requester, and `max_destroy` plans a Terraform or OpenTofu apply first and
holds it only when the plan destroys more than that many resources.

The count is read from the plan file the plan saved, and the apply that follows carries out that
saved plan, so the plan an approver releases is the plan that runs. If the infrastructure changed
since the plan was made, the tool refuses the stale plan rather than applying a different one, and the
apply has to be proposed again. The
[Terraform page](tool-terraform.md#a-gated-apply-carries-out-the-approved-plan) has the details.

A rule with no `max_destroy` that would hold a Terraform or OpenTofu apply plans it first as well.
The apply is planned, the apply the plan proposes carries the saved plan, and the rules decide on that
proposal, so the rule holds the planned apply and the approval binds the plan that runs rather than a
request that would plan again when released. An apply whose own submission asks for approval is
planned first as well, and the apply its plan proposes is held whether or not a rule covers it.

### What a workflow's approval binds

A workflow is approved as a whole, and the approval binds the workflow as it was submitted. The
decision commits a digest of its spec: every step as written, with its tool, its command or playbook,
its inventory, whether it is a dry run, and the steps it depends on, along with the workflow's
variables, the digests of its secret answers, its credentials, its image, and the inventory snapshot
it was submitted with. A workflow held while it reads from a project is pinned to the commit its
branch pointed at when it was held, and every step runs that commit.

What a step works out only when it runs is not part of that approval. A Terraform or OpenTofu apply
step plans and applies when the step runs, so the plan it applies is never shown to the person who
approves the workflow. To approve an exact plan, run the apply on its own, held for approval by a
rule or by asking for approval when it is submitted: it plans first, and the approval binds the saved
plan. An agent's workflow cannot carry an apply step at all, as
[Agent runs are held by default](#agent-runs-are-held-by-default) describes.

### Requiring the approver's reason

`require_reason` makes a decision on a run the rule holds carry the approver's stated reason.
`denials` requires one to deny, and `always` requires one to approve or to deny. Left out, a reason
is optional.

    policies:
      - name: prod-terraform-destroy
        tool: terraform
        command_contains: destroy
        require_reason: always
      - name: hold-everything
        require_reason: denials

When several rules cover one run, the strictest requirement applies. It is copied onto the run when
the rule holds it, the way `require_distinct_approver` is, so editing the rule later cannot loosen a
decision already waiting. A workflow's approval steps carry the requirement of the rules covering the
workflow and its steps. A decision without a required reason is refused with the rule named, and
nothing is recorded. The reason itself is masked, stored, and committed as described in
[Approver reasons](api.md#approver-reasons).

## Rego policies

A Rego policy is listed under `rego:` in the same file. Its modules are read from paths relative to
the policy file, so the YAML and the Rego it loads change together in one diff.

    policies:
      - name: prod-terraform-destroy
        tool: terraform
        command_contains: destroy
    rego:
      - name: guardrails
        files: [rego/guardrails.rego, rego/lib.rego]
        # package: data.switchtender   # optional, defaults to the first file's package
        # syntax: v0                   # optional, for modules written before OPA 1.0
        # warn: note                   # optional, record warnings instead of holding
        # timeout: 2s                  # optional, 500ms unless set, at most 10s

| Key | Meaning |
|-----------|---------------------------------------------------------------------------------|
| `name` | Names the policy in holds, refusals, and the evidence. Unique across the file. |
| `files` | One or more modules, relative to the policy file. Helper packages may sit beside the decision package and be imported from it. |
| `package` | The package whose rules decide. Defaults to the package the first file declares. |
| `syntax` | `v1`, the default, or `v0` for the `deny[msg] { ... }` syntax most Conftest policies are written in. |
| `warn` | What the package's `warn` rule does: `hold`, the default, holds the run for approval, and `note` records the warning on the run and lets it go ahead. See [Warnings: hold or note](#warnings-hold-or-note). |
| `timeout` | How long one evaluation may run, as a duration with a unit such as `750ms` or `2s`. `500ms` unless set, at most `10s`. See [Evaluation time limit](#evaluation-time-limit). |
| `require_reason` | `denials` or `always`, the same setting a YAML rule takes. It applies to every run this policy holds. |

Editing a module takes effect without a restart, exactly as editing the YAML does.

### What a policy decides

The decision package may define any of these rules. Every other rule in it is a helper.

| Rule | Shape | Effect |
|------------------------------|---------------------------------------|------------------------------------------------------------|
| `deny` | set of messages, or boolean | Any message refuses the submission. The run is never created. |
| `allow` | boolean | When the package defines it, anything it does not allow is refused. |
| `hold` | set of messages, or boolean | Any message holds the run for approval. |
| `warn` | set of messages, or boolean | Read as `hold` by default, since a warning asks a person to look and here a person looks by approving. Under `warn: note` it is recorded on the run instead, and the run goes ahead. |
| `require_distinct_approver` | boolean | Whoever releases the run cannot be whoever asked for it. |
| `plan_gate` | boolean | A Terraform or OpenTofu apply is planned first, and the apply the plan proposes is judged again with `input.plan` filled in. |

A message is a string, or a Conftest result object, whose `msg` field is used. A package must define
at least one of `deny`, `allow`, `hold`, or `warn`, or it is refused at load.

The decisions map onto the YAML model one for one. A `deny` behaves as a rule with `effect: deny`, a
`hold` as a plain rule, `require_distinct_approver` as the YAML flag of the same name, and `plan_gate`
with a hold on `input.plan.destroys` as `max_destroy`. Every deny is checked across the whole file
before any hold, so a refusal in Rego is not satisfied by a hold in YAML, or the reverse.

    package switchtender

    import data.lib

    deny contains "agents may not touch production" if {
        input.actor.kind == "agent"
        lib.prod
    }

    hold contains "irreversible change" if input.reversibility.class == "irreversible"

    require_distinct_approver if input.reversibility.class == "irreversible"

    plan_gate if input.run.tool in {"terraform", "opentofu"}

    hold contains msg if {
        input.plan.planned
        input.plan.destroys > 5
        msg := sprintf("plan destroys %d", [input.plan.destroys])
    }

### Warnings: hold or note

A `warn` rule asks a person to look. By default a warning holds the run for approval, the same as
`hold`. A policy that sets `warn: note` records its warnings instead of waiting on them.

| `warn` | What a warning does |
|------------------|--------------------------------------------------------------------------|
| `hold` (default) | The run waits for approval. The hold names the policy and the warning. |
| `note` | The run goes ahead. The warning is recorded on the run as a note: the run page lists it, the runs list marks the run as noted, the dossier includes it, and the outcome the audit chain commits carries it, so a receipt for the run shows it. |

A note names the policy, its messages, and its bundle the way a hold does, for example
`staging-advice (no change ticket on the run, rego sha256:3f9c0a1b2c4d)`. It carries at most ten
messages and says how many more there were. Before a note or a hold is recorded, values the run's
variables and script hold under secret-looking names are masked in it, and so is any
secret-looking assignment it quotes, since a module can build a message from `input.run.command`.

The setting covers `warn` and nothing else.

- A `deny` rule in the same package still refuses, and a `hold` rule still holds. A run one of them
  holds still carries the notes, so the approver sees them.
- A policy that cannot decide, because it errors or runs past its timeout, refuses the submission
  under either setting. Deciding nothing is not a warning.
- Setting `warn: note` on a package with no `warn` rule is refused at load. It would read as
  softening the rules that are there while changing nothing.

A dry run the scan did not find change free is judged twice, as [described
below](#dry-runs-that-are-not-change-free), and a policy that notes records what either pass warned
about.

#### Per project: staging notes, production holds

The setting belongs to a policy entry, so two projects get different treatment from two entries. The
entries below share one set of checks. One decides only about the staging project and notes, the
other decides only about production and holds:

    rego:
      - name: staging-advice
        files: [rego/staging.rego, rego/checks.rego]
        warn: note
      - name: production-advice
        files: [rego/production.rego, rego/checks.rego]

`rego/checks.rego` writes each check once:

    package checks

    findings contains "no change ticket on the run" if not input.run.labels.ticket

    findings contains "an agent asked for this run" if input.actor.kind == "agent"

`rego/staging.rego` turns them into warnings for the staging project:

    package staging

    import data.checks

    warn contains msg if {
        input.run.project_id == "proj_5d0e1a2b3c4f"
        some msg in checks.findings
    }

`rego/production.rego` does the same for production, and its entry leaves `warn` at the default, so
each warning holds the run:

    package production

    import data.checks

    warn contains msg if {
        input.run.project_id == "proj_9a8b7c6d5e4f"
        some msg in checks.findings
    }

A person's staging run without a ticket label goes ahead carrying the note
`staging-advice (no change ticket on the run, ...)`. The same run in production waits for approval,
held by `production-advice`. A run in any other project is decided by neither entry.
`GET /v1/projects` lists each project's id.
An agent's run is [held by default](#agent-runs-are-held-by-default) whatever these entries decide,
unless a rule with `effect: exempt` covers it, and it still carries the staging note, so the
approver sees it.

The same shape scopes a warning to one job instead of a project. A rule reads the playbook the job
runs, `input.run.playbook`, or `input.run.template_id`, which names the template however the run was
fired, a schedule and a trigger included. `input.run.source` and `input.run.source_id` say what fired
it: the template when it is launched directly and the schedule when a schedule fires it.

#### Making a rule blocking or silent

Where a finding lands depends on the rule that emits it and the entry around it:

| To make a finding | Emit it from |
|------------------------------------------------------|----------------------------------------------|
| Refuse the submission, so the run is never created | `deny` |
| Hold the run until someone approves it | `hold`, or `warn` in an entry that leaves `warn` at `hold` |
| Let the run go ahead and keep a record of it | `warn` in an entry that sets `warn: note` |
| Say nothing at all | none of `deny`, `hold`, or `warn` |

To make a finding fully blocking in production, emit it from `deny` in `rego/production.rego`. The
submission is then refused with the finding as the reason, and no approver can release it:

    package production

    import data.checks

    deny contains msg if {
        input.run.project_id == "proj_9a8b7c6d5e4f"
        some msg in checks.findings
    }

To silence one finding for staging, leave it out of the rule that reads the checks. The other finding
is still noted:

    package staging

    import data.checks

    warn contains msg if {
        input.run.project_id == "proj_5d0e1a2b3c4f"
        some msg in checks.findings
        msg != "an agent asked for this run"
    }

To silence a whole entry, remove it from the policy file. Emptying its package instead does not
work: a package that defines none of `deny`, `allow`, `hold`, or `warn` is refused at load, the same
as any other policy that would decide nothing.

### The input document

Every policy is evaluated against the same document, version 1. Every field is always present, with
an empty value when the run has none, so a module never meets a missing field.

| Field | Meaning |
|------------------------------|-------------------------------------------------------------------|
| `version` | `1`. It changes only if a field is removed or changes meaning. |
| `run.id` | The run's id. |
| `run.tool` | `ansible`, `bash`, `terraform`, `opentofu`, `python`, `powershell`, or `go`. Never empty. |
| `run.command` | The script, or the working directory for Terraform and OpenTofu. |
| `run.playbook` | The Ansible playbook path. |
| `run.inventory.id` | The stored inventory the run targets, empty for a path. |
| `run.inventory.path` | The inventory path the run targets. |
| `run.limit` | The host pattern the run is narrowed to. |
| `run.queue` | The worker queue the run is routed to. |
| `run.project_id` | The git project the run reads from. |
| `run.dry_run` | Whether the run was asked for as a no-change preview: the tool's dry-run flag, `--check` for Ansible. |
| `run.dry_run_findings` | For a dry run, why the gate's scan did not find it change free. For Ansible, each play, block, task, role, or include that sets `check_mode` to anything but true, and each play or task that runs a `pipe` lookup, naming the file. For Terraform and OpenTofu, each `external` or `aws_lambda_invocation` data source, and each `http` data source with a write method or a request body, by its address, naming the file and line. For both, whatever the scan could not read. Empty for a dry run read in full that found none of these, for a dry run of a tool nothing scans, and for any other run. |
| `run.change_free` | Whether the gate reads the run as a dry run that changes nothing: `dry_run` is true, `dry_run_findings` is empty, and the tool is one the gate scans or a built-in one whose dry run runs nothing. A dry run of a tool a plugin or the SDK added is never change free. |
| `run.kind` | Empty for a plain run, or `split` or `pipeline` for a coordinator. |
| `run.step_name` | The pipeline step's name, for a step. |
| `run.source` | What fired the run: `api`, `template`, `schedule`, `trigger`, `callback`, `rerun`, `relaunch`, `reconcile`, `propose`, `review` for a pull request plan, or `review_apply` for an apply a pull request comment proposed. |
| `run.source_id` | The template or schedule behind `run.source`, the origin run of a rerun, or the plan run a comment's apply was proposed from. |
| `run.template_id` | The job template the run executes, however it was fired: launched directly, by a schedule, by a trigger, or by a callback, and carried to a rerun, a retry, a shard, and a pipeline step. Empty for a run no template launched. |
| `run.proposed_from` | The plan run an apply was proposed from, empty otherwise. |
| `run.intent` | The plain-language request an AI proposal was built from. |
| `run.image` | The container image named on the run. |
| `run.tags`, `run.skip_tags` | The Ansible tags selected and skipped. |
| `run.labels` | The run's labels, for example `run.labels.env`. This is where the environment usually lives. |
| `run.extra_var_names` | The names of the extra variables, sorted. Values are withheld, since they are where secrets ride. |
| `run.credential_ids` | The stored credentials the run executes with, by id. |
| `actor.name` | The token label or username that fired the run. For an agent, its token's label. |
| `actor.type` | How it authenticated: `agent`, `session`, `token`, `cli`, `forge_comment` for a person commenting from the forge account linked to their SwitchTender account, `webhook`, or `host` for a provisioning callback. |
| `actor.kind` | `agent`, `human` for a session, a person's token, the command line, or a forge comment, or `other` for a webhook, a schedule, a callback, or an unknown source. |
| `actor.account_id` | The id of the account behind the credential. For an agent, the account it acts for. A YAML rule's `account` matches the account's username instead. |
| `plan.planned` | Whether a plan has been read for this apply. |
| `plan.destroys` | How many resources the plan destroys, or null when nothing was planned. |
| `risk.level` | `low`, `medium`, or `high`, the same grade `min_risk` reads. |
| `risk.reasons` | Why the run graded that way. |
| `reversibility.class` | `reversible`, `costly`, or `irreversible`, the same grade `reversibility` reads, including what a playbook's content shows. |
| `reversibility.reasons` | Why the run graded that way. |
| `time.submitted_at` | When the run was submitted, RFC 3339 in UTC. |
| `time.submitted_at_ns` | The same instant in Unix nanoseconds, for `time.*` builtins. |

A reference to a field outside this document, such as `input.run.toool`, is refused at load. In
Rego a missing field is undefined and an undefined rule never fires, so the typo would otherwise load
cleanly and refuse nothing. The check covers references spelled from `input`. A module that copies
`input` into a variable is checked only as far as the copy.

A policy that reads `input.plan` must define `plan_gate`, or it is refused at load, since without
it no apply is planned and the count it reads would never exist.

### Failing closed

A Rego policy that cannot decide is a refusal, never a pass, the same rule the YAML engine follows
for a policy set it cannot read.

| When | What happens |
|--------------------------------------------|--------------------------------------------------------|
| A module does not parse or compile | The file does not load. A server refuses to start, and a running one refuses every submission until the file is fixed. |
| A module calls a disabled builtin | The same, since a call to an undefined function does not compile. |
| A reference names a field the input lacks | The same. |
| Evaluation errors, including a builtin error, which evaluation surfaces rather than hides | The submission is refused, naming the policy, the error, and the bundle. |
| Evaluation runs past the policy's `timeout`, 500 milliseconds unless it sets one | The same, naming the limit. |
| A decision rule has the wrong shape, such as `deny := 5` | The same. |
| `allow` is defined and does not evaluate to true | The submission is refused as not allowed. |

A policy that cannot decide also demands a distinct approver and plans any apply, so no question the
dispatcher asks it is answered in the permissive direction.

### Evaluation time limit

Each evaluation of a Rego policy has a time limit, 500 milliseconds unless the policy's entry sets
`timeout`:

    rego:
      - name: inventory-checks
        files: [rego/inventory.rego]
        timeout: 2s

The value is a duration with a unit, such as `750ms`, `2s`, or `1.5s`, and may be at most `10s`. A
value over `10s`, a value of zero or less, and a number without a unit are refused at load, the
same as a module that does not compile. A bare `500` is refused rather than guessed at, since it
could mean milliseconds or seconds.

A policy that runs past its limit has not decided, so the submission is refused, and the refusal
names the policy, the limit, and the bundle:

    inventory-checks (rego policy: evaluation exceeded its 2s timeout, which the policy's timeout setting can raise to at most 10s, rego sha256:3f9c0a1b2c4d)

The limit applies to each evaluation, and one submission can evaluate a policy several times. The
gate asks it separately whether to refuse, whether to hold, what to note, whether a second approver
is needed, and whether to plan an apply first, and a dry run the scan did not find change free is
evaluated twice for each. A person is waiting on every one of them, so a policy that needs more than
the default is worth making cheaper before its limit is raised.

Changing `timeout` changes which evaluations refuse, so it changes the rule set digest every later
run records, and a timeout other than the default is named in the rule's description in the
evidence.

### Offline and deterministic

Evaluation never leaves the process. Every builtin OPA marks nondeterministic is removed, and so is
anything that reaches outside the input: `http.send`, `net.lookup_ip_addr`, `opa.runtime`,
`time.now_ns`, `rand.intn`, `uuid.rfc4122`, `io.jwt.decode_verify`, the certificate chain
verifiers, `providers.aws.sign_req`, and `trace`. The network allow list is empty. Pure builtins,
including `net.cidr_contains` and the rest of the `net.cidr_*` family, stay available. A module that
needs the time reads `input.time`, so the same run gives the same answer on every evaluation and on
every server.

### Evidence

Each Rego policy is identified by a bundle digest: the SHA-256 over its package, its syntax, and
every module's path and text. The digest is in four places.

- A held run records the policy, its reasons, and the first twelve characters of the digest, for
  example `guardrails (irreversible change, rego sha256:3f9c0a1b2c4d)`.
- A run a policy noted records each note, named the same way, in `policy_notes`. The notes are
  part of the run's outcome entry in the audit chain, so a receipt for the run carries them and
  `switchtender verify` prints each one as a policy note.
- A refused submission names the same in its error, and the audit chain records the refusal as a
  decision entry by `system:policy` on behalf of whoever asked, at
  `/runs/<id>/decision/refused/rego/sha256:<64 hex>/policy/<name>`. The run is never created, so this
  entry is what shows afterward which bundle turned the submission away. A refusal by a YAML deny
  rule is recorded the same way, at `/runs/<id>/decision/refused/policy/<name>`.
- Every run records the rule set in force at submission, and a Rego policy appears in it as
  `guardrails: decided by Rego package data.switchtender, bundle sha256:<64 hex>`. That record is
  part of the run's outcome entry in the audit chain, so a receipt for the run proves which exact
  bundle was in force, and an auditor can check out the commit, hash the modules, and compare. A
  policy that sets `warn: note`, or a `timeout` other than the default, is described with the
  setting after the digest, for example `staging-advice: decided by Rego package data.staging,
  bundle sha256:<64 hex>, warnings noted without holding, timeout 2s`.

Any edit to a module changes the digest, and so changes the rule set digest every later run carries.
Changing `warn` or `timeout` leaves the bundle digest as it is, since no module changed, and changes
the rule set digest, since the same bundle now does something different to a run. Renaming the
policy changes neither.

### Workers

A relay worker reads the control node's policies, Rego bundles included, and compiles each one
again on arrival, refusing one whose modules do not produce the digest they arrived under. A worker
that cannot compile a bundle fails closed exactly as one that cannot reach the control node does.
Each bundle arrives with its `warn` and `timeout` settings, and a setting the worker cannot honor
fails the same way, so the worker's copy of a policy decides exactly as the control node's does.

### License

A Rego policy is part of the full policy engine, which Team covers, the same tier as deny rules,
risk floors, actor scoping, and separation of duties. A Community or Pro install whose policy file
lists a Rego policy refuses to load it and names the policy and the tier, the same refusal it gives
a deny rule. A lapsed Team license keeps serving the policies it was serving. Each Rego policy counts
as one policy toward a tier's policy count.

Rego policies are read only from the policy file. The API does not create them, and a backup
carrying one is refused at restore.

### Dry runs that are not change free

A dry run is a promise about the tool, not about what it is given. An Ansible dry run is
`ansible-playbook --check`, and a playbook can set `check_mode: false` on a play, block, task, role,
or include to run that work for real anyway, and a `pipe` lookup runs its command on the controller
whenever a template renders, under `--check` too. A Terraform or OpenTofu dry run is `plan`, and a
plan reads every data source with the run's credentials: an `external` data source runs the program
it names, an `aws_lambda_invocation` data source invokes its function, and an `http` data source
sends its request. The gate reads the playbook, or the configuration and every module it calls,
before it judges a dry run, and records what it found in `run.dry_run_findings`:

- Each task that forces real work with `check_mode`, by its file.
- Each `pipe` lookup in a play's `vars` or `environment` or anywhere in a task, written as `lookup`,
  `query`, or `q`, with or without the `ansible.builtin.` prefix, or as a `with_pipe` loop, by its
  play or task and file.
- Each `external` or `aws_lambda_invocation` data source, and each `http` data source whose method
  is anything but GET or HEAD or that carries a `request_body`, by its address, file, and line. A
  method the scan cannot read as a plain value counts as a write.

Input the scan could not read is recorded there too, because unread input can act for real as well:
a role that is not in the project, a registry module the gate could not download, a module source
only known at run time, or a file that does not parse. A dry run of a tool a plugin or the SDK added
is never change free, since the plugin runs its dry run as it was coded and nothing scans it.

The scan finds the known ways a check or a plan acts. It does not prove a preview harmless: other
lookups and plugins run code on the controller under `--check`, a module that claims to support
check mode can act anyway, and a plan runs provider code, all with the run's credentials. A rule
that exempts dry runs trusts the playbooks and configurations it lets through, so keep it to code
your team reviews. An agent's dry run is held by the [built-in
hold](#agent-runs-are-held-by-default) whatever the scan finds, unless an exemption covers it, and
the scan is recorded on it as evidence.

A dry run that is not change free is evaluated twice: once with the input as it is, and once as the
real run it may be, with `run.dry_run` false. The stricter answer stands: every deny, every hold, a
demand for a distinct approver, and a plan gate from either pass. A module that exempts previews by
reading `run.dry_run`, the way a YAML rule sets `exclude_dry_run`, therefore holds such a dry run
exactly where the YAML rule does, and lets a change-free dry run through exactly where the YAML rule
does. A module that wants to say so directly reads `run.change_free`:

    hold contains "a change in production waits for a person" if {
        input.run.labels.env == "prod"
        not input.run.change_free
    }

When the scan is the only reason a rule holds a dry run, the run carries a hold note naming what
was found and the two clean fixes: rework it so the dry run is safe, or drop the exemption from the
rule. [Dry runs and `exclude_dry_run`](concepts.md#dry-runs-and-exclude-dry-run) walks through the
common cases.

### Testing a policy locally

The input is plain JSON, so OPA's own tools test a module before it is merged. Save a sample run as
`input.json`:

    {
      "version": 1,
      "run": {
        "id": "run_sample", "tool": "terraform", "command": "infra/prod", "playbook": "",
        "inventory": {"id": "", "path": ""}, "limit": "", "queue": "prod", "project_id": "",
        "dry_run": false, "dry_run_findings": [], "change_free": false, "kind": "",
        "step_name": "", "source": "api", "source_id": "", "template_id": "",
        "proposed_from": "", "intent": "", "image": "", "tags": [], "skip_tags": [],
        "labels": {"env": "prod"}, "extra_var_names": [], "credential_ids": []
      },
      "actor": {"name": "deploy-bot", "type": "agent", "kind": "agent", "account_id": "usr_1"},
      "plan": {"planned": false, "destroys": null},
      "risk": {"level": "high", "reasons": []},
      "reversibility": {"class": "costly", "reasons": []},
      "time": {"submitted_at": "2026-10-01T12:00:00Z", "submitted_at_ns": 1790856000000000000}
    }

Then evaluate the decision package:

    opa eval --format pretty -d rego/ -i input.json data.switchtender

Conftest runs the same modules with `conftest test --namespace switchtender -p rego/ input.json`,
reading `deny` and `warn`. The answer `opa eval` gives is the answer the server gives, with one
difference to keep in mind: the server refuses a module that calls a disabled builtin or reads a
field outside the document, and `opa eval` checks neither.

## Moving from Sentinel

Sentinel is a proprietary language with no open implementation, so a Sentinel policy cannot be
loaded. Its rules translate directly into Rego. A Terraform Cloud policy that refuses applies
destroying more than five resources outside a change window:

    import "tfplan/v2" as tfplan
    import "time"

    destroys = filter tfplan.resource_changes as _, rc {
        rc.change.actions contains "delete"
    }

    in_window = time.now.weekday_name in ["Saturday", "Sunday"]

    main = rule {
        length(destroys) <= 5 or in_window
    }

becomes:

    package switchtender

    plan_gate if input.run.tool in {"terraform", "opentofu"}

    weekend if {
        day := time.weekday(time.parse_rfc3339_ns(input.time.submitted_at))
        day in {"Saturday", "Sunday"}
    }

    deny contains msg if {
        input.plan.planned
        input.plan.destroys > 5
        not weekend
        msg := sprintf("plan destroys %d outside the weekend window", [input.plan.destroys])
    }

Two differences matter. Sentinel reads the clock, and Rego here reads the submission time from the
input, so the answer does not change between evaluations. And Sentinel walks the full plan, while
the input carries the plan's destroy count, which is what a destroy threshold reads. A rule that
needs individual resource changes from the plan is a case for `hold`, so a person reads the plan,
until the input carries more of it.
