<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Pull request review

A review trigger plans a pull request before it merges and posts the result on the pull request. It
works with GitHub, GitHub Enterprise Server, GitLab, and self-managed GitLab. When a pull request is
opened or receives new commits, SwitchTender runs the trigger's template at the pull request's head
commit in the tool's no-change mode, then writes one comment for that template on the pull request and
sets a commit status on the head commit. Every later push updates the same comment in place.

Applying stays in SwitchTender's hands. After the pull request merges, the apply is an ordinary run
of the template, launched the way it always is, and it goes through the same gate and approval queue
as any other change. A person whose GitHub or GitLab account is linked to their SwitchTender account
can also plan and apply from a comment on the pull request, as their SwitchTender account and under
the same rules as the queue, as described in [planning and applying from a
comment](#planning-and-applying-from-a-comment). Nothing else a pull request author writes on the
pull request can approve or release a change.

## Plans and your approval rules

A plan faces your approval rules like any other run. There is no built-in exemption, because a plan
is not free of side effects: a Terraform plan runs provider code and data sources with the
template's credentials, and an Ansible check runs for real any task that sets `check_mode: false`
and any command a `pipe` lookup names. So a rule that holds every Terraform run holds the plan of
every pull request too, and the pull request waits for a person.

To let plans run unattended, add one line to the rule that holds them:

    policies:
      - name: terraform-needs-approval
        tool: terraform
        exclude_dry_run: true

`exclude_dry_run: true` leaves dry runs, and so pull request plans, out of that rule. The apply
after merge is not a dry run and is still held. The line does not change what a plan refuses: a
playbook that forces real tasks under check mode or runs a `pipe` lookup is still refused on a pull
request, and so is a Terraform or OpenTofu configuration with an `external` or
`aws_lambda_invocation` data source, an `http` data source that writes, or anything the review
cannot read in full, as described in [plans that are not change
free](#plans-that-are-not-change-free). A plan of a tool a plugin or the SDK added is refused
before it runs, since nothing can read what it runs. The scan
finds the known ways a plan acts, and it does not prove a plan harmless, so the line trusts the
playbooks and configurations it lets through. See [dry runs and
`exclude_dry_run`](concepts.md#dry-runs-and-exclude-dry-run) for how the gate decides.

A plan a rule holds is reported as waiting for approval. Its comment names the rule and, when
`--public-url` is set, links to the plan in SwitchTender, where an approver releases or rejects it.
Its commit status stays `pending` until then.

## What a plan run is

The plan is the template's own run with three changes:

- It runs in the tool's no-change mode: `terraform plan` or `tofu plan` with a detailed exit code,
  `ansible-playbook --check --diff`, and for Bash, Python, PowerShell, and Go a check of the script
  that does not run it, which previews nothing.
- It is pinned to the head commit the webhook named and fetched from the ref the base repository
  publishes that commit under: `refs/pull/N/head` on GitHub and `refs/merge-requests/N/head` on
  GitLab. If the pull request moved on before the plan started, the plan refuses to run rather than
  plan code nobody asked about. The newer push has its own plan.
- The template's notification channels are left off. A plan of a proposal is not a change those
  channels were set up to hear about.

The run's source is `review`, its actor is the trigger, and it carries a `pull_request` label with the
pull request number, so the runs page can filter to one pull request's plans. The plan's run page
shows the pull request and what it was last told, and says so when a report to it is failing.

Nobody answers the template's survey when a pull request is planned, so every question takes its
default, a secret question's sealed default included, the way a schedule fires the template. A
required question with no default refuses the plan: no run is created, the refusal is recorded, and
the pull request gets a comment naming the question and an `error` status. Give the question a
default and the next push is planned.

## What the comment shows

- Whether the plan succeeded, failed, or is still running or held. A held plan's comment says it is
  waiting for approval, names the rule holding it, and links to the plan, where an approver releases
  or rejects it.
- For Terraform and OpenTofu, the add, change, destroy, and import counts from the plan's summary,
  and the destroy count the plan-content rules weigh. That count is read by the same parser the plan
  gate uses, so it is the number the gate will weigh after merge. A plan whose summary cannot be read
  says so, because the gate holds such an apply rather than treating it as zero.
- For Ansible, the hosts the check would change.
- The decision the apply would get after merge under the rules in force now: no hold, held for
  approval by a named rule, or refused by a named rule, and whether that rule requires an approver
  other than the person who requests the apply. It is computed by the same functions the gate runs,
  against today's rules. The apply is decided again when it is actually submitted.
- A link to the plan run and the approval queue, when `--public-url` is set.
- The receipt of the chain entry that recorded the webhook.
- The plan output, masked, in a collapsed block. A Terraform plan is shown from the point it starts
  describing actions. Output longer than 250 lines or about 30 KB is shortened and links to the full
  log.

The commit status is named `switchtender/<template name>`:

| Plan | Apply decision | GitHub state | GitLab state |
|---|---|---|---|
| Running, or waiting for approval | | `pending` | `pending` |
| Succeeded | No hold, or held for approval | `success` | `success` |
| Succeeded | Refused by a rule | `failure` | `failed` |
| Failed, rejected, or interrupted | | `failure` | `failed` |
| Canceled, or not run | | `error` | `failed` |

A branch protection rule or a merge check can require the status, which makes "the apply would be
refused" block the merge.

## What leaves the server

Plan output can carry secrets. Terraform masks values marked sensitive, and a script masks nothing.
Before any text leaves the server, it passes the same masking the run's log used: the values of every
credential the run executed with, its stored inventory's secrets, its registry login, and the secrets
its own variables and command carry, in their encoded forms too. Secret-looking assignments are
masked on top. The stored log was already masked when it was written, and the comment is masked again
anyway. A server that cannot mask withholds the output and posts the summary alone.

The forge token is a credential of kind `token`, sealed at rest like every other credential and never
returned by the API. A token from an external source, such as Vault, is resolved for each report and
handed back afterward.

Requests to the forge go through the guarded client every outbound request of the server uses: it
refuses the cloud metadata addresses and follows no redirect. With `SWITCHTENDER_EGRESS_PROXY` set,
they leave through that proxy, each after its target's address is checked, and the ambient
`HTTP_PROXY` and `HTTPS_PROXY` are never used.

## Pull requests from forks

A pull request from a fork is not planned unless the trigger sets `allow_forks`. A plan runs the
proposed code with the template's credentials, and anybody can open a pull request from a fork. The
refusal is recorded on the chain, and the pull request gets a commit status, `error` on GitHub and
`failed` on GitLab, reading:

    Not planned: this repository doesn't run plans for pull requests from forks

The status links to this section. The pull request gets no comment. A comment would let anybody able
to open a pull request from a fork make your token write on the pull request, as often as they open
one, so the status is the whole answer.

A maintainer who wants a fork's change planned has two ways. Push the change to a branch of the
repository itself and open the pull request from there, after reading what it runs. Or set
`allow_forks` on the trigger with `PUT /v1/triggers/{id}`, which plans every fork's pull request
from then on. Turn `allow_forks` on only for a template whose credentials a fork's author may
already have. Even then, remember that a Terraform plan executes provider code and data sources, and an
`external` data source runs any program the configuration names.

## Plans that are not change free

An Ansible plan is `ansible-playbook --check`, and a play, block, task, role, or include that sets
`check_mode: false` runs that work for real even under `--check`, as does the command a `pipe`
lookup names, which runs on the controller while a template renders. A Terraform or OpenTofu plan
runs the program every `external` data source names, invokes every `aws_lambda_invocation` data
source's function, and sends every `http` data source's request, with the template's credentials.
Before it plans, the review reads the playbook, or the configuration and every module it calls, at
the pull request's head, the same way the approval gate does. A playbook that forces real tasks or
runs a `pipe` lookup is not planned, since its plan would act from a branch nobody merged, and
neither is a configuration that declares an `external` or `aws_lambda_invocation` data source, or an
`http` data source whose method is anything but GET or HEAD or that carries a request body, since
its plan would run that program or send that call. Neither is one that cannot be read in full, such
as a playbook including a file named only at run time, since what was not read could do the same.
Nor is a plan of a tool a plugin or the SDK added, whose plan runs whatever the plugin coded and
which the review cannot read at all. No run is created, the refusal is recorded on the chain, and the pull request gets a comment naming
what was found, or what could not be read, and an `error` status.

The pull request's commit does not hold the registry and remote modules its configuration calls, so
the review downloads them first, the same way the approval gate does. It runs `terraform get` or
`tofu get` in a private copy of the configuration at the pull request's head, with the template's
own credentials, only where the plan would run, and within the gate's time and size bounds, then
reads what the get installed. The get installs no provider and runs no program. A module hiding one
of those data sources is refused by its address like any other, and a download that fails leaves the
configuration unread, so the plan is refused and the comment says why. A plan that goes ahead
records the download, its exit status, and the digest of the modules it read in the plan run's
`dry_run_scans`, and the plan executes exactly those modules.

A `.terraform` directory committed to the repository is never read. A pull request could commit one
whose manifest points a registry module at a harmless copy beside it while the plan's own `init`
installs the real module, so the review copies the configuration without it and reads only what its
own download installed. There is no setting that makes the review, or the approval gate, trust a
committed copy. To have a module read straight from the repository, vendor it and call it by a local
path, and it is read from the commit like any other file.

A forge stops waiting for a webhook's answer after about ten seconds, and a download can take
longer. The review records the delivery on the chain before anything is downloaded, and answers
the forge within five seconds: with the plan, or the refusal, when the pre-check finished, and with
`accepted` when it is still going. The pull request gets its status and comment either way, once the
pre-check and the plan are done. A redelivery that arrives while they are still going joins them
rather than starting again.

## When the inventory matches no hosts

A template whose inventory is smart or constructed is resolved when the plan is requested. When it
matches no host, the plan is skipped rather than failed, the way a schedule or a webhook fire that
matches no hosts is: no run is created, the webhook is answered with a success, so the forge has
nothing to redeliver, and the skip is its own entry on the audit chain. The pull request gets a
commit status alone, `error` on GitHub and `failed` on GitLab, reading:

    Not planned: the inventory matched no hosts

There is no comment, because nothing is wrong with the pull request. Preview the inventory's hosts
in SwitchTender to see why it matched nothing, and the next push is planned once it matches.

## The audit trail

Each review webhook enters the tamper-evident chain before anything acts on it, and a delivery that
cannot be recorded is refused, the same as a push trigger's fire:

| Entry path | When |
|---|---|
| `/hooks/<trigger>/review/<N>/accepted` | A pull request event passed its checks, before the pre-check downloads anything with the template's credentials. |
| `/hooks/<trigger>/review/<N>/planned` | A plan is about to launch. The plan run carries this entry's receipt. |
| `/hooks/<trigger>/review/<N>/refused` | A fork's pull request, or one whose plan is not change free, such as a playbook forcing real tasks under check mode or running a `pipe` lookup, or a configuration with an `external`, `aws_lambda_invocation`, or writing `http` data source, was not planned. |
| `/hooks/<trigger>/review/<N>/refused/survey/<questions>` | The template's survey has a required question with no default, so the pull request was not planned. |
| `/hooks/<trigger>/review/<N>/skipped` | The template's inventory matched no hosts, so the plan was skipped. The entry commits to the trigger, the template, the inventory, and the reason. |
| `/runs/<run>/outcome/<status>` | The plan run finished, committed like every run's outcome. |
| `/hooks/<trigger>/review/<N>/report` | A comment and status are about to be posted. The entry commits to the plan run, the phase, the commit, the status state, and the SHA-256 of the exact comment body, or to `comment_skipped` with the reason no comment is written: `superseded` when a newer push's plan holds the comment, `fork` for a fork's refusal, `no_hosts` for a plan skipped because its inventory matched no hosts. A report that cannot be recorded is not posted, and a report retried after a failed write is not recorded twice. |
| `/hooks/<trigger>/review/<N>/failed` | The plan could not be launched after the forge was already answered, so the failure is recorded here and in the server log. |
| `/hooks/<trigger>/review/<N>/comment/<comment>/<command>` | A new comment carrying `/switchtender plan` or `/switchtender apply` arrived, before anything acts on it. The entry commits to the forge, the repository, the pull request, the comment's and its author's numeric ids, the SHA-256 of the comment body, and the command. A comment that cannot be recorded does nothing. |
| `/hooks/<trigger>/review/<N>/comment/<comment>/<command>/<result>` | How the command ended: `accepted` for a plan handed to the review's own plan path, which records its own entries, `applied`, or `refused/` with the reason, such as `refused/unlinked`, `refused/fork`, `refused/bot`, `refused/head_moved`, or `refused/separation_of_duties`, or `failed/` with the step that failed on the server's side, such as `failed/forge`, and the run it launched or decided. |
| `/runs/<run>/decision/approved` | An apply was approved from a comment. The entry commits to the comment as described in [what the evidence holds](#what-the-evidence-holds). |

## How reporting holds up

Each plan and each refusal has a report record in the database: the phase the pull request was last
told, the commit status state set, the SHA-256 of the comment body written, and when. The plan's run
page and `GET /v1/runs/{id}` show it as `pull_request_report`.

- Once, across replicas. Every server sharing the database looks for reports that are owed, and
  takes one the way the scheduler takes a due schedule: a compare-and-set on the record, which
  exactly one server wins. Two servers never write one pull request's comment at the same time.
- Across a restart. A starting server reports whatever is owed, including the result of a plan
  that finished while no server was running. A plan from the last hour whose webhook reached a
  server that stopped before recording it is found and reported too.
- In order. A plan is reported as it moves, running, then waiting for approval if a rule holds
  it, then its result. The result waits for the plan's outcome entry on the audit chain, so the pull
  request is never told a plan finished before the chain records how. If the outcome entry has not
  arrived two minutes after the plan ended, the result is posted with a line saying the outcome is
  missing from the chain.
- The newest push keeps the comment. Before writing, the server reads which commit the pull
  request proposes now. A plan of a commit the pull request has moved past never takes the comment
  from the newer push's plan, even when its webhook arrived later. It sets the status on its own
  commit and updates only a comment it wrote itself.
- Retried when the forge fails. A request the forge refuses or never answers is retried with a
  backoff that starts at two seconds and doubles to five minutes. The plan's run page shows the
  failure, the number of attempts, and when the next one goes out. A forge that keeps failing for
  24 hours, such as one whose token was revoked, is given up on, and the run says so.

A server that shuts down cleanly hands back the report it was making at once. One that dies partway
through leaves the report claimed for six minutes, after which another server takes it and posts it
again. The repeat updates the same comment rather than adding one.

## Linking forge accounts

A `/switchtender plan` or `/switchtender apply` comment acts as the SwitchTender account its author
linked. Each person links their own GitHub or GitLab account once, on the Linked accounts page,
through an OAuth application you register on the forge. A link grants nothing by itself. What a
comment may then do is decided by the linked account's own role and grants, exactly as in the queue.

A link is keyed on the forge's numeric account id, never the login. A login can be renamed and then
taken by somebody else, and the numeric id cannot. SwitchTender never stores the login. The token
the forge hands over while linking is used once, to read that id, and then dropped.

### Register the OAuth application

The callback URL is your server's public address followed by `/auth/forge/callback`, such as
`https://switchtender.example.com/auth/forge/callback`. Linking needs `--public-url`, so the forge
can send people back.

On GitHub, open the organization's settings, then Developer settings, then OAuth Apps, and register
a new application with the callback URL above. It needs no scopes, since linking reads only the
numeric id of the account that authorized it. Generate a client secret. GitHub Enterprise Server is
the same, in your server's own Developer settings.

On GitLab, open Applications in the group's settings, or in the Admin area for the whole instance,
and add one with the callback URL above, the `read_user` scope, and Confidential checked.
Self-managed GitLab is the same, on your own instance.

### Configure the server

Pass one `--forge-oauth` per forge. The client secret comes from an environment variable or a file,
never the command line:

    switchtender serve --public-url https://switchtender.example.com \
      --forge-oauth provider=github,client_id=Iv1.0123,secret_env=GITHUB_OAUTH_SECRET \
      --forge-oauth provider=gitlab,client_id=4f2a,secret_file=/run/secrets/gitlab-oauth,web_url=https://gitlab.example.com

`web_url` names a GitHub Enterprise Server or a self-managed GitLab, and its API base is taken to be
`/api/v3` or `/api/v4` under it. Set `api_url` when yours is somewhere else. A link is kept per
forge API base, the same base a review trigger names in `api_url`, so an account on GitHub
Enterprise Server is never mistaken for one on github.com. The page has to be opened at the public
address, since the browser that starts a link must be the one the forge sends back.

### Link and unlink

Open Linked accounts in the navigation, at `/ui/links`. Each forge the server is set up for has a
Link button. It sends you to the forge to authorize the application, and the forge sends you back to
the page with the account linked. The page shows the account by its forge, its host, and its numeric
id.

- One forge account links to one SwitchTender account, and a SwitchTender account links at most one
  account on each forge. Unlink the old one first to move it.
- A bot account cannot be linked: a GitHub account of type Bot, or a GitLab bot user such as the
  account behind a project or group access token.
- Only a person can link or unlink. An agent's token is refused.
- A link finishes only in the browser that started it, within ten minutes. A link address sent to
  somebody else does nothing when they open it, so nobody can tie their forge account to your
  SwitchTender account, or yours to theirs.
- Each link request is signed with a key derived from the server's encryption key together with the
  application's client secret, so somebody holding the client secret alone cannot forge one. Linking
  needs `SWITCHTENDER_ENCRYPTION_KEY` set, and a server without it refuses to start a link.
- Unlinking takes effect at once. A comment from that forge account stops acting as you.
- Deleting a SwitchTender account, through the API or with `switchtender user delete`, ends its
  links first, each recorded on the audit chain as an unlink. A delete whose unlinks cannot be
  recorded is refused, and the account and its links stay. A [backup](backup.md) does not carry
  links, so after a restore each person links again.

Each link and each unlink is an entry on the audit chain, at `/me/forge-links/<link id>/linked` and
`/me/forge-links/<link id>/unlinked`, committing to the SwitchTender account, the forge, its API
base, and the numeric id. The entry holds no login and no token. Each entry is written before the
change it records, so a link never exists without its `linked` entry, and one the chain will not
take is never made. An unlink the chain will not take leaves the link in place, and a link or an
unlink that fails after its entry is written gets the entry that undoes it, so the chain and the
links agree.

The API is `GET`, `POST`, and `DELETE` on `/v1/me/forge-links`, listed in the
[API reference](api.md).

## Planning and applying from a comment

A person whose GitHub or GitLab account is linked to their SwitchTender account can plan and apply
from the pull request. The comment acts as their SwitchTender account, with the rights that account
has in the approval queue and no others. See [linking forge accounts](#linking-forge-accounts) for
how a person links an account.

Two commands are read, on the first line of a new comment that is not blank:

    /switchtender plan [-p TEMPLATE]
    /switchtender apply [PLAN] [-p TEMPLATE]

`PLAN` is the plan id every plan report shows, such as `Plan 0e4f984a`, and a succeeded plan's
report ends with the exact command that applies it. `-p` names the template, by name or id, so when
several review triggers watch one repository only that template's trigger acts and the others leave
the comment alone. Lines after the first are ignored. A first line with anything else on it does
nothing, and so does one indented by a tab or four spaces, which both forges show as a code block,
so an example or a typo is never taken for a command. On GitHub the command goes in a comment on the
pull request's conversation, the kind the Issue comments event delivers, and not in a review or a
comment on a line of the diff.

Before a comment plans or approves anything, SwitchTender reads it back from the forge by its id,
with the trigger's token, and requires the forge's author, pull request, and body to match the
webhook. A signed webhook alone is not enough, since whoever holds the trigger's signing secret
could sign a comment the forge never held. A comment the forge does not confirm, or one more than 24
hours old by the forge's clock, which is a redelivery or a replay, has no effect at all: no plan, no
proposed apply, no record, no reply, and no chain entry beyond the one recording its arrival. A
forged webhook changes nothing.

`/switchtender plan` plans the pull request's current head, read from the forge when the comment
arrives, exactly as a push to the pull request does. The plan runs as the commenter's account, which
needs the operator role and use of everything a launch of the template needs in the queue: the
template, its project, its inventory, its credentials, its registry pull credential, and its worker
queue. Its result is reported in the same comment and commit status as every other plan.

`/switchtender apply` approves and applies a plan SwitchTender reported for the pull request's
current head, one its author could have read:

- `/switchtender apply PLAN` applies exactly that plan, while it is still the head's current plan.
  A plan of an older commit is refused with both commits named, and one that a newer plan of the
  same commit replaced is refused with the newer plan's id.
- A bare `/switchtender apply` is taken only when the head has one reported plan and it was reported
  before the comment was written. Otherwise nothing is applied, and the reply names the plan id to
  comment with.

It happens in two steps, the same way a reconcile is proposed from a drift check:

1. The apply is proposed from the plan. It is the plan's own run, for real, pinned to the plan's
   commit and fetched from the same pull request ref. A Terraform or OpenTofu apply carries the plan
   file the plan saved, which is the plan the pull request was shown, with its digest bound into the
   approval, so the tool refuses it if the state changed after the plan was made. The apply waits
   for approval whatever the rules say, so the comment's decision is always recorded. A plan only
   ever has one apply proposed from it. Its requester is the pull request's author, read from the
   forge, when their forge account is linked, and otherwise the person who commented first.
2. The comment approves that apply as the commenter's account, through the same checks as the
   queue's approve button.

Approving needs the admin role, as it does in the queue, and use of everything the plan runs. An
operator's `/switchtender apply` proposes the apply and leaves it waiting for an approver. Because
the pull request's author is the requester, a rule that requires a different approver keeps the
author from approving their own change, whoever asked for the apply first.

### When a comment does nothing

| Situation | What happens |
|---|---|
| The pull request comes from a fork | Nothing, and no reply. A reply would let anybody able to open a pull request from a fork make your token write on it. A comment never acts on a fork's pull request, whatever `allow_forks` says. |
| The forge does not hold the comment as delivered, or it is more than 24 hours old | Nothing, and no reply. |
| The commenter's forge account is not linked | One reply saying how to link, pointing at the Linked accounts page when `--public-url` is set, once per commenter on each pull request. |
| GitHub marks the comment as written by an app on its author's behalf | One reply. An agent working through an app may propose changes and never approve them. |
| The commenter is a bot: a GitHub account of type Bot, or a GitLab bot user | One reply saying bots cannot plan or apply. A bot account cannot be linked, so a bot that is not linked gets the reply for an unlinked account instead. |
| The pull request is closed | One reply. |
| SwitchTender has reported no plan for the pull request | One reply. Comment `/switchtender plan` first. |
| The pull request's head moved since the plan | One reply naming both commits. Nothing is applied until the new head's plan is reported. |
| A bare apply could mean more than one plan, or its plan was reported after the comment was written | One reply naming the plan id to apply. |
| The named plan is unknown, or a newer plan of the same commit replaced it | One reply. |
| The plan failed, was rejected, or has not finished | One reply. |
| A Terraform or OpenTofu plan kept no plan file | One reply. A plan that found no changes keeps none, and a newer plan of the same configuration replaces an older one's. |
| The account lacks the role or a grant | One reply. An operator's apply is proposed and left waiting. |
| A rule requires a different approver than the pull request's author or the person who asked for the apply | The apply stays held, and one reply says so. Another linked approver's `/switchtender apply` approves it, or an approver releases it in SwitchTender. |
| A rule requires a reason to approve | The apply stays held. A comment carries no reason, so approve it in SwitchTender. |
| The plan's apply was already decided, or was decided while the comment was on its way | One reply naming the apply's run. |
| A rule refuses the apply, or routes it to a queue this install's license does not cover | One reply. Nothing is applied. |
| The template cannot be planned for a pull request, such as a workflow template | One reply saying why. |
| Something failed on SwitchTender's side, such as a call to the forge | One reply saying the command did nothing. Comment again to retry. |

### What a comment costs

The link is looked up on the server before anything is read from the forge. A refusal found there,
such as an unlinked account, is answered once the forge confirms the comment and the pull request is
not a fork's, which costs your token one read of the comment, one of the pull request, and the one
reply that author is owed on that pull request. Every later comment from that author on that pull
request costs the forge nothing, and so does a repeat of a comment the forge would not confirm,
tried at most once in ten minutes. A refusal's reply goes out once per author, pull request, and
reason, and a refusal whose words change, such as a head that moved again, is answered once more.

Each forge account may have 20 commands acted on through one trigger in ten minutes. A command past
that is answered `429` and leaves nothing behind: no record, no chain entry, and no forge call. The
limit is counted on each server.

The record that a comment was acted on, and the record of each reply, is kept for seven days and
then removed. A comment older than 24 hours is refused anyway, so no comment can act twice after its
record is gone.

### Once, however often it arrives

Each comment is acted on once. Its forge id, with the trigger it arrived through, keys a record in
the database every server shares, so a redelivered webhook, or one delivery reaching two servers, is
answered and ignored. A second `/switchtender apply` comment finds the apply the first one proposed,
approves it only if it is still waiting, and otherwise says the plan was already decided. Each reply
is posted once across replicas, by the same claim the plan reports use.

Only a newly created comment is read. An edit or a deletion is ignored on both forges, so changing a
comment afterward cannot plan, apply, or change what was approved.

### What the evidence holds

The decision's chain entry commits the forge and its API base, the repository, the pull request,
the comment's numeric id, its author's numeric id, and the SHA-256 of the comment body as the forge
holds it, and the plan run whose plan it approved. The decision is recorded under the commenter's
SwitchTender account with the decider type `forge_comment`. The run's decisions, its evidence
dossier, and its page in SwitchTender show that the decision came from a comment, with those ids and
the fingerprint, and a receipt for the run discloses the same comment in the decision's body, where
a verifier checks it against the digest the chain committed. An edit to the comment or its deletion
afterward changes none of it.

### Security

- A comment is trusted only as far as the forge confirms it. GitHub's signature or GitLab's secret
  token is verified before the body is read, and the comment is then read back from the forge by
  its id before it plans or approves.
- A forge account is known by its numeric user id, never its login. A renamed account keeps its
  link, and somebody who later takes an old login gets nothing.
- The comment acts with the linked account's own rights. A forge role, such as write access to the
  repository, grants nothing in SwitchTender.
- A fork's pull request is never answered, and any other refusal is answered once per author, pull
  request, and reason, so nobody can use comments to make your token post again and again.
- Bots are refused, a comment GitHub marks as written by an app for its author is refused, and an
  agent's token can never link a forge account. Agents can propose changes. Only authorized humans
  can approve them. Separation of duties can require an independent human when policy demands it.
- What the forges cannot tell apart: GitLab marks nothing like GitHub's app marker, and neither
  forge marks a comment written with a person's own access token. A person who hands an agent their
  forge token hands it their approval rights, so keep such tokens out of agents' reach.

## Setting it up

You need the server's encryption key set, because a review trigger always verifies its deliveries and
its signing secret is sealed. Set `--public-url` so comments link to the run, and so a plan waiting
for approval links to the place it is approved.

Decide what your rules do with plans before the first pull request arrives. A rule that holds the
template's runs holds its plans too, unless the rule carries the one line from
[plans and your approval rules](#plans-and-your-approval-rules):

    exclude_dry_run: true

The template must run from a project, since that is where the pull request's commit is fetched, and
the project's repository must be the base repository the pull requests target. A workflow template is
not supported, because its steps are not planned as a unit. A template that splits across shards is
planned as one run.

### GitHub

1. Create a token that can read pull requests, comment on them, and set commit statuses in the
   repository. A fine-grained personal access token needs Pull requests read and write and Commit
   statuses read and write. A classic token needs `repo`. A GitHub App installation token with the
   same permissions works too.
2. Store it as a credential of kind `token`.
3. Create the trigger:

       curl -s -X POST -H "Authorization: Bearer $ST_TOKEN" -H "Content-Type: application/json" \
         localhost:8080/v1/triggers -d '{
           "name": "network plans",
           "template_id": "tpl_network",
           "review": {
             "provider": "github",
             "repository": "acme/infra",
             "credential_id": "cred_github"
           }
         }'

   The response carries `webhook_path` and `signing_secret`, each shown once.
4. In the repository's settings, add a webhook. The payload URL is your server's public address
   followed by `webhook_path`. The content type is `application/json`. The secret is
   `signing_secret`. Choose "Let me select individual events" and select Pull requests. Select
   Issue comments too for [comment commands](#planning-and-applying-from-a-comment).

For GitHub Enterprise Server, add `"api_url": "https://github.example.com/api/v3"` to `review`.

### GitLab

1. Create a project access token or a personal access token with the `api` scope, on an account with
   at least the Developer role, which commit statuses need.
2. Store it as a credential of kind `token`.
3. Create the trigger with `"provider": "gitlab"` and the project's full path as `repository`, such
   as `"platform/infra/network"`. For self-managed GitLab, add
   `"api_url": "https://gitlab.example.com/api/v4"`.
4. In the project's Settings, then Webhooks, add a webhook. The URL is your server's public address
   followed by `webhook_path`. The secret token is `signing_secret`. Select Merge request events,
   and Comments for [comment commands](#planning-and-applying-from-a-comment).

GitLab sends its secret token as the `X-Gitlab-Token` header rather than signing the body, and the
trigger compares it in constant time. GitHub signs the body with HMAC SHA-256, checked against
`X-Hub-Signature-256`.

### Changing a review trigger

`PUT /v1/triggers/{id}` takes a `review` object that replaces the trigger's settings, checked the same
way as at creation. A review trigger's signature check cannot be turned off, and a push trigger cannot
become a review trigger or the reverse, because the forge was configured for one of them. Rotate the
signing secret with `POST /v1/triggers/{id}/rotate-secret` and update the forge's webhook to match.

## What it does not do

- A comment carries no reason for a decision. A rule that requires a reason to approve is satisfied
  in SwitchTender, not from a comment.
- A comment acts on the plan of the trigger its webhook arrived through. When several review
  triggers watch one repository, each acts on its own template's plan, and each answers the comment.
- Ansible's check mode is only as safe as the playbook. A playbook that sets `check_mode: false` or
  runs a `pipe` lookup is refused, as described above, but another lookup or plugin that runs code
  on the controller, or a custom module that claims to support check mode and acts anyway, is not
  something the read can see. Review the playbooks a review trigger plans, and do not allow forks on
  one whose playbooks you have not read.
- A Terraform or OpenTofu plan runs provider code with the template's credentials. Providers are
  trusted code the team chose, and the review does not judge them: it refuses `external` and
  `aws_lambda_invocation` data sources, `http` data sources that write, and configuration it cannot
  read, nothing else.
- The apply preview grades an Ansible playbook from the project's checkout, which holds the branch
  until the pull request merges. A rule with a reversibility floor that depends on what the pull
  request changes in the playbook can decide differently when the apply is submitted.
- The comment is found by a marker the server writes, on comments posted by the token's own account.
  When the forge will not say which account the token belongs to, as with some GitHub App tokens, the
  oldest marked comment is used, and a refused edit falls back to a new comment.
