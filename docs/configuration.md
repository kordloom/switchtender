<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Configuration

SwitchTender is one binary with subcommands. This page lists every command, flag, and environment
variable.

## Environment variables

| Variable | Used by | Purpose |
|----------|---------|---------|
| `SWITCHTENDER_ENCRYPTION_KEY` | serve, worker | Passphrase that seals stored credentials with AES-256-GCM. Credentials are disabled when unset. |
| `SWITCHTENDER_ENCRYPTION_SALT` | serve, worker | Per-deployment salt for argon2id key derivation. Must be set alongside the key and stay stable across restarts, or stored credentials cannot be decrypted. Credentials are disabled when unset. |
| `SWITCHTENDER_AUDIT_KEY` | serve | Hex-encoded ed25519 seed that signs audit exports so the trail can be verified offline. Signing is off when unset. A malformed value stops startup. Generate one with `switchtender audit keygen`. |
| `SWITCHTENDER_PASSWORD` | user new | Initial account password, read instead of prompting so it never lands on the command line. |
| `SWITCHTENDER_SMTP_PASSWORD` | serve | Password for SMTP authentication when `--smtp-username` is set. |
| `SWITCHTENDER_AI_KEY` | serve | API key for a cloud AI provider such as Anthropic or an OpenAI-compatible endpoint. A local Ollama needs none. |
| `SWITCHTENDER_OIDC_CLIENT_SECRET` | serve | OpenID Connect client secret, paired with `--oidc-client-id`. Read from the environment so it stays off the command line. |
| `SWITCHTENDER_LDAP_PASSWORD` | serve | Password for the `--ldap-bind-dn` service account. |
| `SWITCHTENDER_ADMIN_PASSWORD` | init | Password for the first admin account. When unset, init generates one and prints it once. |
| `SWITCHTENDER_DESKTOP_NO_BROWSER` | desktop | Set to any value to skip opening the browser, for a headless or remote run. |

## init

Bootstraps a new deployment: it creates the database and the first admin account, writes an
environment file, and optionally a systemd unit. Run it once, then start `serve`.

| Flag | Default | Purpose |
|------|---------|---------|
| `--db` | `switchtender.db` | SQLite database path. |
| `--config` | `switchtender.env` | Environment file to write. |
| `--addr` | `:8080` | Address the server listens on. |
| `--admin` | `admin` | Username for the first admin account. |
| `--systemd` | none | Path to write a systemd unit to, empty to skip. |
| `--force` | `false` | Overwrite an existing config file. |

The admin password comes from `SWITCHTENDER_ADMIN_PASSWORD`, or is generated and printed once when
that variable is unset.

## serve

Runs the HTTP API, the in-process executor, the scheduler, the retention sweeper, and the web UI.

| Flag | Default | Purpose |
|------|---------|---------|
| `--addr` | `:8080` | Address the server listens on. |
| `--db` | `switchtender.db` | SQLite file path, or a `postgres://` DSN for the PostgreSQL backend. |
| `--tls-cert` | none | TLS certificate file, to serve HTTPS directly with no reverse proxy. Requires `--tls-key`. |
| `--tls-key` | none | TLS private key file. Requires `--tls-cert`. |
| `--oidc-issuer` | none | OpenID Connect issuer URL to enable single sign-on. Empty leaves SSO off. |
| `--oidc-client-id` | none | OIDC client id. |
| `--oidc-redirect-url` | none | OIDC redirect URL, for example `https://host/auth/oidc/callback`. |
| `--oidc-default-role` | `viewer` | Role granted to an account created on first SSO sign-in: admin, operator, or viewer. |
| `--ldap-url` | none | LDAP directory URL to enable directory sign-in, for example `ldaps://ldap.example.com:636`. |
| `--ldap-bind-dn` | none | Service account DN used to search for a user, empty for an anonymous search. |
| `--ldap-base-dn` | none | Search base for finding a user. |
| `--ldap-user-filter` | `(uid=%s)` | Search filter with one `%s` for the username. |
| `--ldap-default-role` | `viewer` | Role for an account created on first directory sign-in. |
| `--ldap-role-map` | none | Map a directory group to a role as `groupDN=role`. A matched group sets the role on every sign-in. Repeatable. |
| `--saml-idp-metadata-url` | none | SAML IdP metadata URL to enable SAML sign-in. Empty leaves SAML off. |
| `--saml-base-url` | none | Public base URL of this server, used to build the SAML entity id and ACS endpoint. |
| `--saml-cert` | none | Path to the service provider certificate, PEM. |
| `--saml-key` | none | Path to the service provider RSA private key, PEM. |
| `--saml-username-attr` | NameID | Assertion attribute used as the username. Empty uses the subject NameID. |
| `--saml-groups-attr` | `groups` | Assertion attribute holding the user's groups, used with `--saml-role-map`. |
| `--saml-default-role` | `viewer` | Role granted to an account created on first SAML sign-in. |
| `--saml-role-map` | none | Map an asserted group to a role as `group=role`. A matched group sets the role on every sign-in. Repeatable. |
| `--jwt-jwks-url` | none | JWKS URL to enable bearer JWT sign-in, so a service can present a JWT minted elsewhere. |
| `--jwt-issuer` | none | Expected token issuer, the `iss` claim. |
| `--jwt-audience` | none | Expected token audience, empty to skip the audience check. |
| `--jwt-username-claim` | `sub` | Claim naming the account. |
| `--jwt-groups-claim` | none | Claim holding the user's groups, used with `--jwt-role-map`. |
| `--jwt-role-map` | none | Map a token group to a role as `group=role`. Repeatable. |
| `--jwt-default-role` | `viewer` | Role granted to an account created on first JWT sign-in. |
| `--ai-provider` | none | Enable advisory AI features with a provider: `ollama`, `anthropic`, or `openai`. Empty leaves AI off. |
| `--ai-model` | provider default | Model name for the AI provider. Required for `openai`, which has no universal default. |
| `--ai-url` | provider default | Base URL for the AI provider, for a self-hosted Ollama, an OpenAI-compatible server, or a proxy. |
| `--schedule-interval` | `15s` | How often the scheduler checks for due schedules. |
| `--notify-webhook` | none | URL that receives a JSON notification when a run finishes. Repeatable. |
| `--notify-slack` | none | Slack incoming webhook URL that receives a message when a run finishes. Repeatable. |
| `--notify-discord` | none | Discord incoming webhook URL that receives a message when a run finishes. Repeatable. |
| `--notify-teams` | none | Microsoft Teams incoming webhook URL that receives an Adaptive Card when a run finishes. Repeatable. |
| `--allow-container-ee` | `false` | Allow runs whose project pins a container image to execute inside it. Needs Docker on the executor. |
| `--container-memory` | `2g` | Memory cap for containerized runs, as docker `--memory`. Empty removes the cap. |
| `--container-cpus` | `2` | CPU cap for containerized runs, as docker `--cpus`. Empty removes the cap. |
| `--container-pids-limit` | `2048` | Process cap for containerized runs, as docker `--pids-limit`. Zero removes the cap. |
| `--container-network` | `bridge` | Network mode for containerized runs, as docker `--network`, for example bridge or none. |
| `--strict-grants` | `false` | Deny non-admins access to an object that has no grants, instead of deferring to the global role. |
| `--read-only` | `false` | Reject every mutating request, for a safely exposable instance. |
| `--matrix-cap` | `50000` | Largest host matrix, in cells, the UI draws before showing a notice. 0 means no limit. |
| `--plugins-dir` | none | Directory of extension plugin binaries loaded at startup. Also `SWITCHTENDER_PLUGINS_DIR`. See [Extend in Go](sdk.md). |
| `--retain-runs` | none | Delete terminal runs older than this, for example `90d`. Empty keeps them forever. |
| `--retain-events` | none | Drop run events and logs older than this, for example `30d`. Empty keeps them forever. |
| `--retention-interval` | `1h` | How often the retention sweeper runs. |
| `--smtp-addr` | none | SMTP server host:port for run notification emails. Empty disables email. |
| `--smtp-from` | none | Sender address for notification emails. |
| `--smtp-to` | none | Recipient address for notification emails. Repeatable. |
| `--smtp-username` | none | SMTP username. The password comes from `SWITCHTENDER_SMTP_PASSWORD`. |
| `--notify-on` | `failure` | When to email: `failure` for failed runs only, or `finish` for every terminal run. |

Retention windows accept a whole number of days with a `d` suffix, such as `30d`, or Go duration
syntax such as `720h`.

### AI providers

The advisory AI features run against one provider, chosen with `--ai-provider`: `ollama` for a local
model, or `anthropic` and `openai` for a cloud model with `SWITCHTENDER_AI_KEY`. The provider is off
until set, and no feature ever executes anything the provider suggests.

Cloud models see automation content: commands, playbook names, failed-run logs, and host drift.
Because that content is security-adjacent, a model with strict safety classifiers can decline a
benign request as a false positive. When the Anthropic model is a Fable or Mythos model, SwitchTender
opts into server-side fallbacks, so a declined request is retried on `claude-opus-4-8` in the same
call and the feature keeps working. A Fable model also requires that the account keep 30-day data
retention, or the API rejects every request.

## desktop

Runs SwitchTender as a local desktop application: it serves on a private loopback port, stores its
data in a per-user directory, and opens the web UI in the default browser. It takes no flags. Set
`SWITCHTENDER_DESKTOP_NO_BROWSER` to skip opening a browser. See [Desktop](desktop.md) for packaging a
macOS app or a Windows installer.

## worker

Leases pending runs from the shared store and executes them. Point it and a server at the same
database and they compete for work.

| Flag | Default | Purpose |
|------|---------|---------|
| `--db` | `switchtender.db` | SQLite file path, or a `postgres://` DSN. |
| `--name` | host and pid | Worker name stamped on the runs it executes. |
| `--queue` | none | Queue this worker serves. Repeatable. Without any, it serves the default pool. |
| `--allow-container-ee` | `false` | Allow container execution environments on this worker. Needs Docker. |
| `--plugins-dir` | none | Directory of extension plugin binaries loaded at startup. Also `SWITCHTENDER_PLUGINS_DIR`. |
| `--container-memory` | `2g` | Memory cap for containerized runs, as docker `--memory`. Empty removes the cap. |
| `--container-cpus` | `2` | CPU cap for containerized runs, as docker `--cpus`. Empty removes the cap. |
| `--container-pids-limit` | `2048` | Process cap for containerized runs, as docker `--pids-limit`. Zero removes the cap. |
| `--container-network` | `bridge` | Network mode for containerized runs, as docker `--network`. |

## token

Manages API tokens. Creating the first token turns on authentication.

- `token new --name <label> [--ttl <duration>]` mints a token, printed once. A zero TTL never
  expires.
- `token list` lists tokens without their secrets.
- `token revoke <id>` deletes a token.

All token subcommands take `--db` and the global `--pretty` flag for indented JSON.

## user

Manages accounts with roles: admin, operator, and viewer.

- `user new <username> --role <role>` creates an account. The password comes from
  `SWITCHTENDER_PASSWORD` or a prompt, never an argument.
- `user list` lists accounts.
- `user delete <id>` deletes an account. Its tokens stop working.

All user subcommands take `--db`.

## import

Migrates from AWX or Semaphore. See [Migration](migration.md).

- `import awx <export.json> [--apply]`.
- `import semaphore <export.json> [--apply]`.

Both take `--db` for the target database. Without `--apply` the command only reports what it would
create.

## demo

Seeds a fresh database with sample data and real runs, then serves it read-only, so a public
instance is safe to expose. It needs ansible on the PATH to run the sample playbooks.

| Flag | Default | Purpose |
|------|---------|---------|
| `--addr` | `:8080` | Address the demo listens on. |
| `--db` | temporary file | Database to seed and serve. Empty uses a fresh temporary SQLite file. |

## Global flags

| Flag | Purpose |
|------|---------|
| `--pretty` | Indent JSON output instead of the compact default. |
