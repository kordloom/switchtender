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

Applying stays in SwitchTender. After the pull request merges, the apply is an ordinary run of the
template, launched the way it always is, and it goes through the same gate and approval queue as any
other change. Nothing a pull request author writes on the pull request can approve or release it.

## Plans and your approval rules

A plan faces your approval rules like any other run. There is no built-in exemption, because a plan
is not free of side effects: a Terraform plan runs provider code and data sources with the
template's credentials, and an Ansible check runs for real any task that sets `check_mode: false`.
So a rule that holds every Terraform run holds the plan of every pull request too, and the pull
request waits for a person.

To let plans run unattended, add one line to the rule that holds them:

    policies:
      - name: terraform-needs-approval
        tool: terraform
        exclude_dry_run: true

`exclude_dry_run: true` leaves dry runs, and so pull request plans, out of that rule. The apply
after merge is not a dry run and is still held. The line does not change what a plan refuses: a
playbook that forces real tasks under check mode is still refused on a pull request, and so is a
Terraform or OpenTofu configuration that runs an external data source, or that cannot be read in
full, as described in [plans that are not change free](#plans-that-are-not-change-free). See [dry
runs and `exclude_dry_run`](concepts.md#dry-runs-and-exclude-dry-run) for how the gate decides.

A plan a rule holds is reported as waiting for approval. Its comment names the rule and, when
`--public-url` is set, links to the plan in SwitchTender, where an approver releases or rejects it.
Its commit status stays `pending` until then.

## What a plan run is

The plan is the template's own run with three changes:

- It runs in the tool's no-change mode: `terraform plan` or `tofu plan` with a detailed exit code,
  `ansible-playbook --check --diff`, and a syntax check for Bash, Python, PowerShell, and Go.
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
`check_mode: false` runs that work for real even under `--check`. A Terraform or OpenTofu plan runs
the program every `external` data source names, with the template's credentials. Before it plans,
the review reads the playbook, or the configuration and every module it calls, at the pull request's
head, the same way the approval gate does. A playbook that forces real tasks is not planned, since
its plan would change hosts from a branch nobody merged, and neither is a configuration that
declares an external data source, since its plan would run that program. Neither is one that cannot
be read in full, such as a playbook including a file named only at run time, since what was not
read could do the same. No run is created, the refusal is recorded on the chain, and the pull
request gets a comment naming what was found, or what could not be read, and an `error` status.

The pull request's commit does not hold the registry and remote modules its configuration calls, so
the review downloads them first, the same way the approval gate does. It runs `terraform get` or
`tofu get` in a private copy of the configuration at the pull request's head, with the template's
own credentials, only where the plan would run, and within the gate's time and size bounds, then
reads what the get installed. The get installs no provider and runs no program. A module hiding an
external data source is refused by its address like any other, and a download that fails leaves
the configuration unread, so the plan is refused and the comment says why. A plan that goes ahead
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
| `/hooks/<trigger>/review/<N>/refused` | A fork's pull request, or one whose plan is not change free, such as a playbook forcing real tasks under check mode or a configuration with an external data source, was not planned. |
| `/hooks/<trigger>/review/<N>/refused/survey/<questions>` | The template's survey has a required question with no default, so the pull request was not planned. |
| `/hooks/<trigger>/review/<N>/skipped` | The template's inventory matched no hosts, so the plan was skipped. The entry commits to the trigger, the template, the inventory, and the reason. |
| `/runs/<run>/outcome/<status>` | The plan run finished, committed like every run's outcome. |
| `/hooks/<trigger>/review/<N>/report` | A comment and status are about to be posted. The entry commits to the plan run, the phase, the commit, the status state, and the SHA-256 of the exact comment body, or to `comment_skipped` with the reason no comment is written: `superseded` when a newer push's plan holds the comment, `fork` for a fork's refusal, `no_hosts` for a plan skipped because its inventory matched no hosts. A report that cannot be recorded is not posted, and a report retried after a failed write is not recorded twice. |
| `/hooks/<trigger>/review/<N>/failed` | The plan could not be launched after the forge was already answered, so the failure is recorded here and in the server log. |

## How reporting holds up

Each plan and each refusal has a report record in the database: the phase the pull request was last
told, the commit status state set, the SHA-256 of the comment body written, and when. The plan's run
page and `GET /v1/runs/{id}` show it as `pull_request_report`.

- **Once, across replicas.** Every server sharing the database looks for reports that are owed, and
  takes one the way the scheduler takes a due schedule: a compare-and-set on the record, which
  exactly one server wins. Two servers never write one pull request's comment at the same time.
- **Across a restart.** A starting server reports whatever is owed, including the result of a plan
  that finished while no server was running. A plan from the last hour whose webhook reached a
  server that stopped before recording it is found and reported too.
- **In order.** A plan is reported as it moves, running, then waiting for approval if a rule holds
  it, then its result. The result waits for the plan's outcome entry on the audit chain, so the pull
  request is never told a plan finished before the chain records how. If the outcome entry has not
  arrived two minutes after the plan ended, the result is posted with a line saying the outcome is
  missing from the chain.
- **The newest push keeps the comment.** Before writing, the server reads which commit the pull
  request proposes now. A plan of a commit the pull request has moved past never takes the comment
  from the newer push's plan, even when its webhook arrived later. It sets the status on its own
  commit and updates only a comment it wrote itself.
- **Retried when the forge fails.** A request the forge refuses or never answers is retried with a
  backoff that starts at two seconds and doubles to five minutes. The plan's run page shows the
  failure, the number of attempts, and when the next one goes out. A forge that keeps failing for
  24 hours, such as one whose token was revoked, is given up on, and the run says so.

A server that shuts down cleanly hands back the report it was making at once. One that dies partway
through leaves the report claimed for six minutes, after which another server takes it and posts it
again. The repeat updates the same comment rather than adding one.

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
   `signing_secret`. Choose "Let me select individual events" and select Pull requests.

For GitHub Enterprise Server, add `"api_url": "https://github.example.com/api/v3"` to `review`.

### GitLab

1. Create a project access token or a personal access token with the `api` scope, on an account with
   at least the Developer role, which commit statuses need.
2. Store it as a credential of kind `token`.
3. Create the trigger with `"provider": "gitlab"` and the project's full path as `repository`, such
   as `"platform/infra/network"`. For self-managed GitLab, add
   `"api_url": "https://gitlab.example.com/api/v4"`.
4. In the project's Settings, then Webhooks, add a webhook. The URL is your server's public address
   followed by `webhook_path`. The secret token is `signing_secret`. Select Merge request events.

GitLab sends its secret token as the `X-Gitlab-Token` header rather than signing the body, and the
trigger compares it in constant time. GitHub signs the body with HMAC SHA-256, checked against
`X-Hub-Signature-256`.

### Changing a review trigger

`PUT /v1/triggers/{id}` takes a `review` object that replaces the trigger's settings, checked the same
way as at creation. A review trigger's signature check cannot be turned off, and a push trigger cannot
become a review trigger or the reverse, because the forge was configured for one of them. Rotate the
signing secret with `POST /v1/triggers/{id}/rotate-secret` and update the forge's webhook to match.

## What it does not do

- Approving an apply from a pull request comment is not supported. SwitchTender has no way to tie a
  forge account to a SwitchTender account it would trust to release a change, and a comment can be
  written by anyone with access to the pull request. Approvals happen in SwitchTender.
- Ansible's check mode is only as safe as the playbook. A playbook that sets `check_mode: false` is
  refused, as described above, but a custom module that claims to support check mode and acts
  anyway is not something the read can see. Review the playbooks a
  review trigger plans, and do not allow forks on one whose playbooks you have not read.
- A Terraform or OpenTofu plan runs provider code with the template's credentials. Providers are
  trusted code the team chose, and the review does not judge them: it refuses external data sources
  and configuration it cannot read, nothing else.
- The apply preview grades an Ansible playbook from the project's checkout, which holds the branch
  until the pull request merges. A rule with a reversibility floor that depends on what the pull
  request changes in the playbook can decide differently when the apply is submitted.
- The comment is found by a marker the server writes, on comments posted by the token's own account.
  When the forge will not say which account the token belongs to, as with some GitHub App tokens, the
  oldest marked comment is used, and a refused edit falls back to a new comment.
