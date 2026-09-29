<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# HTTP API

Every endpoint the server exposes. The API is served under the `/v1` base path. The web UI at
`/ui/`, along with `/healthz`, `/metrics`, the OpenID Connect and SAML sign-in routes, the webhook
`/hooks` path, and the `/relay` worker path, is unversioned. The root redirects to the UI.

| Method | Path                    | What                                                    |
|--------|-------------------------|---------------------------------------------------------|
| POST   | `/v1/runs`                 | Submit a run. `shards` of two or more splits it.        |
| GET    | `/v1/runs`                 | Run history, newest first.                              |
| GET    | `/v1/runs/{id}`            | One run.                                                |
| POST   | `/v1/runs/{id}/cancel`     | Cancel a pending or running run.                        |
| POST   | `/v1/runs/{id}/retry`      | New split from only the failed shards of a finished one.|
| POST   | `/v1/runs/{id}/approve`    | Release a run held for approval so it runs.             |
| POST   | `/v1/runs/{id}/reject`     | Deny a run held for approval.                           |
| GET    | `/v1/runs/{id}/shards`     | Shard runs of a split.                                  |
| GET    | `/v1/runs/{id}/steps`      | Step runs of a pipeline.                                |
| GET    | `/v1/runs/{id}/logs`       | Captured output as plain text.                          |
| GET    | `/v1/runs/{id}/events`     | Structured events as JSON.                              |
| GET    | `/v1/runs/{id}/stream`     | Live events and log over Server-Sent Events.            |
| POST   | `/v1/runs/{id}/explain`    | Advisory AI explanation of a run, when a provider is configured. |
| POST   | `/v1/ai/draft`             | Advisory AI draft of a bash, python, powershell, or go step script from a description. Operator role. |
| POST   | `/v1/ai/ask`               | Advisory AI answer to a fleet question, from run, health, and drift metadata. Rate limited. |
| POST   | `/v1/ai/propose-run`       | Turn a plain-language request into a run proposal, validated and held for approval. Operator role. |
| POST   | `/v1/drift/reconcile`      | Build a reconcile proposal for a drifted host, held for approval. Operator role. |
| POST   | `/v1/pipelines`            | Submit ordered playbook steps as one pipeline.          |
| POST   | `/v1/schedules`            | Cron schedule for a run, split, pipeline, or template.  |
| GET    | `/v1/schedules`            | List schedules.                                         |
| GET    | `/v1/schedules/{id}`       | One schedule.                                           |
| PUT    | `/v1/schedules/{id}`       | Update a schedule.                                      |
| DELETE | `/v1/schedules/{id}`       | Delete a schedule.                                      |
| GET    | `/v1/fleet`                | Hosts ranked by failures over recent runs, flaky flags. |
| GET    | `/v1/hosts/{host}/runs`    | One host's recent per-run outcomes.                     |
| GET    | `/v1/tasks`                | Per-task duration trends over recent runs.              |
| GET    | `/v1/drift`                | Resources drifting from desired state, from dry runs.   |
| POST   | `/v1/projects`             | Register a git project. Runs record their commit.       |
| GET    | `/v1/projects`             | List projects.                                          |
| PUT    | `/v1/projects/{id}`        | Update a project.                                       |
| DELETE | `/v1/projects/{id}`        | Delete a project. 409 while a template or source uses it.|
| POST   | `/v1/templates`            | Save a launch preset.                                   |
| GET    | `/v1/templates`            | List templates.                                         |
| POST   | `/v1/templates/{id}/launch`| Launch a template, answering its survey and choosing selectable credentials if it has them. |
| PUT    | `/v1/templates/{id}`       | Update a template.                                      |
| DELETE | `/v1/templates/{id}`       | Delete a template.                                      |
| POST   | `/v1/triggers`             | Create a webhook trigger, returns a signing secret once.|
| PUT    | `/v1/triggers/{id}`        | Rename a trigger or toggle signature enforcement.       |
| POST   | `/v1/triggers/{id}/rotate-secret` | Rotate the signing secret, shown once.           |
| GET    | `/v1/triggers`             | List webhook triggers.                                  |
| DELETE | `/v1/triggers/{id}`        | Delete a trigger, revoking its webhook.                 |
| POST   | `/hooks/{token}`        | Fire a trigger from a git push. A required HMAC signature is checked first.|
| POST   | `/v1/credentials`          | Store a credential, encrypted at rest. Thirteen kinds: ssh_key, ssh_password, vault_password, become_password, become, network, env, token, registry, aws, azure, gcp, vmware.|
| GET    | `/v1/credentials`          | List credentials, secrets never included.               |
| PUT    | `/v1/credentials/{id}`     | Update a credential.                                    |
| DELETE | `/v1/credentials/{id}`     | Delete a credential. 409 while an object still uses it. |
| POST   | `/v1/auth/login`           | Sign in with username and password, returns a token.    |
| POST   | `/v1/auth/check`           | Verify an API token.                                    |
| GET    | `/auth/oidc/login`      | Start the OpenID Connect sign-in handshake.             |
| GET    | `/auth/oidc/callback`   | Complete the OIDC handshake and issue a token.          |
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
| POST   | `/v1/orgs/{id}/members`    | Add a user to an organization with an organization role.|
| GET    | `/v1/orgs/{id}/members`    | List an organization's members and their roles.         |
| DELETE | `/v1/orgs/{id}/members/{userID}` | Remove a user from an organization.               |
| POST   | `/v1/grants`               | Grant a user or team read, use, or manage on an object. |
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
| DELETE | `/v1/inventories/{id}`     | Delete a stored inventory.                              |
| POST   | `/v1/policies`             | Create an approval policy that gates matching runs.     |
| GET    | `/v1/policies`             | List approval policies.                                 |
| PUT    | `/v1/policies/{id}`        | Update an approval policy.                              |
| DELETE | `/v1/policies/{id}`        | Delete an approval policy.                              |
| POST   | `/v1/import/{format}`      | Import an AWX or Semaphore export. Format is awx or semaphore.|
| GET    | `/v1/audit`                | The mutation trail, admin only.                         |
| GET    | `/v1/audit/verify`         | Verify the audit hash chain is intact.                  |
| GET    | `/v1/audit/export`         | Signed, self-verifying snapshot of the audit chain.     |
| GET    | `/metrics`              | Prometheus series: run, fleet, queue-depth, and worker gauges, plus a run-duration histogram. |
| GET    | `/healthz`              | Liveness.                                               |

## Account profiles

An account carries an optional profile alongside its role: `full_name`, `email`, `phone`, `title`,
`links`, and `notes`. All of them are optional, so an account created by the CLI or provisioned over
single sign-on stays valid with none of them set. `title` is descriptive and grants nothing; `role`
alone decides what an account may do.

```bash
curl -X PUT https://switchtender.example.com/v1/users/user_9f2c \
  -H "Authorization: Bearer $SWITCHTENDER_TOKEN" \
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

A profile is personal data and is treated as such. Only an admin may read it: `/v1/users` requires
the admin role and is not delegable by a manage grant. The values are never written to the logs, and
a rejection names the offending field without echoing it. Each single-line field is capped at 320
characters, `notes` at 2000, and an account may carry at most eight links. A link must be an `http`
or `https` address; any other scheme is refused, because the admin page renders links as anchors.

## Template run timeout

A template may cap how long its launches are allowed to execute with `timeout`, a whole number of
seconds. It is accepted on create and update, and every launch of the template carries it onto the
run, whether the launch came from the API, a schedule, or a webhook trigger.

```bash
curl -X POST https://switchtender.example.com/v1/templates \
  -H "Authorization: Bearer $SWITCHTENDER_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "nightly database backup",
    "playbook": "plays/backup.yml",
    "timeout": 5400
  }'
```

Zero, or the field omitted, leaves launches on the server default set by `--run-timeout`, so a
template saved before this field existed is unchanged. A run that exceeds its timeout is canceled
and finalized as failed. A launch cannot raise the cap; the template's value is what applies.

## Relay endpoints

With `--worker-token` set, the server also serves the mesh relay under `/relay`: the execution
path an outbound worker started with `--server` uses instead of a database connection. Every call
presents the worker bearer token.

| Method | Path                               | What                                          |
|--------|------------------------------------|-----------------------------------------------|
| GET    | `/relay/v1/policies`               | Read the approval policies in force.          |
| POST   | `/relay/v1/claim`                  | Lease the oldest pending run for the caller.  |
| POST   | `/relay/v1/heartbeat`              | Renew the lease on a run.                     |
| GET    | `/relay/v1/runs/{id}`              | Fetch one run.                                |
| POST   | `/relay/v1/runs/{id}/save`         | Save the run's state.                         |
| POST   | `/relay/v1/runs/{id}/log`          | Append captured output.                       |
| POST   | `/relay/v1/runs/{id}/events`       | Append structured events.                     |
| POST   | `/relay/v1/runs/{id}/host-summary` | Save the run's per-host summaries.            |
| POST   | `/relay/v1/runs/{id}/task-summary` | Save the run's per-task summaries.            |
