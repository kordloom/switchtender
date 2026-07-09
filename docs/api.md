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
| POST   | `/runs`                 | Submit a run; `shards` of two or more splits it.        |
| GET    | `/runs`                 | Run history, newest first.                              |
| GET    | `/runs/{id}`            | One run.                                                |
| POST   | `/runs/{id}/cancel`     | Cancel a pending or running run.                        |
| POST   | `/runs/{id}/retry`      | New split from only the failed shards of a finished one.|
| GET    | `/runs/{id}/shards`     | Shard runs of a split.                                  |
| GET    | `/runs/{id}/steps`      | Step runs of a pipeline.                                |
| GET    | `/runs/{id}/logs`       | Captured output as plain text.                          |
| GET    | `/runs/{id}/events`     | Structured events as JSON.                              |
| GET    | `/runs/{id}/stream`     | Live events and log over Server-Sent Events.            |
| POST   | `/pipelines`            | Submit ordered playbook steps as one pipeline.          |
| POST   | `/schedules`            | Cron schedule for a run, split, pipeline, or template.  |
| GET    | `/schedules`            | List schedules.                                         |
| GET    | `/schedules/{id}`       | One schedule.                                           |
| DELETE | `/schedules/{id}`       | Delete a schedule.                                      |
| GET    | `/fleet`                | Hosts ranked by failures over recent runs, flaky flags. |
| GET    | `/hosts/{host}/runs`    | One host's recent per-run outcomes.                     |
| GET    | `/tasks`                | Per-task duration trends over recent runs.              |
| POST   | `/projects`             | Register a git project; runs record their commit.       |
| GET    | `/projects`             | List projects.                                          |
| DELETE | `/projects/{id}`        | Delete a project.                                       |
| POST   | `/templates`            | Save a launch preset.                                   |
| GET    | `/templates`            | List templates.                                         |
| POST   | `/templates/{id}/launch`| Launch a template, answering its survey if it has one.  |
| DELETE | `/templates/{id}`       | Delete a template.                                      |
| POST   | `/triggers`             | Create a webhook trigger; returns a signing secret once.|
| PUT    | `/triggers/{id}`        | Rename a trigger or toggle signature enforcement.       |
| POST   | `/triggers/{id}/rotate-secret` | Rotate the signing secret, shown once.           |
| GET    | `/triggers`             | List webhook triggers.                                  |
| DELETE | `/triggers/{id}`        | Delete a trigger, revoking its webhook.                 |
| POST   | `/hooks/{token}`        | Fire a trigger from a git push; a required HMAC signature is checked first.|
| POST   | `/credentials`          | Store a credential (ssh_key, vault_password, env, become_password, registry), encrypted at rest.|
| GET    | `/credentials`          | List credentials, secrets never included.               |
| DELETE | `/credentials/{id}`     | Delete a credential.                                    |
| POST   | `/auth/login`           | Sign in with username and password, returns a token.    |
| POST   | `/auth/check`           | Verify an API token.                                    |
| POST   | `/users`                | Create an account with a role.                          |
| GET    | `/users`                | List accounts.                                          |
| DELETE | `/users/{id}`           | Delete an account; its tokens stop working.             |
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
| DELETE | `/inventory-sources/{id}` | Delete an inventory source.                           |
| POST   | `/inventories`          | Store an inventory; runs reference it by id anywhere.   |
| GET    | `/inventories`          | List stored inventories.                                |
| DELETE | `/inventories/{id}`     | Delete a stored inventory.                              |
| GET    | `/audit`                | The mutation trail, admin only.                         |
| GET    | `/metrics`              | Prometheus gauges for runs and fleet health.            |
| GET    | `/healthz`              | Liveness.                                               |
