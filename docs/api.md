<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# HTTP API

Every endpoint the server exposes. The API is served under the `/v1` base path. The web UI at
`/ui/`, along with `/healthz`, `/readyz`, `/metrics`, `/.well-known/loomseal.json`, the workload
identity federation documents under `/.well-known/`, the OpenID Connect and SAML sign-in routes, the
webhook `/hooks` path, and the `/relay` worker path, is unversioned. The root redirects to the UI. A
template's provisioning callback, `POST /v1/templates/{id}/callback`, is versioned and carries no
token: the host config key in its body is the credential. The same holds for the one AWX path
served, a template's AWX-compatible callback address,
`POST /api/v2/job_templates/{awx id}/callback/`. Nothing else under `/api/` is served.

`/.well-known/loomseal.json` is deliberately public and unauthenticated: it publishes this install's
signing key and identifiers so a relying party can pin the key from a channel independent of any
bundle it is handed.

`/.well-known/openid-configuration` and `/.well-known/jwks.json` are public and unauthenticated for
the same kind of reason: a cloud verifying a run's identity token fetches them and has no account
here. They hold public keys only, and answer `404` until `--federation-issuer` is set. See
[Federated cloud credentials](federation.md).

## Licensed endpoints

Almost every endpoint below runs on Community, which needs no license. These answer `403` without one,
with a message naming the feature rather than failing in some subtler way:

| Endpoint | Tier | Note |
|----------|------|------|
| `GET /v1/audit/register` | Team | The period change register, covering the last 90 days unless `from` and `to` name another period, each a date or an RFC 3339 timestamp. Per-run dossiers, receipts, bundles, and `GET /v1/audit/verify` are free. |
| `POST /v1/drift/reconcile` | Team | One-click reconcile. Drift detection is free. |
| `POST` and `PUT /v1/policies` | Team, past the free set | Deny rules, risk floors, actor scoping, and distinct-approver separation of duties. One require-approval policy or one exemption is Community, Pro holds five, Team is uncapped. The built-in hold on agent runs is not a stored policy and counts toward no limit. |
| A named `queue` on `POST /v1/runs`, `POST /v1/pipelines`, and `POST` or `PUT` on `/v1/templates` and `/v1/inventories` | Team | Only a worker serves a named queue, and every worker is Team. The default queue is the server's own pool and is always allowed. A run that inherits a queue from its template or inventory is refused the same way. |

Two more are enforced somewhere other than the request:

- `/relay` is served whatever the license. The gate is on the other end: `switchtender worker
  --server` refuses to start without Team, and so does a worker sharing the database: every worker process is gated.
- The directory sign-in routes need Pro, enforced at startup rather than per request. Configuring any
  of OIDC, SAML, LDAP, or JWT without a license refuses the server at startup, so those routes are
  either licensed or absent.

| Method | Path                    | What                                                    |
|--------|-------------------------|---------------------------------------------------------|
| POST   | `/v1/runs`                 | Submit a run. `shards` of two or more splits it.        |
| GET    | `/v1/runs`                 | Run history, newest first. Pages with `limit` (default 200, maximum 1000) and `offset`; the response carries `has_more` and `next_offset`. Filters: `status`, `tool`, `order`, `task`, `after`, `before`, and `q` for a text search over the run. |
| GET    | `/v1/runs/{id}`            | One run. A [pull request review](pull-request-review.md) plan also carries `pull_request_report`: the pull request, the phase and commit status it was last told, when, and a forge failure being retried with its attempts and next try. |
| POST   | `/v1/runs/{id}/cancel`     | Cancel a pending or running run.                        |
| POST   | `/v1/runs/{id}/retry`      | New split from only the failed shards of a finished one.|
| POST   | `/v1/runs/{id}/relaunch-failed` | Re-run only the hosts a finished run left failed or unreachable. |
| POST   | `/v1/runs/{id}/approve`    | Release a run held for approval so it runs. Given a workflow approval step's id, it approves the step, and an optional `state_digest` body field binds the decision to the state the approver was shown. Given a workflow waiting at an approval step, it answers 409 naming each waiting step and the call that decides it, since only a step's own decision moves the workflow. An optional `reason` is recorded as audit evidence, as [Approver reasons](#approver-reasons) describes. |
| POST   | `/v1/runs/{id}/reject`     | Deny a run held for approval, or deny a workflow approval step, given the step's id, so the workflow takes its deny path. A workflow waiting at an approval step is refused with 409 the way an approval is. An optional `reason` is recorded the same way. |
| GET    | `/v1/runs/{id}/decisions`  | The run's decision records: each approval and denial a person made on it or on its approval steps, the reason given, its corrections, any redaction, and for an agent-initiated run the separation-of-duties evaluation. Admin, or the actor who asked for the run. |
| POST   | `/v1/runs/{id}/decisions/{decision}/corrections` | Append a correction to a decision's reason, `{"text": "..."}`. A reason is never edited. Admin. |
| POST   | `/v1/runs/{id}/decisions/{record}/redact` | Remove a reason's text and random value, `{"category": "personal_data"}` (or `secret`, `other`), recorded on the chain. Admin. |
| GET    | `/v1/approvals`            | Workflow approval steps waiting for a decision: the workflow, the step and its description, the steps it waited behind, what each answer runs, when it times out, and the `state_digest` to decide against. Viewer role. |
| GET    | `/v1/attention`            | What is stopping work: one count per main blocker, worker lost, no worker available, approval needed, and blocked, and each item with its other conditions, how long it has been in its current blocker, who can act, and what happens next. `?blocker=` lists one blocker's items. See [What needs attention](#what-needs-attention). Viewer role. |
| GET    | `/v1/runs/{id}/shards`     | Shard runs of a split.                                  |
| GET    | `/v1/runs/{id}/steps`      | Step runs of a pipeline.                                |
| GET    | `/v1/runs/{id}/logs`       | Captured output as plain text, streamed. `?tail=<bytes>` returns only the end, capped at 4 MiB; when anything was dropped the response carries `Switchtender-Log-Truncated: 1` and `Switchtender-Log-Omitted-Bytes`. |
| GET    | `/v1/runs/{id}/evidence`   | Self-contained HTML evidence document for one run. `?format=json` returns the same content as JSON. |
| GET    | `/v1/runs/{id}/receipt`    | Signed LoomSeal receipt proving what this run did. `?sparse` discloses only this run's own entries, each proved to belong to the whole chain; `?from=<size>` adds a consistency proof that the log only appended since that size. The response carries the signing key's id in a `Switchtender-Key-Id` header. |
| POST   | `/v1/runs/{id}/rerun`      | Submit a fresh run with this run's execution settings.  |
| POST   | `/v1/runs/{id}/stream-ticket` | Mint a short-lived, single-use ticket for opening this run's event stream. |
| GET    | `/v1/runs/{id}/events`     | Structured events as JSON. `?after=<seq>` and `?limit` page them, and the response carries `next_after` to continue. `?download=1` streams the same events as newline-delimited JSON with a filename attachment. |
| GET    | `/v1/runs/{id}/notifications` | What the run's named notification targets were told, in the run's event order: each event, each target, and whether the delivery arrived, is pending, failed after its retries, or was skipped, with why. Operator role. |
| GET    | `/v1/runs/{id}/compare`    | What changed against a baseline run: host verdicts, task timing, duration. `with=` names the baseline or `prev` for the previous run of the same source. |
| GET    | `/v1/runs/{id}/stream`     | Live events and log over Server-Sent Events. Opened with `?ticket=` from the endpoint above, since EventSource cannot set a header. |
| POST   | `/v1/runs/{id}/explain`    | Advisory AI explanation of a run, when a provider is configured. |
| POST   | `/v1/ai/draft`             | Advisory AI draft of a bash, python, powershell, or go step script from a description. Operator role. |
| POST   | `/v1/ai/ask`               | Advisory AI answer to a fleet question, from run, health, and drift metadata. Rate limited. |
| POST   | `/v1/ai/propose-run`       | Turn a plain-language request into a run proposal, validated and held for approval. Operator role. |
| POST   | `/v1/drift/reconcile`      | Build a reconcile proposal for a drifted host, held for approval. A Terraform or OpenTofu proposal carries the plan the check saved, and a check that kept none answers `409`. Operator role. |
| POST   | `/v1/pipelines`            | Submit ordered playbook steps as one pipeline.          |
| POST   | `/v1/schedules`            | Cron or RFC 5545 recurrence schedule for a run, split, pipeline, or template. The response records `created_by`. |
| GET    | `/v1/schedules`            | List schedules.                                         |
| GET    | `/v1/schedules/preview`    | Next fire times for a cron expression or a recurrence rule and timezone, without saving anything. |
| GET    | `/v1/schedules/{id}`       | One schedule.                                           |
| PUT    | `/v1/schedules/{id}`       | Update a schedule.                                      |
| DELETE | `/v1/schedules/{id}`       | Delete a schedule.                                      |
| GET    | `/v1/fleet`                | Hosts ranked by failures over recent runs, flaky flags. |
| GET    | `/v1/hosts/{host}/runs`    | One host's recent per-run outcomes.                     |
| GET    | `/v1/hosts/{host}/facts`   | The most recent Ansible facts gathered for one host.    |
| GET    | `/v1/estate`               | The estate as it stood at an instant. `at` is RFC 3339 and defaults to now, so the current estate is the same query with no argument. Each host carries the newest facts gathered at or before that instant, so a host holds its last observed state until something newer was seen, and a host first gathered afterward is absent rather than invented. `withheld` counts hosts left out because the caller may not read the run that gathered them. Retention deleting that run does not withhold it: the purge retains what decided readability, so a reading stays readable to whoever could always read it. `horizon` is the oldest retained reading and `before_history` reports that the instant predates it, which separates an estate that was empty from records that do not reach that far. |
| GET    | `/v1/estate/diff`          | What changed between two instants. `from` is required, `to` defaults to now. Each host is added, changed, or unobserved, and a changed host names the fact keys that differ with the value at each end. `unobserved` means nothing gathered that host inside the window, so its state is carried forward rather than confirmed: not known to have changed, and not known not to have. `unchanged` counts hosts gathered in the window and found identical, which is a different and stronger statement. |
| GET    | `/v1/changes`              | Every change the caller can see, newest first, each summarized without its member runs. Built by scanning recent runs for the label, so `scanned` reports how many were read and `partial` reports that the scan was capped: a change whose runs are all older than that is not listed. The period change register answers a date range exhaustively. |
| GET    | `/v1/changes/{change}`     | One change: every run carrying the `change` label with that value, as one thing. Carries the span, who acted, and an outcome derived from the member runs rather than declared, so it cannot disagree with what happened. A change is what an auditor asks about; a run is what an executor produces. `withheld` counts members the caller may not read, so a partial change is never mistaken for the whole one. |
| GET    | `/v1/tasks`                | Per-task duration trends over recent runs.              |
| GET    | `/v1/drift`                | Resources drifting from desired state, from dry runs.   |
| POST   | `/v1/projects`             | Register a git project. Runs record their commit.       |
| GET    | `/v1/projects`             | List projects.                                          |
| PUT    | `/v1/projects/{id}`        | Update a project.                                       |
| DELETE | `/v1/projects/{id}`        | Delete a project. 409 while a template or source uses it.|
| GET    | `/v1/projects/{id}/files`  | Browse the project checkout's tree.                     |
| GET    | `/v1/projects/{id}/file`   | Read one file from the project checkout. `?path=` within the repo. |
| POST   | `/v1/templates`            | Save a launch preset.                                   |
| GET    | `/v1/templates`            | List templates.                                         |
| POST   | `/v1/templates/{id}/launch`| Launch a template, answering its survey and choosing selectable credentials if it has them. |
| PUT    | `/v1/templates/{id}`       | Update a template.                                      |
| DELETE | `/v1/templates/{id}`       | Delete a template.                                      |
| POST   | `/v1/templates/{id}/callback-key` | Mint or rotate the template's provisioning callback key, returned once. Needs the server encryption key. |
| DELETE | `/v1/templates/{id}/callback-key` | Revoke the template's callback key. Every host holding it is refused from then on. |
| POST   | `/v1/templates/{id}/callback` | A host's provisioning callback. Authenticated by the host config key in the body rather than a token. See [provisioning callbacks](#provisioning-callbacks). |
| POST   | `/api/v2/job_templates/{awx id}/callback/` | The same callback at the address AWX gave an imported template, with or without the trailing slash. POST only. See [provisioning callbacks](#provisioning-callbacks). |
| POST   | `/v1/triggers`             | Create a webhook trigger, or with `review` a [pull request review](pull-request-review.md) trigger, returns a signing secret once.|
| PUT    | `/v1/triggers/{id}`        | Rename a trigger, toggle signature enforcement, or replace a review trigger's `review` settings.|
| POST   | `/v1/triggers/{id}/rotate-secret` | Rotate the signing secret, shown once.           |
| GET    | `/v1/triggers`             | List webhook triggers, each with `last_error` saying why its last delivery started no run. |
| DELETE | `/v1/triggers/{id}`        | Delete a trigger, revoking its webhook.                 |
| POST   | `/hooks/{token}`        | Fire a trigger from a git push, or plan a pull request for a review trigger. A required HMAC signature, or GitLab's token, is checked first. A fire whose inventory matches no hosts is skipped and answered `200` with `skipped`. Answers within five seconds, with `accepted` when the launch is still going.|
| POST   | `/v1/notifications`        | Create a named notification target. Its address and key are sealed at rest and never returned. Admin. |
| GET    | `/v1/notifications`        | List notification targets, secrets never included. Operator role. |
| GET    | `/v1/notifications/{id}`   | One notification target. Operator role. |
| PUT    | `/v1/notifications/{id}`   | Update a target. A blank or masked address or key keeps the stored one. Admin, or a manage grant on it. |
| DELETE | `/v1/notifications/{id}`   | Delete a target and every attachment it has. Admin, or a manage grant on it. |
| GET    | `/v1/notifications/{id}/attachments` | The objects a target is attached to, and for which events. Operator role. |
| GET    | `/v1/notifications/{id}/deliveries` | A target's recent deliveries, newest first. `?status=failed` keeps the failures. Operator role. |
| POST   | `/v1/notifications/{id}/attachments` | Attach a target to a template, workflow, schedule, project, or organization for one event. Use of the target and management of the object. |
| DELETE | `/v1/notifications/{id}/attachments/{attachment}` | Detach one attachment, asking what attaching it asked. |
| POST   | `/v1/credentials`          | Store a credential, encrypted at rest. Fifteen built-in kinds, or a custom type via `type_id` and `fields`. Non-secret `settings` ride beside the secret and return from the API. The four federated kinds, `aws_oidc`, `gcp_oidc`, `azure_oidc`, and `oidc_token`, take `settings` and no `secret`. |
| GET    | `/v1/credentials`          | List credentials, secrets never included.               |
| POST   | `/v1/credential-types`     | Define a custom credential type: fields and how they inject. Admin only. |
| GET    | `/v1/credential-types`     | List custom credential types. Admin only.               |
| GET    | `/v1/credential-types/{id}`| One custom credential type. Admin only.                 |
| PUT    | `/v1/credential-types/{id}` | Replace a custom credential type, keeping its creation time and origin. Admin only. |
| DELETE | `/v1/credential-types/{id}` | Delete a custom credential type. Admin only.           |
| PUT    | `/v1/credentials/{id}`     | Update a credential. A custom-typed credential takes `fields`, or a built-in `kind` with that kind's `secret` to move onto the kind. |
| GET    | `/.well-known/openid-configuration` | The federation issuer's discovery document. Public. |
| GET    | `/.well-known/jwks.json`   | The federation issuer's public key set: every key from its creation until its removal, which is the signing key, a key a rotation published ahead of signing, and any key retired within the last 24 hours. Public. |
| GET    | `/v1/federation/keys`      | The federation signing keys by id, each with its state and its created, activated, retired, and removed times. No private material. |
| POST   | `/v1/federation/keys/rotate` | Normal rotation: publish a new federation key now and start signing with it in 24 hours. The current key signs until then and stays published 24 hours after. 409 while a rotation is already under way. Admin only. |
| POST   | `/v1/federation/keys/rotate/emergency` | Emergency rotation, for a key that may be compromised: a new key signs at once, and every other key leaves the published set now with its private half erased, not only the one signing. The keys are all stored the same way, so a suspected compromise of one is treated as a compromise of all. Runs holding a token from a removed key fail. Admin only. |
| DELETE | `/v1/credentials/{id}`     | Delete a credential. 409 while an object still uses it. |
| POST   | `/v1/auth/login`           | Sign in with username and password, returns a token.    |
| POST   | `/v1/auth/check`           | Verify an API token.                                    |
| GET    | `/v1/auth/me`              | Who the server resolved the caller to be.               |
| POST   | `/v1/auth/logout`          | End the caller's own session, revoking its token.       |
| POST   | `/v1/tokens`               | Mint a token bound to an account. Returns it once, with `created_by` naming who minted it. |
| GET    | `/v1/tokens`               | List tokens without secrets, each with `created_by` and `created_by_type`. Admin only. |
| DELETE | `/v1/tokens/{id}`          | Revoke a token everywhere at once. Admin only.          |
| GET    | `/auth/oidc/login`      | Start the OpenID Connect sign-in handshake.             |
| GET    | `/auth/oidc/callback`   | Complete the OIDC handshake and issue a token.          |
| GET    | `/v1/me/forge-links`       | The caller's own linked GitHub and GitLab accounts, and the forges this server can link. See [linking forge accounts](pull-request-review.md#linking-forge-accounts). |
| POST   | `/v1/me/forge-links`       | Start linking a forge account to the caller's own account. Returns the forge's `authorize_url`. A person only, never an agent. |
| DELETE | `/v1/me/forge-links/{id}`  | Unlink one of the caller's own forge accounts. Recorded on the audit chain. |
| GET    | `/auth/forge/callback`  | Where the forge sends the browser back to finish a link. Public, checked by its signed state and the browser's cookie. |
| GET    | `/auth/saml/login`      | Start the SAML sign-in handshake.                       |
| POST   | `/auth/saml/acs`        | Consume the IdP assertion and issue a token.            |
| GET    | `/auth/saml/metadata`   | Service provider metadata for IdP registration.         |
| POST   | `/v1/users`                | Create an account with a role and an optional profile.  |
| GET    | `/v1/users`                | List accounts with their profiles, admin only.          |
| PUT    | `/v1/users/{id}`           | Update an account's role, password, or profile.         |
| DELETE | `/v1/users/{id}`           | Delete an account. Its tokens stop working.             |
| POST   | `/v1/teams`                | Create a team of users.                                 |
| GET    | `/v1/teams`                | List teams.                                             |
| DELETE | `/v1/teams/{id}`           | Delete a team and its memberships.                      |
| POST   | `/v1/teams/{id}/members`   | Add a user to a team.                                   |
| GET    | `/v1/teams/{id}/members`   | List a team's members.                                  |
| DELETE | `/v1/teams/{id}/members/{userID}` | Remove a user from a team.                       |
| POST   | `/v1/orgs`                 | Create an organization.                                 |
| GET    | `/v1/orgs`                 | List organizations.                                     |
| DELETE | `/v1/orgs/{id}`            | Delete an organization and its memberships.             |
| POST   | `/v1/orgs/{id}/members`    | Add a user to an organization with an organization role. Organization admin grants manage over that organization's projects, templates, inventories and credentials, bounded by the account's global role: on a viewer account it confers use, not manage. See [concepts](concepts.md). |
| GET    | `/v1/orgs/{id}/members`    | List an organization's members and their roles.         |
| DELETE | `/v1/orgs/{id}/members/{userID}` | Remove a user from an organization.               |
| POST   | `/v1/grants`               | Grant a user or team read, use, or manage on an object: a project, template, inventory, or credential id, or a worker queue as `queue:<name>`. |
| GET    | `/v1/grants`               | List access grants.                                     |
| DELETE | `/v1/grants/{id}`          | Delete an access grant.                                 |
| GET    | `/v1/workers`              | The executor fleet with lease freshness.                |
| POST   | `/v1/inventory-sources`    | Register a dynamic inventory source.                    |
| GET    | `/v1/inventory-sources`    | List inventory sources.                                 |
| POST   | `/v1/inventory-sources/{id}/refresh` | Refresh a source into its inventory now.      |
| PUT    | `/v1/inventory-sources/{id}` | Update an inventory source.                           |
| DELETE | `/v1/inventory-sources/{id}` | Delete an inventory source.                           |
| POST   | `/v1/inventories`          | Store an inventory. Runs reference it by id anywhere.   |
| GET    | `/v1/inventories`          | List stored inventories.                                |
| PUT    | `/v1/inventories/{id}`     | Update a stored inventory.                              |
| DELETE | `/v1/inventories/{id}`     | Delete a stored inventory, with every fact cached for its hosts. |
| GET    | `/v1/inventories/{id}/facts` | List the inventory's hosts with cached facts: size, gathering run, and time. Operator role. |
| GET    | `/v1/inventories/{id}/facts/{host}` | Read one host's cached facts. Secret-looking values are masked below admin. Operator role. |
| DELETE | `/v1/inventories/{id}/facts/{host}` | Clear one host's cached facts, so the next run gathers them afresh. |
| POST   | `/v1/inventories/preview`  | Resolve an unsaved smart or constructed inventory and list the hosts it reaches for the caller, with the `engine` that resolved it and the `ansible_core` version when Ansible did. |
| POST   | `/v1/inventories/{id}/preview` | Resolve a stored smart or constructed inventory and list the hosts a launch by the caller would reach, with the same `engine` and `ansible_core`. |
| POST   | `/v1/policies`             | Create a policy that holds, denies, or exempts matching runs. `effect` is `require_approval`, the default, `deny`, or `exempt`. An exempt policy lets the agent runs it matches go ahead without the built-in agent hold, takes only `tool`, `command_contains`, `inventory_id`, `queue`, `actor`, and `actor_kind: agent`, and is Community. See [Agent runs are held by default](policy.md#agent-runs-are-held-by-default). |
| GET    | `/v1/policies`             | List approval policies. A Rego policy from the policy file carries a `rego` object with its `package`, `syntax`, bundle `sha256`, `modules`, `warn` (`hold` or `note`), and `timeout`. See [Approval policies](policy.md). |
| PUT    | `/v1/policies/{id}`        | Update an approval policy.                              |
| DELETE | `/v1/policies/{id}`        | Delete an approval policy.                              |
| POST   | `/v1/import/{format}`      | Import an AWX, Semaphore, Chef, Puppet, Rundeck, or Jenkins export. Format is awx, semaphore, chef, puppet, rundeck, or jenkins; any other format is refused. Chef and Puppet bring a fleet as an inventory rather than job definitions, and take no `?inventory=`. Rundeck and Jenkins take `?inventory=` to say which hosts their jobs target, since neither brings an inventory. Two formats accept a zip: the Jenkins body is one `config.xml` or a zip of a jobs directory, and the Rundeck body is a job export or a project archive, each told apart by content. The body is capped at 25 MiB and a larger one is refused with 413, which is lower than the CLI, where a project archive is bounded only by the archive reader's own limits. Previews by default; `?apply=true` writes the plan. A crontab imports from the CLI only, with `switchtender import cron`. Which objects each format carries is in [what each source brings over](migration.md#what-each-source-brings-over).|
| GET    | `/v1/audit`                | A page of the mutation trail, admin only. `?limit=` up to 1000, default 100; `has_more` reports whether older entries remain. |
| GET    | `/v1/audit/register`       | The change register as a self-contained HTML document, admin only. |
| GET    | `/v1/doctor`               | Install health checks and their findings, admin only, with the server's ansible-core version and the releases the inventory engine is tested against under `ansible`. |
| GET    | `/v1/audit/verify`         | Verify the audit hash chain is intact.                  |
| GET    | `/v1/audit/bundle`         | The audit chain as a signed LoomSeal bundle, verifiable offline or on the /verify page. |
| GET    | `/metrics`              | Prometheus series: run, fleet, queue-depth, and worker gauges, plus a run-duration histogram. |
| GET    | `/healthz`              | Liveness.                                               |
| GET    | `/readyz`               | Readiness: 200 once the store answers, 503 while it does not. |

A streamed export whose status line has already been sent cannot report a later failure with a
status code. The run event NDJSON download and the run log download therefore end with a
`{"export_incomplete":true,"reason":"..."}` line when they stop early, so a short file is never
mistaken for a whole one.

### When a run needs a credential that is not ready

A launch, a rerun, a retry, a relaunch, and a webhook answer `409` when the run needs a credential
that has no secret yet, which every imported credential is until someone sets one, or a credential
whose sealed secret does not open under this server's encryption key and salt. The error names the
credential and the fix: set its secret, or restore the key and salt it was sealed with.

### How long a submission can take

A submission answers once the approval gate has judged the run, and for a Terraform or OpenTofu
dry run whose configuration calls registry or remote modules, the gate downloads those modules
first. `POST /v1/runs`, a template launch, a rerun, and an agent's submission through MCP wait for
that download, which `--module-fetch-timeout` bounds at two minutes by default, and then for the
scan, so allow a client that long before it gives up. A download that runs out of time does not
fail the submission: the plan is recorded as unclassified, and held or refused like any other plan
the gate could not read in full. A webhook does not wait that long, since its sender would stop
listening first: `POST /hooks/{token}` answers within five seconds, with `accepted` when the launch
is still going.

### When the chain itself refuses a bundle

`GET /v1/audit/bundle` recomputes the whole chain and holds it against every anchor recorded over it
before any window is applied. A chain this server checked and rejected is a finding, not a fault, so
it answers `409` rather than `500`, and a caller can tell the two apart without reading prose. A
`500` is left to a real fault here, such as a store that will not read, and a `limit` that is not a
count stays a `400`.

    {
      "error":  "entry 3 does not recompute (sequence 3)",
      "reason": "chain_break",
      "broke_at": 3,
      "broke_seq": 3,
      "count": 9
    }

| `reason` | Meaning |
|----------|---------|
| `chain_break` | An entry does not recompute. `broke_at` is its one-based position and `broke_seq` its chain sequence, both zero when the entry carries no readable sequence, which is itself a shape tampering takes. |
| `anchor_unsatisfied` | Every entry recomputes, but the chain no longer satisfies an anchor recorded over it, which is how a missing tail shows up. `anchor_problems` names each one. |
| `chain_unbundlable` | The chain verifies but no bundle can be formed over it. |

The coordinates are the same ones `GET /v1/audit/verify` reports, so the two answers agree. A
windowed request is refused for a break anywhere in the chain, not only inside the window: a bundle
signed over a window sitting past a break would attest to entries this install cannot stand behind.


## Opening a live stream

`EventSource` cannot set a header, so `GET /v1/runs/{id}/stream` takes a short-lived ticket in the
query string instead of a bearer token. Mint one over the ordinary authenticated route and open the
stream with it:

    curl -X POST -H "Authorization: Bearer $ST_TOKEN" \
      localhost:8080/v1/runs/run_abc/stream-ticket
    # {"ticket":"...","expires_in":30}

A ticket opens that one run, works once, and expires in thirty seconds. The reason is that a URL is
not private: a reverse proxy logs the full request line by default, so a session token in the query
string reaches every access log downstream. A ticket in the same place is worth almost nothing.

An install running open, with no tokens, needs no ticket and the plain path works.

## Account profiles

An account carries an optional profile alongside its role: `full_name`, `email`, `phone`, `title`,
`links`, and `notes`. All of them are optional, so an account created by the CLI or provisioned over
single sign-on stays valid with none of them set. `title` is descriptive and grants nothing; `role`
alone decides what an account may do.

```bash
curl -X PUT https://switchtender.example.com/v1/users/user_9f2c \
  -H "Authorization: Bearer $ST_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "username": "ada",
    "role": "operator",
    "full_name": "Ada Lovelace",
    "email": "ada@example.com",
    "phone": "+1 555 0100",
    "title": "Platform Engineer",
    "links": ["https://wiki.example.com/people/ada"],
    "notes": "review each quarter"
  }'
```

The profile is replaced wholesale on update, so send the profile you want to end up with rather than
only the parts that changed. An omitted field clears.

A profile is personal data and is treated as such. Only an admin may read it. `/v1/users` requires
the admin role and is not delegable by a manage grant. The values are never written to the logs, and
a rejection names the offending field without echoing it. Each single-line field is capped at 320
characters, `notes` at 2000, and an account may carry at most eight links. A link must be an `http`
or `https` address. Any other scheme is refused, because the admin page renders links as anchors.

## Template run timeout

A template may cap how long its launches are allowed to execute with `timeout`, a whole number of
seconds. It is accepted on create and update, and every launch of the template carries it onto the
run, whether the launch came from the API, a schedule, or a webhook trigger.

```bash
curl -X POST https://switchtender.example.com/v1/templates \
  -H "Authorization: Bearer $ST_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "nightly database backup",
    "playbook": "plays/backup.yml",
    "timeout": 5400
  }'
```

Zero, or the field omitted, leaves launches on the server default set by `--run-timeout`, so a
template saved before this field existed is unchanged. A run that exceeds its timeout is canceled
and finalized as failed. A launch cannot raise the cap. The template's value is what applies.

## Naming what a run targets

Two fields name a target and they are not interchangeable.

`inventory_id` names a stored inventory, the kind the UI creates and the one almost every
caller wants. `inventory` is a path to an inventory file already on the server, for a run whose
inventory is managed outside this product.

Sending a stored inventory's name in `inventory` is read as a path. Ansible exits zero when a
host pattern matches nothing, so a run aimed at a path that does not exist is recorded as
succeeded having touched no host at all.

## Ansible run controls

A run submission and a template both accept the Ansible controls that used to require a hand-built
command. They ride onto a run the same way from the API, a schedule, or a webhook trigger, and a
retry keeps them.

| Field | Type | What it does |
|-------|------|--------------|
| `limit` | string | Narrows the run to the hosts matching this pattern. Becomes `--limit`. Empty targets the whole inventory. |
| `tags` | list of strings | Runs only the plays and tasks carrying one of these tags. Becomes `--tags`. |
| `skip_tags` | list of strings | Skips the plays and tasks carrying one of these tags. Becomes `--skip-tags`. |
| `forks` | integer | How many hosts Ansible addresses at once. Zero leaves the Ansible default. Becomes `--forks`. |
| `verbosity` | integer 0 to 4 | Raises Ansible logging. One through four becomes `-v` through `-vvvv`; a higher number is clamped to four. |
| `diff_mode` | boolean | Shows the before and after of every changed file and template. Becomes `--diff`. |

```bash
curl -X POST https://switchtender.example.com/v1/runs \
  -H "Authorization: Bearer $ST_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "playbook": "plays/deploy.yml",
    "inventory_id": "inv_3f9c1b7a2e04",
    "limit": "canary01",
    "tags": ["web", "config"],
    "skip_tags": ["reboot"],
    "forks": 25,
    "verbosity": 2,
    "diff_mode": true
  }'
```

These apply to the Ansible tool. The other tools ignore them, so a Bash or Terraform template that
carries one is unaffected.

A run submission also accepts `extra_vars`, an object of variables injected into the run. Ansible
receives them the way `--extra-vars` supplies them, and a plugin tool reads them as its input. A
template carries its own `extra_vars` and a launch may merge more over them.

## Saved workflows

A template may carry `steps`, a pipeline graph, instead of a single tool. Such a
template is a saved workflow: every path that fires a template, a launch, a schedule, or a webhook
trigger, runs the graph as a pipeline, and the template's survey answers and extra vars reach every
step. A workflow template sets no top-level `playbook`, `command`, `tool`, `shards`, or Ansible
controls, since each step names its own. The graph is validated when the template is saved, so a
cycle or an unknown dependency is refused then rather than on every launch.

A step with `"type": "approval"` is an approval step: the workflow waits there until an admin
approves or denies it. It takes a `description` and an `approval_timeout` in seconds, and a step
names it in `if_denied` to run when it is denied or times out. `GET /v1/approvals` lists the steps
waiting, and `POST /v1/runs/{id}/approve` or `/reject` with the step's id decides one, optionally
with `{"state_digest": "..."}` from the listing so the decision binds to what was shown. The same
call with the workflow's id is refused with 409, naming the step and its call. A workflow
reaching an approval step is announced with its own event, `workflow.step_awaiting_approval`, which
names the step and what each answer runs next, so a receiver can tell it from a whole run held for
approval. The [concepts page](concepts.md) describes the semantics.

```bash
curl -X POST https://switchtender.example.com/v1/templates   -H "Authorization: Bearer $ST_TOKEN"   -H 'Content-Type: application/json'   -d '{
    "name": "build and ship",
    "inventory_id": "inv_3f9c1b7a2e04",
    "steps": [
      {"name": "build", "tool": "bash", "command": "make release"},
      {"name": "deploy", "playbook": "deploy.yml", "depends_on": ["build"]}
    ]
  }'
```

## Approver reasons

An approval or a denial may carry the approver's reason, for a held run and a workflow approval
step alike. It is optional unless a policy's [`require_reason`](policy.md#requiring-the-approvers-reason)
asks for one, and it holds at most 1,000 characters.

```bash
curl -X POST https://switchtender.example.com/v1/runs/run_abc123/approve \
  -H "Authorization: Bearer $ST_TOKEN" -H 'Content-Type: application/json' \
  -d '{"reason": "approved, change window confirmed with the database team"}'
```

The reason passes through the secret masker before anything records it, the same masker run output
and pull request comments pass through. When the masker changes the text, nothing is recorded: the
answer is 409 with `masked_reason`, the text as it would be kept, and the decision goes through when
it is sent again with `masked_reason` set to exactly that text. A reason is never refused for looking
like a secret. The interface shows the masked text and asks the approver to confirm it, and labels
the field "Stored as audit evidence. Don't include secrets or personal data." with "Masked for known
secrets before storage. Treat it as permanent audit data." beneath it.

The masked text is stored with a random 32-byte value beside the decision. The chain entry for the
decision commits a hiding commitment to it, never the text: `sha256:` and the hex SHA-256 of the
RFC 8785 canonical JSON object `{"event": <decision id>, "random": <hex of the random value>,
"reason": <masked text>}`, where the decision id is the id of the chain entry that records the
decision and is committed in its body as `decision_id`. The request that carried the reason is
recorded with the reason withheld from its fingerprint, so the text as typed is committed nowhere.

`GET /v1/runs/{id}/decisions` lists the run's decisions with their reasons and random values, the
separation-of-duties evaluation of a decision on an agent's run, corrections, and redactions. The
evidence dossier and a contiguous receipt carry the same text and random value beside each decision,
so anybody holding the receipt recomputes the commitment offline with no secret, and
`switchtender verify` does exactly that and fails a receipt whose reason does not open it.

A reason is never edited. `POST /v1/runs/{id}/decisions/{decision}/corrections` with
`{"text": "..."}` appends a correction, masked and committed the same way under its own chain entry,
and the dossier and the interface show it under the decision it corrects. An admin may redact a
reason or a correction with `POST /v1/runs/{id}/decisions/{record}/redact` and
`{"category": "personal_data"}`, or `secret` or `other`. Redaction is a privacy action, not an edit:
it removes the text and the random value together and appends a chain entry recording who redacted
it, when, and the category, referencing the decision. The commitment stays on the chain and can never
be opened again. Receipts already issued keep what they disclosed.

A reason is audit data, subject to your organization's retention policy. It is held as long as its
run: when retention removes the run, it removes the decision records with it, and the commitment that
stays on the chain can no longer be opened. A rejected run's `error` reads "rejected by an approver"
and a denied step's "denied by an approver". The reason stays in the decision record, where a
redaction can reach it, and is never copied into a run's error or its committed outcome.

## Fact cache

A template may set `use_fact_cache`, the setting AWX calls the same thing. Every launch then writes
the facts earlier runs gathered for the inventory's hosts into a private directory that Ansible's
`jsonfile` cache plugin reads, and stores what the run gathers when it ends, so a play with
`gather_facts: false` still sees each host's facts. `fact_cache_timeout` is how many seconds
cached facts stay fresh enough to serve, and zero serves them however old they are, which is the
AWX default. Both need `inventory_id`, because facts are kept per stored inventory host, and both
are omitted-means-unchanged on an update.

```bash
curl -X PUT https://switchtender.example.com/v1/templates/tpl_9d41c2 \
  -H "Authorization: Bearer $ST_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "patch web",
    "playbook": "plays/patch.yml",
    "inventory_id": "inv_3f9c1b7a2e04",
    "use_fact_cache": true,
    "fact_cache_timeout": 86400
  }'
```

With the cache on, a run's spec records `"fact_cache": {"timeout_seconds": N}`. An approval binds to
it and a receipt discloses it, so a run whose fact cache setting changes after it was approved is
refused, and a held run shows the setting to its approver. With the cache off the field is absent,
so the digest of every such run is what it always was.

`GET /v1/inventories/{id}/facts` lists the cached hosts and `GET /v1/inventories/{id}/facts/{host}`
returns one host's document. Reading either takes the operator role and read on the inventory, and a
host whose gathering run the caller may not read is left out. A server started with
`--fact-cache-admin-only` answers 403 to anyone below admin. Cached facts never enter the audit
chain, a receipt, an evidence bundle, or a backup, and a document larger than one mebibyte is not
kept. See [the Ansible guide](tool-ansible.md#fact-cache) for how the cache behaves during a run.

## Provisioning callbacks

A template may set `allow_callbacks`, which lets a host in the template's stored inventory launch
the template against itself, usually from a boot script. The host posts the template's host config
key to `/v1/templates/{id}/callback`. The server matches the address the request came from to one
host in the inventory and launches the template limited to that host. The launch passes through the
approval policies like any other.

Mint the key once, and keep it, because it is never shown again:

```bash
curl -X POST https://switchtender.example.com/v1/templates/tpl_9d41c2/callback-key \
  -H "Authorization: Bearer $ST_TOKEN"
```

```json
{
  "template": "tpl_9d41c2",
  "host_config_key": "hck_...",
  "callback_path": "/v1/templates/tpl_9d41c2/callback"
}
```

The host then calls back the same way an AWX host does:

```bash
curl -k -f -i -H 'Content-Type: application/json' -X POST \
  -d '{"host_config_key": "hck_..."}' \
  https://switchtender.example.com/v1/templates/tpl_9d41c2/callback
```

A form body, `host_config_key=hck_...`, is accepted too. The answers are:

| Status | When |
|--------|------|
| 201 | The run was created. `Location` names it, and an approval policy may have held it. |
| 400 | No host, or more than one, matched the caller's address, the template's survey has a required question with no default (see [launches nobody answers](#launches-nobody-answers)), or the body carried `extra_vars`. |
| 403 | The key is wrong, the template does not accept callbacks, a deny rule refused the run, or the calling host is outside a limit the template keeps. An unknown AWX id, and a template with its AWX-compatible address off, answer the same as a wrong key. |
| 409 | A callback run for this host is still pending or running, or the template cannot launch from a callback, which includes a template whose inventory is resolved when a run starts: one read from a secret source or a command, and a smart or constructed inventory. It is also the answer when the template keeps its limit and this server cannot run `ansible-inventory` to check it. |
| 410 | On the AWX-compatible address only: the template that address reached was deleted. |
| 429 | Too many callbacks or wrong keys from this address in the last minute. The body names the limit and the flag that sets it, and `Retry-After` says when to try again. |
| 503 | The callback could not be recorded in the audit trail, or the template's limit could not be evaluated, so nothing was launched. |

Turning `allow_callbacks` off revokes the key. A template also carries `callback_limit`, which says
what a callback does with the template's `limit`: `intersect`, the default, launches only for a
calling host the limit selects, and `replace` launches for any matched host, the way AWX does. Both
are omitted-means-unchanged on an update.

A template an import bound to its AWX job template id is served with `awx_job_template_id` and, once
a host has called through the AWX-compatible address, `awx_callback_called_at`. Neither can be set.
`awx_callback` turns that address on or off, and can be turned on only for a template an import
bound. See [the Ansible guide](tool-ansible.md#provisioning-callbacks) for how a host is matched and
how the launch is recorded, and [the migration
guide](migration.md#the-awx-compatible-callback-address) for the AWX-compatible address.

### What a request may carry

Text holding the NUL byte, or text that is not valid UTF-8, in the request path or a query parameter
is refused with `400` before the request is routed, so it never reaches a store. A JSON body is held
to the same rule: a string holding the NUL character, written `\u0000`, is refused with `400` naming
the field, such as `steps[1].name`. An import is checked the same way before it writes anything: an
export holding such text in any object it would create is refused with `400` naming the object and
the field.

Some values are stored under an index and have a bound. Past it the request is refused with `400`
stating the bound.

| Value                                      | Bound     |
|--------------------------------------------|-----------|
| `Idempotency-Key` header                   | 255 bytes |
| A run's, template's, or inventory's queue  | 255 bytes |
| Username                                   | 255 bytes |
| Token name                                 | 255 bytes |
| The id a grant or a membership names       | 512 bytes |

## List responses

Every list response is an envelope: the rows under a name, `count` for how many were returned, and
`total` for how many exist. They agree on any ordinary install.

The run list pages, because run history grows without bound: `limit` (default 200, maximum 1000) and
`offset`, with `has_more` and `next_offset` to continue. The configuration lists, users, tokens,
templates, schedules, triggers and organizations, return at most 1000 rows in one response. Past
that, `total` exceeds `count` and the response is the first 1000. An install with more configuration
than that should read it through the object endpoints rather than the list.

## Survey field constraints

A template survey field accepts bounds beyond its type, checked at launch before any answer becomes
an extra var. A field also takes an optional `help` string shown beneath its prompt, and a
`multiline` type for a block of text such as a set of variables or a note.

| Field kind | Constraints |
|------------|-------------|
| `int` | `min` and `max` bound the answer, inclusive. |
| `text`, `multiline`, `secret` | `min_length` and `max_length` bound the length, and `pattern` is a regular expression the whole answer must match. |
| `choice` | The answer must be one of `choices`. |

A launch that violates a constraint is refused with the field it failed, and no run is submitted.

The shape itself, which strict decoding refuses to guess at:

```json
{
  "survey": [
    {"var": "release", "label": "Release tag", "type": "text",
     "required": true, "pattern": "^v[0-9]+\\.[0-9]+\\.[0-9]+$",
     "help": "The tag to deploy, such as v2.1.0"},
    {"var": "batch", "label": "Hosts per batch", "type": "int",
     "default": 5, "min": 1, "max": 50},
    {"var": "environment", "label": "Environment", "type": "choice",
     "required": true, "choices": ["staging", "production"]},
    {"var": "notes", "label": "Change notes", "type": "multiline",
     "max_length": 2000}
  ]
}
```

`type` is one of `text`, `multiline`, `secret`, `int`, `choice`, or `bool`, and `integer`, `boolean`,
`string`, `textarea`, and `password` are read as `int`, `bool`, `text`, `multiline`, and `secret`.
`var` names the extra var the
answer becomes, and it is the only field besides `type` that every entry must carry. An unknown key
is refused rather than ignored, so a survey that almost parses is reported instead of silently
losing a field.

A launch answers the survey under `answers`, keyed by each field's `var`, with an `int` answered as a
number and a `bool` as `true` or `false`:

```json
{"answers": {"release": "v2.1.0", "batch": 10, "environment": "staging"}}
```

### Secret fields

A `secret` field asks for a password, a token, or any other answer that must not be kept as text.
AWX calls the same thing a password field. The answer is checked against the field's bounds like a
text answer, then sealed with the credential key before anything is stored, the same AES-256-GCM
sealing a credential's secret gets. What follows from that:

- The run keeps only the sealed answer. Its `extra_vars` never hold it, and a run read through the
  API, the UI, an export, or an agent's MCP tools lists the variable under `sealed_vars` by name
  only, which says an answer was supplied and nothing about what it was.
- The executor opens the answer inside the execution that uses it and hands it to the tool the way
  every other answer arrives: as an Ansible extra var, in `SWITCHTENDER_VARS` and its own
  `SWITCHTENDER_VAR_<name>` entry for a script, or as a `TF_VAR_` entry for Terraform and OpenTofu.
  A secret answer of the same name wins over a template extra var.
- The answer is masked in the run's log and its events wherever the tool prints it, whatever the
  variable is called, the way a credential's secret is. A value shorter than four characters is
  not masked, the same floor credentials have.
- The audit chain, the receipt, the dossier, and the SIEM stream record that a secret answer was
  supplied, by variable name in the run's spec, and never its value or its sealed form.
- An approval covers which sealed answer runs, not only that one was given. A run created with a
  secret answer lists, under `sealed_var_digests`, the SHA-256 of each answer's sealed form, fixed
  when the run is created and part of the spec an approval binds and a receipt discloses. A digest of
  ciphertext sealed under the server's key says nothing about the answer. The executor opens a
  sealed answer only while it still matches its digest, so an answer swapped for another under the
  same name after the run was approved fails the run with the reason and never reaches the tool. A
  run created before digests existed carries none and keeps the spec it had.
- Launching a template that asks a secret question needs `SWITCHTENDER_ENCRYPTION_KEY`. Without one
  the launch is refused rather than storing the answer as text.
- A relay worker receives a sealed answer only when its pool registered a delivery key: the control
  node opens the answer at claim and seals it, with the run's credentials, to that pool's key, bound
  to the claim. A run carrying one on a pool with no key fails on the relay worker with the reason
  instead of running with the answer missing. See
  [Delivering secrets to relay workers](configuration.md#delivering-secrets-to-relay-workers).

A secret field may carry a `default`. It is sealed when the template is saved, and every read of the
template, an admin's included, shows it as `"[redacted]"` to say that a default is set. Send that
mask back unchanged on an edit and the stored default is kept, send a new value to replace it, or
send an empty string to clear it. A launch that leaves the field unanswered uses the sealed default
without it ever being opened outside the execution. In the launch dialog the field is a password
input that is never filled in, and leaving it empty is what uses the default.

A relaunch, a rerun, a retry, a shard of a split, and every step of a saved workflow carry the
sealed answer of the run they come from, which is what AWX does on relaunch, held to the digest that
run was created with. Retention removes the
sealed answer with the run it belongs to. An agent may answer a secret question on a template it is
allowed to launch, and no caller, agent or person, can read an answer back.

### Launches nobody answers

A schedule, a webhook trigger, a pull request plan, and a provisioning callback launch a template
with nobody there to answer its survey, so each one fills it in the same way:

- Every question takes its default. A plain question's default becomes an extra var, over a
  template extra var of the same name, and a secret question's sealed default rides onto the run
  still sealed, in place of any template extra var of that name.
- A required question takes its default too, and the default is checked against the question's
  rules the way an answer is. An empty default does not count as one.
- A required question with no default, or with a default its rules refuse, has no answer, so the
  launch is refused and no run is created. The refusal names every such question and says to give
  it a default or launch the template by hand.

Where the refusal is recorded depends on what fired:

| Launch | Recorded on | Chain entry |
|--------|-------------|-------------|
| Schedule | The schedule's `last_error`, shown as "did not run" in the schedules list. | `SCHEDULE /schedules/{id}/refused/survey/{questions}` |
| Webhook trigger | The answer to the sender, a 409, and the trigger's `last_error` and `last_error_at`. | `POST /hooks/{trigger}/refused/survey/{questions}` |
| Pull request plan | The pull request's comment and status, and the trigger's `last_error`. | `POST /hooks/{trigger}/review/{number}/refused/survey/{questions}` |
| Provisioning callback | The answer to the host, a 400. | None, as for every callback refused before a host is matched. |

The questions are the variable names, comma separated. The refusal is recorded in place of a fire,
so the trail never shows a fire that ran nothing. On a push trigger, `last_error` says why the most
recent delivery started no run, whatever the reason. On a review trigger it records a plan refused
for its survey, and the pull request hears about every other refusal. Either clears when a delivery
starts a run. The Doctor reports an enabled schedule whose template cannot be answered as broken,
before its first fire.

## Per-template notifications

A template or a run submission may carry `notifications`, a list of targets that receive its
terminal state in addition to the server-wide channels, and hear when a rule holds it for approval.
Each target names a `kind` and the field that kind is addressed by, plus an optional `on_failure`
that limits the target to failed runs, which also leaves it out of holds. A hold reaches webhook,
chat, ntfy, and email targets, and never a PagerDuty, Grafana, or Twilio one: a hold asks for a
decision, and those report incidents.

| Kind | Required fields | What is sent |
|------|-----------------|--------------|
| `webhook` | `url` | The run as JSON with its `event`, extra vars redacted. |
| `slack`, `mattermost`, `rocketchat` | `url` | A message to the incoming webhook. |
| `discord`, `teams` | `url` | A message or Adaptive Card to the webhook. |
| `ntfy` | `url` | A notification to the topic, raised priority on failure. |
| `pagerduty` | `key` | An incident trigger on the routing key, failed and interrupted runs only. |
| `grafana` | `url`, `key` | An annotation to that instance's annotations API with that token. |
| `twilio` | `to` | An SMS to that recipient through the server-held Twilio account. |
| `email` | `to` | Mail to that comma-separated recipient list through the server SMTP transport. |

```bash
curl -X POST https://switchtender.example.com/v1/templates \
  -H "Authorization: Bearer $ST_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "prod deploy",
    "playbook": "plays/deploy.yml",
    "notifications": [
      {"kind": "slack", "url": "https://hooks.slack.com/services/T0/B1/x"},
      {"kind": "pagerduty", "key": "R0UTINGKEY", "on_failure": true},
      {"kind": "email", "to": "oncall@example.com, lead@example.com"}
    ]
  }'
```

A webhook, a target or one named with `--notify-webhook`, receives one body shape:
`{"event": "run.finished", "run": {...}}` when a run reaches a terminal state, and the same with
`"event": "run.held"` when a rule holds it for a person to decide on. Two more events use the same
shape:

| Event | When | What the `run` carries |
|-------|------|------------------------|
| `workflow.step_awaiting_approval` | A workflow reaches an approval step and waits for a person. | The workflow, with `awaiting_step`: the step's `id`, which approve and reject take, its `name` and `description`, `on_approve` and `on_deny` naming the steps each answer runs next, and `expires_at` when it times out. |
| `run.needs_attention` | Work has needed attention past its alert threshold. See [What needs attention](#what-needs-attention). | The run, with `attention`: the alert's `id`, its `blocker`, a one-line `summary`, the `reason`, `who_can_act`, `next`, `since`, `waiting_seconds`, `threshold_seconds`, and the interface `path` that lists it. A schedule held back by the run is named in `schedule_id` and `schedule_name`. |

The chat channels, ntfy, and email say the same in words: a step's message names the workflow, the
step, what it asks, and what approving and denying each run, and an alert leads with its summary
and who can act. A step waiting reaches the same channels a hold does. An alert reaches the chat
channels, webhooks, ntfy, and email, and is emailed whatever `--notify-on` says, since it reports a
problem. A run's own PagerDuty, Grafana, and Twilio targets hear neither, the same rule holds
follow.

A malformed target is refused at create or update with the field it lacks, not dropped at
delivery. A Twilio or email target names only a recipient. The account credentials stay in server
flags, so a template never carries them. On read, webhook URLs, PagerDuty routing keys, and
Grafana tokens come back masked. An edit that echoes the mask back keeps the stored value.

## Notification targets

A notification target is a channel defined once and attached to many objects, the way AWX attaches
a notification template. It takes the same kinds and fields as a
[per-template notification](#per-template-notifications): `webhook`, `slack`, `mattermost`,
`rocketchat`, `discord`, `teams`, and `ntfy` with a `url`, `pagerduty` with a `key`, `grafana` with
a `url` and a `key`, and `twilio` and `email` with a `to`. Each target has a `name`, an optional
`description`, and an optional `org_id` whose members may use it.

```bash
curl -X POST https://switchtender.example.com/v1/notifications \
  -H "Authorization: Bearer $ST_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name": "ops slack", "kind": "slack", "url": "https://hooks.slack.com/services/T0/B1/x"}'
```

The `url` and `key` are sealed with the install's encryption key before they are stored, so
creating a target that carries either needs `SWITCHTENDER_ENCRYPTION_KEY` and
`SWITCHTENDER_ENCRYPTION_SALT` and is refused with 409 without them. Neither is ever returned. A
read answers `url` as a hint masked to its scheme and host, `key_set` when a key is stored, and
`needs_secret` for a target an import created without the secret it needs. An edit that sends a
blank or masked `url` or `key` keeps the stored one, so a target can be renamed without its secret
being entered again, and a stored secret never has to travel back through a form to be kept.

A read also carries `delivery`, the target's delivery status. Its `state` is `needs_secret` for a
target waiting for its secret, `configured` for one that has delivered nothing yet, `healthy` when
its latest finished delivery arrived, `retrying` while a failed attempt waits for its next one, and
`failing` when its latest finished delivery failed after its retries. Latest means the one that
finished last, so a delivery retried for minutes counts from when it finished, not from when it was
recorded. Beside it are `missing`, the times of the latest delivery and failure, the latest
failure's reason, and how many failed in the last seven days. A target waiting for its secret
keeps every part an import carried, such as a Grafana instance's address, and `missing` names what
it still lacks: `url`, `key`, or both. An edit that sends only those parts finishes it. An edit
that leaves one still missing is saved, and the target keeps waiting.

An attachment ties a target to an object for one event:

```bash
curl -X POST https://switchtender.example.com/v1/notifications/ntf_ab12cd34ef56/attachments \
  -H "Authorization: Bearer $ST_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"object_kind": "template", "object_id": "tpl_abc123", "event": "failure"}'
```

| Field | Values |
|-------|--------|
| `object_kind` | `template`, `workflow` (a template whose steps make a graph, stored as `template`), `schedule`, `project`, or `org` |
| `event` | `started` when a run begins, `success` when it succeeds, `failure` when it fails, is interrupted, is canceled, or is rejected, `approval` when a rule holds it for a person or a workflow waits at an approval step, `skipped` when a schedule's fire starts nothing because its inventory matched no hosts, and `attention` when it has needed attention past its alert threshold. AWX's `error` and `approvals` are read as `failure` and `approval`. |

When a top-level run reaches an event, every target attached for that event to the template it came
from, the schedule that fired it, its project, its organization, or the organization that owns its
template is told, once, however many of those it is attached through. The template is found through the run's source, so a target on a
template hears the runs its schedules and triggers fire, and a rerun's runs as well. A split's shards
and a pipeline's steps are announced through their parent rather than one by one.

A read that fails while the template is being found, of the schedule, trigger, or run that names it,
or of the template to learn its organization, counts as a database that cannot be read. The event
is asked for again and is not recorded for the other targets alone, as described under
[Delivery order](#delivery-order). A template that no longer exists is different: it has no targets
left to find, and asking again finds nothing more. Its targets are passed over, the others are told,
and the server logs a warning that names the run and what could not be found. That includes a
template whose schedule, trigger, or rerun origin was deleted after the run started.

A target is delivered with the same formatters and the same refusal of private addresses as every
other channel, and with `SWITCHTENDER_EGRESS_PROXY` set it leaves through that proxy, after its
address is checked, as every channel does. A hold reaches a PagerDuty, Grafana, or Twilio target
never, the same rule a template's own targets follow. The rule is applied to the target as it stands
when the message goes out, so a target changed to one of those kinds after a hold was recorded for
it skips the hold, and the delivery is recorded as skipped with why. A target attached for
`approval` hears a whole run held and a workflow waiting at an approval step alike, each as its own
event, `run.held` or `workflow.step_awaiting_approval`. A target attached for `attention` hears an
alert on every kind, a PagerDuty, Grafana, or Twilio target included, since attaching one for that
event is the request to be paged. A PagerDuty incident takes the alert's `id` as its dedup key, so
one condition opens one incident. A `started` event reaches a webhook as `"event": "run.started"`,
and a webhook target's body also carries `delivery`: `{"id": "ntf_ab12cd34ef56/run_x1y2/2",
"sequence": 2}`, with `note` added when an earlier message failed. The same `id` arrives as the
`Idempotency-Key` header.

### Delivery order

For each run and notification target, SwitchTender attempts delivery in the run's event order. A
failed notification does not prevent later ones from being attempted after its retries run out.

Each event is recorded as the run reaches it, with the run's next sequence number, in the same
transaction that queues one delivery per attached target. A database that refuses the write for a
moment, or cannot be read to find the targets, is asked again for up to five seconds. A run's end is
also marked owed in the same database write that ends the run, whatever ends it: the run's own
server, a worker's report, the lease sweep, a cancel, or a decision. Every server records an end
still owed a few seconds later, so a server that stops right after a run ends, or a database that
refused the end for longer, leaves the end late rather than lost. A run's end is recorded once,
however many times it is announced.

A run's start and its hold are marked owed the same way. A start is owed in the write that moves a
run into running, whether a worker claims it, a coordinator starts it, a decision starts it, or a
workflow resumes after an approval step. A hold is owed in the write that creates a run held, or
moves it into a hold from a state that was not one, including a workflow parked at an approval step.
A decision that takes a held run and lets it go again is not a new hold. Every server records a
start or hold still owed a few seconds later. A parked workflow's hold is recorded as each approval
step it waits at, the way the workflow announces them. A run's start, its own hold, and each
approval step's hold are recorded once, however many times they are announced.

When the sweep finds that the run has already moved past a start or hold that is still owed,
because it ended, was decided, or moved on, the event is not sent. Telling a target that a run
started after it ended, or asking a person to decide a run somebody already decided, would arrive
after the messages that followed it. The event is recorded on the run as skipped, with the reason,
for example `not sent: the run had already ended, failed, before its start could be announced`. It
shows at `GET /v1/runs/{id}/notifications` like every other skipped delivery.

A delivery waits until every earlier delivery to the same target for the same run has finished.
Targets never wait for each other: a server attempts at most four deliveries to one target at once
and keeps attempting the others' beside them, so a target that never answers holds only its own
share. Steps of a workflow that run beside each other are not put in an order between themselves: an
approval step's message follows the workflow's start and every step it comes after, and the
workflow's end follows all of them.

A failed attempt is tried four more times over about three minutes. A target that refuses a message
outright, with any 4xx but 408, 425, or 429, or a channel this server has no transport for, is not
retried. An attempt cut short because its server is stopping is not counted, and the next server to
claim the delivery makes it. When the attempts run out the delivery is kept as failed, and the next
message to that target about that run carries the note "An earlier notification about this run to
this target could not be delivered." The failure shows on the run at
`GET /v1/runs/{id}/notifications` and on its page, in the target's delivery status and at
`GET /v1/notifications/{id}/deliveries`, and in the doctor. A target waiting for its secret when an
event happens is recorded as skipped, with why.

Deliveries are idempotent on the target, the run, and the event's sequence number, so a retry, a
restart, or several servers delivering from one database never queue or claim the same delivery
twice. Events live in the database, so a delivery pending when a server stops is made by the next
process to start on it. Nothing further is promised: not the order a receiver sees messages in, not
that each arrives exactly once, and not that any arrives.

This is how named targets are delivered. The server-wide `--notify-*` channels and a template's own
`notifications` list keep their direct, best-effort delivery.

A schedule's skipped fire is announced the same way, to the targets attached for `skipped` to the
schedule, the template it fires, or its organization, and to the targets attached to the schedule
itself for `failure`. An AWX import never attaches anything for `skipped`, and whoever watches a
schedule's failures is who needs to know it stopped reaching hosts. A target attached only for
`started`, `success`, or `approval` does not hear it. Each channel says `skipped: no hosts matched`
and never failed, and like a hold a skip never reaches a PagerDuty, Grafana, or Twilio target. A
webhook receives `"event": "schedule.skipped"` with a run-shaped body that names the schedule as its
`source_id` and its inventory as `inventory_id`, carries `kind` `skipped_fire`, and has no `id`,
because no run was started.

A target's secrets are opened at delivery and never written onto the run, so they stay out of the
run record, its receipt, and its evidence, where a template's inline targets are stored on the run.
The server-wide channels and per-template notifications keep working beside targets.

Deleting a template, a workflow, a schedule, a project, or an organization removes its attachments
in the same transaction, and leaves the targets. The delete's own audit entry states the cleanup,
the count and the targets, in its path, such as
`/v1/templates/tpl_abc123?notification_attachments=2&notification_targets=ntf_a,ntf_b`, rather than
as entries of its own. A delete that finds the attachments changed after its entry was written
deletes nothing and answers 409. The doctor reports any attachment whose object or target is gone,
from whatever path it was left behind.

Attaching asks two things: use of the target, and management of the object, which is admin, or a
manage grant on a template or project, or admin of the organization that owns it. Attaching to a
schedule or an organization is admin work, the same as editing one. A grant on `ntf_<id>` scopes a
target the way a grant on a credential scopes it.

## What needs attention

`GET /v1/attention` answers the question an operator asks of work that is not moving: what is
stopping it. Every unfinished run, workflow, split, and schedule falls under at most one of four
answers, its main blocker, and `counts` says how many fall under each, so the four add up to the
items listed:

| Blocker | What it means | Who acts |
|---------|---------------|----------|
| `worker_lost` | The worker running it stopped renewing its lease, three renewals in a row. | Nobody, until the lease sweep is due to reclaim it. An admin when the reclaim is overdue. |
| `no_worker` | It is queued and no connected worker serves its queue. | An admin, by connecting a worker for the queue or moving the work. |
| `approval_needed` | A whole run held by a rule, a plan gate's apply among them, or a workflow waiting at an approval step. | An admin, never an agent, and not the requester when the rule requires a different approver. |
| `blocked` | It has waited past its threshold, 15 minutes by default, behind another run: every worker serving its queue is full, or a schedule's fires are skipped while a run it started is still going. | An admin, or whoever can finish or stop the run holding it. |

An item in several conditions at once, such as a workflow with one branch waiting at an approval
step while another branch's worker was lost, shows the first of them in the order above as its main
blocker, the rest under `badges`, and one line in `interaction` on how they bear on each other.
`since` and `waiting_seconds` measure the time in the current blocker rather than since the run was
created: a run approved after a long hold has waited for a worker only since the approval released
it, and one whose last worker went away has waited since that worker last reported. A retried
workflow step, a dependency, and a run holding a slot are given as the reason on an item, not
counted on their own.

Each item's `main` condition carries `reason`, `who_can_act`, and `next`. For a missing worker it
carries `queue` and `eligible_workers`, for a lost one `worker`, `last_seen`, and `reclaim_at`, the
automatic reclaim the view counts down to, for a blocked item `holder_run_id`, and for an approval
`approval`: whether it is a whole run or a `workflow_step`, for a step whether the workflow's
`other_branches_running` or it is `paused`, the `decision_id` approve and reject take, who may
approve, and `on_approve` and `on_deny` saying what each answer runs next.

`alert_after_seconds` is the threshold that applies to the item and `alerting` reports it has passed
it, which is also when the doctor lists it and when the attached targets are told, once per
condition, as a `run.needs_attention` event. The defaults alert on a lost worker only when the lease
sweep has not reclaimed its run within two lease periods, a minute, on a missing worker after 15
minutes, on a blocked item after 15 minutes, and never on an approval, since an approval waiting is
the gate working. `--attention-file` changes them for the organization, an organization on the
install, a queue, or a template. See [the configuration
reference](configuration.md#attention-thresholds). `thresholds` in the answer gives the install-wide
values in seconds, zero meaning off.

The view lists only what the caller may read, under the rules the run list and the schedule list
apply. There is no way to put a run back in the queue by hand from it. A worker that stopped
reporting may only be cut off from the store rather than gone, and a run requeued from under it
could execute twice, so the lease sweep's reclaim, which waits for the lease to expire first, is the
only way a lost worker's run moves.

```bash
curl -s "https://switchtender.example.com/v1/attention?blocker=no_worker" \
  -H "Authorization: Bearer $ST_TOKEN"
```

## Schedule timezone

A schedule reads its cron expression in its `timezone`, an IANA name such as `America/New_York` or
`Europe/Berlin`. With one set, `0 2 * * *` fires at 02:00 in that zone and follows its
daylight-saving shifts, so a nightly window stays put across the year. The field is accepted on
create and update and applies to the same expression the preview endpoint renders. A create or an
import that names no zone takes the server's local zone and writes its name onto the schedule, so
every server of a highly available group reads it alike, and the preview answers that zone as
`timezone`. A schedule an earlier release stored without a zone is read in UTC, and the first server
of this release to open the database writes `UTC` onto it.

`spring_forward` says what a schedule does with a time the clocks skip when they go forward: `jump`
runs it when the clock jumps (the default for `cron`), `later` runs it after the clock change, the
length of the jump later (the default for `rrule`, as AWX does), and `skip` skips that day. The preview takes the same parameter and
answers `spring_gap` for the next night it matters.

```bash
curl -X POST https://switchtender.example.com/v1/schedules \
  -H "Authorization: Bearer $ST_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"cron":"0 2 * * *","timezone":"America/New_York","template_id":"tpl_abc123"}'
```

## Schedule recurrence rules

A schedule may carry `rrule`, an RFC 5545 recurrence, in place of `cron`: a `DTSTART` and one or
more `RRULE` lines, with any `EXRULE`, `RDATE`, and `EXDATE` lines, separated by line breaks or
spaces. A `TZID` on the `DTSTART` sets the zone, and a create or update that sends no `timezone`
takes it from there. A schedule that sends both `cron` and `rrule`, a rule whose zone disagrees with
`timezone`, or a rule with no fire left after now is refused with 400 and a message naming the
fault.

```bash
curl -X POST https://switchtender.example.com/v1/schedules \
  -H "Authorization: Bearer $ST_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"rrule":"DTSTART;TZID=America/New_York:20260102T170000\nRRULE:FREQ=MONTHLY;BYMONTH=3,6,9,12;BYDAY=-1FR","template_id":"tpl_abc123"}'
```

`GET /v1/schedules/preview` takes `rrule` in place of `cron` and answers `next` with up to five
fires. A rule bounded by `COUNT` or `UNTIL` with fewer left answers what it has and `"finished":
true`, and a rule whose `DTSTART` names a zone answers that zone as `timezone`. The
[schedule tutorial](tutorial-schedule-a-job.md#schedule-what-cron-cannot-say) lists the parts a rule
takes and how daylight-saving changes are handled.

## Fleet view windows

`/v1/fleet` and `/v1/tasks` take a `window`, the number of recent runs per host or per task the
view considers, and `/v1/hosts/{host}/runs` takes a `limit`. All three default to 10. The window is
capped at 100 and the host history limit at 500. A larger value is answered with the cap, and the
response echoes the window it actually used. The caps exist because the per-host and per-task
summaries are kept when their runs are deleted, so on a long-lived fleet the tables hold a row for
every host of every run, and every row a window admits becomes an element of the answer.

The same tables are bounded by count rather than by age. `--retain-history` keeps the newest N
summaries for each host and each task and drops the rest, so a host's outcome history still
outlives its runs without the tables growing forever. N is never allowed below 500, the deepest
window these endpoints will answer, so trimmed history is history no request could have reached.

```bash
curl -s "https://switchtender.example.com/v1/fleet?window=30" \
  -H "Authorization: Bearer $ST_TOKEN"
```

## Relay endpoints

With `--worker-token` set, the server also serves the mesh relay under `/relay`: the execution
path an outbound worker started with `--server` uses instead of a database connection. Every call
presents the worker bearer token.

| Method | Path                               | What                                          |
|--------|------------------------------------|-----------------------------------------------|
| GET    | `/relay/v1/policies`               | Read the approval policies in force.          |
| POST   | `/relay/v1/claim`                  | Lease the oldest pending run for the caller, with its secrets sealed to the pool's delivery key when the pool registered one. |
| POST   | `/relay/v1/heartbeat`              | Renew the lease on a run.                     |
| POST   | `/relay/v1/runs/{id}/start`        | Fence the run from pending to running as it begins.           |
| GET    | `/relay/v1/runs/{id}`              | Fetch one run.                                |
| POST   | `/relay/v1/runs/{id}/save`         | Save the run's state.                         |
| POST   | `/relay/v1/runs/{id}/log`          | Append captured output.                       |
| POST   | `/relay/v1/runs/{id}/events`       | Append structured events.                     |
| POST   | `/relay/v1/runs/{id}/propose-apply`| Report a plan's findings so the control node holds its apply. |
| POST   | `/relay/v1/runs/{id}/drift-plan`   | Hand over the plan a drift check saved, which the control node seals and keeps for a reconcile. |
| POST   | `/relay/v1/runs/{id}/host-summary` | Save the run's per-host summaries.            |
| POST   | `/relay/v1/runs/{id}/host-facts`   | Save the facts the run gathered per host.     |
| POST   | `/relay/v1/runs/{id}/task-summary` | Save the run's per-task summaries.            |

Each report call is bounded twice. It presents the per-claim capability the claim response issued,
so it can only write to the run this worker holds, and one call carries at most a few thousand
items, so a worker cannot force an unbounded decode on the control node. The worker sends a wide
run's evidence in several calls rather than losing it to that cap. Host facts are bounded further: a
worker may write facts only for hosts its run has already reported results for, so nothing can be
recorded about a machine no run claims to have touched.

What that does and does not give you: a worker authors its own results, so a worker you do not trust can
still describe its own run untruthfully. What it cannot do is reach past that run into the recorded state
of the rest of the fleet. Give a queue only to workers you would let touch the hosts that queue targets.
