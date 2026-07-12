<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# HTTP API

Every endpoint the server exposes. The web UI lives at `/ui/` and the root redirects to it.

| Method | Path                    | What                                                    |
|--------|-------------------------|---------------------------------------------------------|
| POST   | `/runs`                 | Submit a run. `shards` of two or more splits it.        |
| GET    | `/runs`                 | Run history, newest first.                              |
| GET    | `/runs/{id}`            | One run.                                                |
| POST   | `/runs/{id}/cancel`     | Cancel a pending or running run.                        |
| POST   | `/runs/{id}/retry`      | New split from only the failed shards of a finished one.|
| POST   | `/runs/{id}/approve`    | Release a run held for approval so it runs.             |
| POST   | `/runs/{id}/reject`     | Deny a run held for approval.                           |
| GET    | `/runs/{id}/shards`     | Shard runs of a split.                                  |
| GET    | `/runs/{id}/steps`      | Step runs of a pipeline.                                |
| GET    | `/runs/{id}/logs`       | Captured output as plain text.                          |
| GET    | `/runs/{id}/events`     | Structured events as JSON.                              |
| GET    | `/runs/{id}/stream`     | Live events and log over Server-Sent Events.            |
| POST   | `/runs/{id}/explain`    | Advisory AI explanation of a run, when a provider is configured. |
| POST   | `/ai/draft`             | Advisory AI draft of a bash, python, or go step script from a description. Operator role. |
| POST   | `/ai/ask`               | Advisory AI answer to a fleet question, from run, health, and drift metadata. Rate limited. |
| POST   | `/drift/reconcile`      | Build a reconcile proposal for a drifted host, held for approval. Operator role. |
| POST   | `/pipelines`            | Submit ordered playbook steps as one pipeline.          |
| POST   | `/schedules`            | Cron schedule for a run, split, pipeline, or template.  |
| GET    | `/schedules`            | List schedules.                                         |
| GET    | `/schedules/{id}`       | One schedule.                                           |
| PUT    | `/schedules/{id}`       | Update a schedule.                                      |
| DELETE | `/schedules/{id}`       | Delete a schedule.                                      |
| GET    | `/fleet`                | Hosts ranked by failures over recent runs, flaky flags. |
| GET    | `/hosts/{host}/runs`    | One host's recent per-run outcomes.                     |
| GET    | `/tasks`                | Per-task duration trends over recent runs.              |
| GET    | `/drift`                | Resources drifting from desired state, from dry runs.   |
| POST   | `/projects`             | Register a git project. Runs record their commit.       |
| GET    | `/projects`             | List projects.                                          |
| PUT    | `/projects/{id}`        | Update a project.                                       |
| DELETE | `/projects/{id}`        | Delete a project. 409 while a template or source uses it.|
| POST   | `/templates`            | Save a launch preset.                                   |
| GET    | `/templates`            | List templates.                                         |
| POST   | `/templates/{id}/launch`| Launch a template, answering its survey if it has one.  |
| PUT    | `/templates/{id}`       | Update a template.                                      |
| DELETE | `/templates/{id}`       | Delete a template.                                      |
| POST   | `/triggers`             | Create a webhook trigger, returns a signing secret once.|
| PUT    | `/triggers/{id}`        | Rename a trigger or toggle signature enforcement.       |
| POST   | `/triggers/{id}/rotate-secret` | Rotate the signing secret, shown once.           |
| GET    | `/triggers`             | List webhook triggers.                                  |
| DELETE | `/triggers/{id}`        | Delete a trigger, revoking its webhook.                 |
| POST   | `/hooks/{token}`        | Fire a trigger from a git push. A required HMAC signature is checked first.|
| POST   | `/credentials`          | Store a credential (ssh_key, vault_password, env, token, become_password, registry), encrypted at rest.|
| GET    | `/credentials`          | List credentials, secrets never included.               |
| PUT    | `/credentials/{id}`     | Update a credential.                                    |
| DELETE | `/credentials/{id}`     | Delete a credential. 409 while an object still uses it. |
| POST   | `/auth/login`           | Sign in with username and password, returns a token.    |
| POST   | `/auth/check`           | Verify an API token.                                    |
| GET    | `/auth/oidc/login`      | Start the OpenID Connect sign-in handshake.             |
| GET    | `/auth/oidc/callback`   | Complete the OIDC handshake and issue a token.          |
| POST   | `/users`                | Create an account with a role.                          |
| GET    | `/users`                | List accounts.                                          |
| PUT    | `/users/{id}`           | Update an account's role or password.                   |
| DELETE | `/users/{id}`           | Delete an account. Its tokens stop working.             |
| POST   | `/teams`                | Create a team of users.                                 |
| GET    | `/teams`                | List teams.                                             |
| DELETE | `/teams/{id}`           | Delete a team and its memberships.                      |
| POST   | `/teams/{id}/members`   | Add a user to a team.                                   |
| GET    | `/teams/{id}/members`   | List a team's members.                                  |
| DELETE | `/teams/{id}/members/{userID}` | Remove a user from a team.                       |
| POST   | `/grants`               | Grant a user or team use or manage on an object.        |
| GET    | `/grants`               | List access grants.                                     |
| DELETE | `/grants/{id}`          | Delete an access grant.                                 |
| GET    | `/workers`              | The executor fleet with lease freshness.                |
| POST   | `/inventory-sources`    | Register a dynamic inventory source.                    |
| GET    | `/inventory-sources`    | List inventory sources.                                 |
| POST   | `/inventory-sources/{id}/refresh` | Refresh a source into its inventory now.      |
| PUT    | `/inventory-sources/{id}` | Update an inventory source.                           |
| DELETE | `/inventory-sources/{id}` | Delete an inventory source.                           |
| POST   | `/inventories`          | Store an inventory. Runs reference it by id anywhere.   |
| GET    | `/inventories`          | List stored inventories.                                |
| PUT    | `/inventories/{id}`     | Update a stored inventory.                              |
| DELETE | `/inventories/{id}`     | Delete a stored inventory.                              |
| POST   | `/policies`             | Create an approval policy that gates matching runs.     |
| GET    | `/policies`             | List approval policies.                                 |
| PUT    | `/policies/{id}`        | Update an approval policy.                              |
| DELETE | `/policies/{id}`        | Delete an approval policy.                              |
| POST   | `/import/{format}`      | Import an AWX or Semaphore export. Format is awx or semaphore.|
| GET    | `/audit`                | The mutation trail, admin only.                         |
| GET    | `/audit/verify`         | Verify the audit hash chain is intact.                  |
| GET    | `/audit/export`         | Signed, self-verifying snapshot of the audit chain.     |
| GET    | `/metrics`              | Prometheus gauges for runs and fleet health.            |
| GET    | `/healthz`              | Liveness.                                               |
