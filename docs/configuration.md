<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Configuration

SwitchTender is one binary with subcommands. This page lists every command, flag, and environment
variable.

## Licensed features

Almost everything on this page runs on Community, which needs no license. Six features are licensed,
and separately an install is capped on how many approval policies it holds at once. A flag or request
that turns on a licensed feature is refused outright rather than quietly ignored, so an install never
believes it has a control it does not have.

| Capability | Tier | Turned on by |
|------------|------|--------------|
| Directory sign-in (OIDC, SAML, LDAP, JWT) | Pro | Any `--oidc-*`, `--saml-*`, `--ldap-*`, or `--jwt-*` flag. `serve` refuses to start with one set and no license. |
| The full policy engine: deny rules, risk floors, actor scoping, distinct-approver separation of duties | Team | Creating a policy that uses one. A single require-approval policy stays Community. |
| More approval policies at once | Pro holds five, Team is uncapped | Creating policies. Community holds one. |
| The period change register | Team | `audit report`, and `GET /v1/audit/register`. |
| Distributed workers | Team | Every `switchtender worker`, whether it shares the database or reaches the server over the mesh relay with `--server`. |
| Initializing a new PostgreSQL database | Team | The first `serve` against a `postgres://` DSN. Opening a schema that already exists is never gated. |
| One-click drift reconcile | Team | `POST /v1/drift/reconcile`. Drift detection itself is free. |

`license status` prints the tier this install runs and when a license lapses. Tiers and prices are at
<https://switchtender.com/pricing>.

## Environment variables

| Variable | Used by | Purpose |
|----------|---------|---------|
| `SWITCHTENDER_ENCRYPTION_KEY` | serve, worker | Passphrase that seals stored credentials with AES-256-GCM. Credentials are disabled when unset. |
| `SWITCHTENDER_ENCRYPTION_SALT` | serve, worker | Per-deployment salt for argon2id key derivation. Must be set alongside the key and stay stable across restarts, or stored credentials cannot be decrypted. Credentials are disabled when unset. |
| `SWITCHTENDER_DB` | serve, worker | The database when `--db` is not given: a SQLite file path or a `postgres://` DSN. A DSN carries its password, and on the command line it shows in the process list and, on Kubernetes, in the pod spec, so a shared deployment names it here instead. |
| `SWITCHTENDER_AUDIT_KEY` | serve | Hex-encoded ed25519 seed for the install's signing identity, which signs the LoomSeal bundles it emits and binds every audit entry to this install. Unset beside a local database, the install mints and stores its own key there. Unset against a shared database it mints nothing, since every process must sign as the same install, and the chain is recorded unattributed and unbound until a seed is supplied to all of them. A malformed value stops startup. |
| `SWITCHTENDER_IDENTITY_DIR` | serve, audit, receipt | Directory holding the install's producer signing identity. A SQLite install keeps it beside the database and needs no setting. A postgres install has no filesystem home, so it uses a per-user configuration directory; when the account has no home, as in a container, there is nowhere durable to put a key and startup refuses rather than choosing a path a restart would empty. Point this at a durable path the server owns. |
| `SWITCHTENDER_PASSWORD` | user new | Initial account password, read instead of prompting so it never lands on the command line. |
| `SWITCHTENDER_SMTP_PASSWORD` | serve | Password for SMTP authentication when `--smtp-username` is set. |
| `SWITCHTENDER_AI_KEY` | serve | API key for a cloud AI provider such as Anthropic or an OpenAI-compatible endpoint. A local Ollama needs none. |
| `SWITCHTENDER_OIDC_CLIENT_SECRET` | serve | OpenID Connect client secret, paired with `--oidc-client-id`. Read from the environment so it stays off the command line. |
| `SWITCHTENDER_LDAP_PASSWORD` | serve | Password for the `--ldap-bind-dn` service account. |
| `SWITCHTENDER_WORKER_TOKEN` | serve, worker | Mesh relay bearer token. The server reads it when `--worker-token` is unset, and a relay worker started with `--server` presents it on every call. |
| `SWITCHTENDER_GALAXY_SERVER` | serve, worker | Default for `--galaxy-server`, a private Ansible Galaxy or Automation Hub URL. |
| `SWITCHTENDER_PUBLIC_URL` | serve | Default for `--public-url`, this server's public address for links in pull request reviews. |
| `SWITCHTENDER_GALAXY_TOKEN` | serve, worker | Token for the `--galaxy-server` URL, read from the environment so it never lands on the command line. |
| `SWITCHTENDER_ANSIBLE_BIN` | serve, worker | Default for `--ansible-bin`, the directory of Ansible commands runs use, or `system` for the ones on PATH. |
| `SWITCHTENDER_ANSIBLE_RUNTIME_DIR` | serve, worker, ansible | The [managed Ansible runtime](ansible-runtime.md) directory when no flag names one. Defaults to `ansible/` in the data directory. |
| `SWITCHTENDER_FEDERATION_ISSUER` | serve, worker | Default for `--federation-issuer`, the external URL this install is an OpenID Connect issuer at for federated cloud credentials. Every process on one database must see the same value. |
| `SWITCHTENDER_PLUGINS_DIR` | serve, worker | Directory of extension plugin binaries, read when `--plugins-dir` is unset. |
| `SWITCHTENDER_EGRESS_PROXY` | serve, worker | Proxy for the server's own outbound requests: notifications, federation token exchange, forge review calls, external secret sources, git remotes, and the registry lookups `--image-digest-lookup` makes. Its scheme is http, https, or socks5. The ambient `HTTP_PROXY` and `HTTPS_PROXY` are never used, so a forgotten proxy variable cannot route these requests somewhere that skips the cloud metadata and loopback checks. When this is set, each target is resolved and held to those checks before the request reaches the proxy, and an IP-literal target is refused. The proxy's own egress policy is then the boundary for a hostname that rebinds after the check, so point it at the hosts the server is meant to reach. |
| `SWITCHTENDER_RUNFILES_DIR` | serve, worker | Default for `--runfiles-dir`, the private directory runs stage their keys, tokens, and secret files under. See [Run files](run-files.md). |
| `SWITCHTENDER_ADMIN_PASSWORD` | init | Password for the first admin account. When unset, init generates one and prints it once. |
| `SWITCHTENDER_DESKTOP_NO_BROWSER` | desktop | Set to any value to skip opening the browser, for a headless or remote run. |
| `SWITCHTENDER_NOTIFY_NTFY_TOKEN` | serve | Bearer token for a protected ntfy topic, read when `--notify-ntfy-token` is unset. A run cannot read it, and a flag shows in the process list. |
| `SWITCHTENDER_NOTIFY_GRAFANA_TOKEN` | serve | Bearer token for the Grafana annotations API, read when `--notify-grafana-token` is unset. |
| `SWITCHTENDER_NOTIFY_TWILIO_TOKEN` | serve | Twilio Auth Token, read when `--notify-twilio-token` is unset. |
| `SWITCHTENDER_WITNESS_TOKEN` | witness serve | Bearer token the witness API requires, read when `--api-token` is unset. |
| `SWITCHTENDER_MCP_TOKEN` | mcp | API token the agent presents, read when `--token` is unset. |
| `SWITCHTENDER_TOKEN` | mcp | Fallback for `SWITCHTENDER_MCP_TOKEN`. |
| `SWITCHTENDER_LICENSE` | serve, worker, license, audit | Path to the license file, overriding `switchtender-license.json` beside the database. |
| `SWITCHTENDER_DEMO_PORT` | docker compose | Host port the compose `demo` profile publishes, `8081` by default so the demo and the stack can run side by side. |

Every command that takes `--db` defaults to `switchtender.db` in the current directory. Only `serve`,
`init`, `demo`, `restore`, and an import create a database there. The commands that work on an
existing install, such as `token`, `user`, `examples`, `backup`, and the `audit` commands, refuse a
SQLite path that does not exist and name the path they looked at. Opening it would create an empty
database the server never reads, and the command would report success. An import that starts a new
database says so, with the `--db` a server needs to read it.

## init

Bootstraps a new deployment. It creates the database and the first admin account, writes an
environment file, and optionally a systemd unit. Run it once, then start `serve`. Running it again,
with `--force` to rewrite the environment file or after deleting it, leaves an admin account that
already exists as it is, password included.

| Flag | Default | Purpose |
|------|---------|---------|
| `--db` | `switchtender.db` | SQLite database path. |
| `--config` | `switchtender.env` | Environment file to write. |
| `--addr` | `127.0.0.1:8080` | Address the server listens on. Loopback by default. |
| `--admin` | `admin` | Username for the first admin account. |
| `--systemd` | none | Path to write a systemd unit to, empty to skip. |
| `--force` | `false` | Rewrite an existing config file, keeping the encryption key and salt it already holds so the credentials sealed under them still open. Delete the file instead to start over with new keys. |

The admin password comes from `SWITCHTENDER_ADMIN_PASSWORD`, or is generated and printed once when
that variable is unset.

## serve

Runs the HTTP API, the in-process executor, the scheduler, the retention sweeper, and the web UI.

| Flag | Default | Purpose |
|------|---------|---------|
| `--addr` | `127.0.0.1:8080` | Address the server listens on. Loopback by default. Set `0.0.0.0:8080` to expose it on the network. |
| `--db` | `switchtender.db` | SQLite file path, or a `postgres://` DSN for the PostgreSQL backend. |
| `--tls-cert` | none | TLS certificate file, to serve HTTPS directly with no reverse proxy. Requires `--tls-key`. |
| `--tls-key` | none | TLS private key file. Requires `--tls-cert`. |
| `--oidc-issuer` | none | OpenID Connect issuer URL to enable single sign-on. Empty leaves SSO off. |
| `--oidc-client-id` | none | OIDC client id. |
| `--oidc-redirect-url` | none | OIDC redirect URL, for example `https://host/auth/oidc/callback`. |
| `--oidc-default-role` | `viewer` | Role granted to an account created on first SSO sign-in: admin, operator, or viewer. |
| `--forge-oauth` | none | A GitHub or GitLab OAuth application people link their forge account through, so a pull request comment can act as them. Comma-separated `provider=github` or `provider=gitlab`, `client_id=ID`, and the client secret from exactly one of `secret_env=VAR` or `secret_file=PATH`, never the command line. Add `web_url` for GitHub Enterprise Server or self-managed GitLab, and `api_url` when its API base is not the usual `/api/v3` or `/api/v4` under it. Repeatable, one per forge. Needs `--public-url` and `SWITCHTENDER_ENCRYPTION_KEY`, which signs each link request. See [linking forge accounts](pull-request-review.md#linking-forge-accounts). |
| `--ldap-url` | none | LDAP directory URL to enable directory sign-in, for example `ldaps://ldap.example.com:636`. |
| `--ldap-bind-dn` | none | Service account DN used to search for a user, empty for an anonymous search. |
| `--ldap-base-dn` | none | Search base for finding a user. |
| `--ldap-user-filter` | `(uid=%s)` | Search filter with one `%s` for the username. |
| `--ldap-default-role` | `viewer` | Role for an account created on first directory sign-in. |
| `--ldap-role-map` | none | Map a directory group to a role as `groupDN=role`. A matched group sets the role on every sign-in. Repeatable. |
| `--public-url` | none | Public base URL of this server, such as `https://switchtender.example.com`. A [pull request review](pull-request-review.md) links its comment and commit status to the run under it. Empty posts run ids without links. |
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
| `--jwt-audience` | none | Expected token audience. Left empty the audience is not checked, so every token the issuer signs is accepted, including one minted for a different application at the same issuer. Set it unless the issuer serves this install alone. |
| `--jwt-username-claim` | `sub` | Claim naming the account. |
| `--jwt-groups-claim` | none | Claim holding the user's groups, used with `--jwt-role-map`. |
| `--jwt-role-map` | none | Map a token group to a role as `group=role`. Repeatable. |
| `--jwt-default-role` | `viewer` | Role granted to an account created on first JWT sign-in. |
| `--ai-provider` | none | Enable advisory AI features with a provider: `ollama`, `anthropic`, or `openai`. Empty leaves AI off. |
| `--ai-model` | provider default | Model name for the AI provider. Required for `openai`, which has no universal default. |
| `--ai-url` | provider default | Base URL for the AI provider, for a self-hosted Ollama, an OpenAI-compatible server, or a proxy. |
| `--schedule-interval` | `15s` | How often the scheduler checks for due schedules. |
| `--workers` | `4` | Concurrent runs this process executes at once. At least 1: the server executes its own runs, so there is no value that makes it execute none. |
| `--max-shards` | `512` | Most groups a split fans out into. A split is always bounded by the host count. |
| `--run-timeout` | `0` | Default cap on how long a run may execute before it is canceled and failed, for example `1h`. A run may set a shorter timeout. Zero leaves runs uncapped. |
| `--module-fetch-timeout` | `2m` | How long one module download may run, from `1s` to `1h`. The approval gate downloads the registry and remote modules a Terraform or OpenTofu plan calls before it reads the plan, while the submission waits, and a run downloads them again when the gate's copy is not kept where it executes. Past the bound the download stops and the plan stays unclassified, so it is held or refused. |
| `--module-fetch-max-mib` | `512` | How many MiB one module download may write, from `1` to `65536`. The download also stops past 50,000 files. Past either bound the plan stays unclassified, so it is held or refused. |
| `--module-keep-for` | `168h` | How long the module trees the gate downloads are kept for the run it judged, from `1h` to `2160h`. A run held for approval longer than this downloads its modules again when it starts, and is refused unless they match what the gate read. |
| `--module-keep-max-mib` | `2048` | How many MiB the kept module trees may occupy together, from `1` to `1048576`. Past it the oldest tree is dropped first, and its run downloads its modules again when it starts. |
| `--notify-webhook` | none | URL that receives a JSON notification when a run finishes, is held for approval, waits at a workflow approval step, or needs attention past its alert threshold. Repeatable. |
| `--notify-slack` | none | Slack incoming webhook URL that receives a message when a run finishes, is held for approval, waits at a workflow approval step, or needs attention past its alert threshold. Repeatable. |
| `--notify-mattermost` | none | Mattermost incoming webhook URL that receives a message when a run finishes, is held for approval, waits at a workflow approval step, or needs attention past its alert threshold. Repeatable. |
| `--notify-rocketchat` | none | Rocket.Chat incoming webhook URL that receives a message when a run finishes, is held for approval, waits at a workflow approval step, or needs attention past its alert threshold. Repeatable. |
| `--notify-discord` | none | Discord incoming webhook URL that receives a message when a run finishes, is held for approval, waits at a workflow approval step, or needs attention past its alert threshold. Repeatable. |
| `--notify-teams` | none | Microsoft Teams incoming webhook URL that receives an Adaptive Card when a run finishes, is held for approval, waits at a workflow approval step, or needs attention past its alert threshold. Repeatable. |
| `--notify-ntfy` | none | ntfy topic URL that receives a notification when a run finishes, is held for approval, waits at a workflow approval step, or needs attention past its alert threshold, such as https://ntfy.sh/my-topic. Repeatable. |
| `--notify-ntfy-token` | none | Optional bearer token for a protected ntfy topic, applied to every `--notify-ntfy` URL. |
| `--notify-pagerduty` | none | PagerDuty Events API routing key that triggers an incident when a run fails. Repeatable. |
| `--notify-grafana` | none | Grafana base URL that receives an annotation when a run finishes. Repeatable. |
| `--notify-grafana-token` | none | Bearer token for the Grafana annotations API, applied to every `--notify-grafana` URL. |
| `--notify-twilio-sid` | none | Twilio Account SID for SMS notifications on a failed run. |
| `--notify-twilio-token` | none | Twilio Auth Token, paired with `--notify-twilio-sid`. |
| `--notify-twilio-from` | none | Twilio sender phone number that texts run failures. |
| `--notify-twilio-to` | none | Phone number that receives an SMS when a run fails. Repeatable. |
| `--allow-container-ee` | `false` | Allow runs whose project pins a container image to execute inside it. Needs Docker on the executor. |
| `--default-image` | none | Fallback execution image for runs that pin none at the run, template, or project level. It is resolved when a run is submitted and pinned onto it, so an approval covers it. Empty leaves an unpinned run on the host. |
| `--image-digest-lookup` | `true` | Resolve a submitted run's image tag to the digest its registry serves, so the run executes by that digest and an approval covers that exact image. A run whose registry does not answer stays bound to its tag, and the approval view says so. A registry that does not answer within 10 seconds is not asked again for a minute, so a dead registry delays one submission a minute rather than every one. The lookup ends when the request submitting the run ends, and the submission stops with it. Turn off where no registry is reachable from the server. |
| `--require-image-digest` | `false` | Reject a container run whose image is not pinned to an `@sha256:` digest. |
| `--container-memory` | `2g` | Memory cap for containerized runs, as docker `--memory`. Empty removes the cap. |
| `--container-cpus` | `2` | CPU cap for containerized runs, as docker `--cpus`. Empty removes the cap. |
| `--container-pids-limit` | `2048` | Process cap for containerized runs, as docker `--pids-limit`. Zero removes the cap. |
| `--container-network` | `bridge` | Network mode for containerized runs, as docker `--network`, for example bridge or none. A run with network access can reach the host's cloud metadata service and read the instance identity, which on a cloud host is a credential. Use none for runs that do not need the network, and block the link-local metadata address at the host firewall where they do. |
| `--container-runtime` | `docker` | Container CLI for containerized runs: docker or podman. |
| `--container-pull-policy` | `missing` | Image pull policy for containerized runs, as docker `--pull`: always, missing, or never. |
| `--container-runfiles-size` | `64m` | Size of the in-memory filesystem a containerized run's private directory is mounted as, nosuid and nodev with exec allowed. It counts against `--container-memory` as it fills. See [Run files](run-files.md). |
| `--runfiles-dir` | none | Private directory each run stages its keys, tokens, and secret files under. Empty picks the systemd runtime directory, then a private `XDG_RUNTIME_DIR`, then the temporary directory, which the doctor warns about. It must be on a known local filesystem, a tmpfs for preference, and startup refuses one that is not. Also `SWITCHTENDER_RUNFILES_DIR`. See [Run files](run-files.md). |
| `--galaxy-server` | none | Private Ansible Galaxy or Automation Hub URL for project collection installs. Token from `SWITCHTENDER_GALAXY_TOKEN`. |
| `--ansible-bin` | none | Directory holding the `ansible-playbook` and `ansible-inventory` runs use, or `system` for the ones on PATH. A relative value is resolved against the working directory at startup. Empty uses the [managed Ansible runtime](ansible-runtime.md) when one is in use, then PATH. Also `SWITCHTENDER_ANSIBLE_BIN`. |
| `--ansible-runtime-dir` | `ansible/` in the data directory | Managed Ansible runtime directory. Also `SWITCHTENDER_ANSIBLE_RUNTIME_DIR`. |
| `--federation-issuer` | none | External https URL this install is an OpenID Connect issuer at, so a run reaches AWS, Google Cloud, or Azure with short-lived federated credentials and nothing durable is stored. Serves the discovery document and public keys under it and needs the encryption key and salt, which seal the signing key. Falls back to `SWITCHTENDER_FEDERATION_ISSUER`. See [Federated cloud credentials](federation.md). |
| `--strict-grants` | `false` | Deny non-admins access to an object that has no grants, instead of deferring to the global role. Off by default, which means separation between organizations is not enforced until you turn it on: see below. |
| `--read-only` | `false` | Reject every mutating request, for a safely exposable instance. |
| `--matrix-cap` | `50000` | Largest host matrix, in cells, the UI draws before showing a notice. 0 means no limit. |
| `--plugins-dir` | none | Directory of extension plugin binaries loaded at startup. Also `SWITCHTENDER_PLUGINS_DIR`. See [Extend in Go](sdk.md). |
| `--worker-token` | none | Bearer token that authenticates mesh relay workers and enables the relay endpoints. Also `SWITCHTENDER_WORKER_TOKEN`. Keep it secret. On its own, every worker holding it may lease from every queue. |
| `--worker-pools` | none | YAML file binding each worker token to the queues it may lease from, so a queue is a boundary rather than a routing hint. A pool may also register a `delivery_key` to receive its runs' secrets sealed to it: see [Delivering secrets to relay workers](#delivering-secrets-to-relay-workers). |
| `--retain-runs` | none | Delete terminal runs older than this, for example `90d`. Empty keeps them forever. Deleting a run does not make what it left behind unreadable: summaries, drift, and host state history outlive it, and the purge retains the record of who could read the run so those rows stay readable to exactly the same people. That record is dropped automatically once nothing references it. A run whose outcome is not yet on the audit chain is kept, with its steps or shards and everything they hold, until the outcome is committed. |
| `--retain-events` | none | Drop run events and logs older than this, for example `30d`. Empty keeps them forever. A run whose outcome is not yet on the audit chain keeps its events and logs until the outcome is committed, since the outcome commits to its log. |
| `--retain-history` | none | Keep only this many per-host and per-task summaries for each host and each task, for example `500`. Summaries outlive the runs they came from, so this is the only bound on them. Zero keeps every summary forever. A smaller value is raised to 500, the deepest window the fleet views will answer. The summaries of a run whose outcome is not yet on the audit chain are kept until it is. |
| `--facts-interval` | `24h` | Minimum spacing between retained host state snapshots. The estate history keeps the newest gather in each period, which is what answers what a host looked like on a date. Zero keeps every gather, at roughly a hundred times the disk. |
| `--retain-facts` | `400` | Keep only this many host state snapshots for each host. A fact set is hundreds of kilobytes, so unlike summaries this is bounded by default. Zero keeps every snapshot forever. |
| `--retention-interval` | `1h` | How often the retention sweeper runs. |
| `--evidence-dir` | none | Directory for periodic change registers. Set together with `--evidence-cadence`. Team: the server refuses to start with a cadence set on a Community license. |
| `--evidence-cadence` | none | How long each change register covers and how often one is written, for example `2160h` for a quarter. Minimum `1h`. Zero writes none. Progress is read from the archive, so a restart resumes from the newest pack rather than starting the period again. |
| `--forward-url` | none | HTTP endpoint audit events stream to as NDJSON, one JSON object per line, each carrying its `seq:link` receipt. Splunk HEC raw, Elastic, and log routers ingest it directly. |
| `--forward-header` | none | Header set on every forwarded batch, as `Name: value`, for example an HEC token. Repeatable. |
| `--forward-syslog` | none | TCP syslog collector (`host:port`) audit events stream to as RFC 5424, octet-counted, one message per event with the JSON event as the body. |
| `--forward-syslog-tls` | `false` | Wrap the syslog connection in TLS. |
| `--forward-state` | `switchtender-forward.json` | Durable cursor recording the last position every sink accepted. The cursor advances only on delivery, so an outage delays events rather than dropping them, and a restart resumes without restreaming. |
| `--forward-interval` | `5s` | How often the forwarder polls the chain when caught up. Minimum `1s`. |
| `--smtp-addr` | none | SMTP server host:port for run notification emails. Empty disables email. |
| `--smtp-from` | none | Sender address for notification emails. |
| `--smtp-to` | none | Recipient address for notification emails. Repeatable. |
| `--smtp-username` | none | SMTP username. The password comes from `SWITCHTENDER_SMTP_PASSWORD`. |
| `--notify-on` | `failure` | When to email: `failure` for failed runs only, or `finish` for every finished run, every run held for approval, and every workflow waiting at an approval step. An attention alert is emailed under either setting. |
| `--policy-file` | none | YAML file holding the approval policies and naming any Rego modules they load, read relative to the file. When set, the file is the source of truth and the API refuses policy edits. See [Approval policies](policy.md). |
| `--attention-file` | none | YAML file setting when waiting work shows as blocked on the overview's Needs attention panel and when it alerts, as organization defaults with per-organization, per-queue, and per-template overrides. Empty uses the built-in thresholds. A malformed file stops the server. See [Attention thresholds](#attention-thresholds). |
| `--trusted-proxy` | none | CIDR of a reverse proxy whose client IP header to believe, repeatable. Required behind a proxy: without it every request appears to come from the proxy itself, so the failed sign-in budget, the webhook rate limit, and the per-client stream budget all become one budget shared by everyone behind it, and one stranger's failed guesses can lock sign-in for the whole install. |
| `--client-ip-header` | none | Header carrying the real client address from a trusted proxy. Defaults to the leftmost `X-Forwarded-For` entry. |
| `--callback-rate-limit` | `30` | Provisioning callbacks one client address may make in a minute, the native and the AWX-compatible callback address counted together. A refusal answers `429`, names the limit and this flag, and is logged once per address a minute. See [callback rate limits](tool-ansible.md#callback-rate-limits) for hosts behind one NAT address. |
| `--callback-key-failure-limit` | `10` | Wrong host config keys one client address may present in a minute. Past it, every callback from that address is refused for the rest of the minute, the right key included, so a key cannot be guessed at a useful rate. |
| `--fact-cache-admin-only` | `false` | Restrict reading cached facts to admins. By default an operator with read on the inventory and on the run that gathered the facts may read them, with secret-looking values masked. |
| `--span-cadence` | `0` | Append a span beat to the audit chain this often, for example `60s`. Whole seconds only. Zero appends none. |
| `--anchor-tsa-url` | none | RFC 3161 timestamp authority the chain anchors against. Empty anchors nothing. |

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

Runs SwitchTender as a local desktop application. It serves on a private loopback port, stores its
data in a per-user directory, and opens the web UI in the default browser. It takes no flags. Set
`SWITCHTENDER_DESKTOP_NO_BROWSER` to skip opening a browser. See [Desktop](desktop.md) for packaging a
macOS app or a Windows installer.

## worker

Leases pending runs from the shared store and executes them. Point it and a server at the same
database and they compete for work. Or start it with `--server` and it leases runs from the control
node over the mesh relay, with no database access of its own.

| Flag | Default | Purpose |
|------|---------|---------|
| `--db` | `switchtender.db` | SQLite file path, or a `postgres://` DSN. Ignored with `--server`. |
| `--server` | none | Control node base URL to lease runs from over the mesh relay, for example `https://switchtender.example.com`. When set, the worker needs no database and dials one outbound connection. Token from `SWITCHTENDER_WORKER_TOKEN`. |
| `--name` | host and pid | Worker name stamped on the runs it executes. |
| `--queue` | none | Queue this worker serves. Repeatable. Without any, it serves the default pool. |
| `--workers` | `4` | Concurrent runs this process executes at once. At least 1: a worker with no slots would lease nothing and sit idle. |
| `--run-timeout` | `0` | Default cap on how long a run may execute before it is canceled and failed, for example `1h`. Zero leaves runs uncapped. |
| `--module-fetch-timeout` | `2m` | How long one module download may run, from `1s` to `1h`. A run downloads its modules again here when the copy the gate read is not kept on this machine, and goes ahead only if they are the ones the gate read. |
| `--module-fetch-max-mib` | `512` | How many MiB one module download may write, from `1` to `65536`, and at most 50,000 files. |
| `--module-keep-for` | `168h` | How long a module tree kept in this worker's run-files root is offered to the run the gate read it for, from `1h` to `2160h`. A worker sharing its run-files root with a server executes the trees that server's gate kept. |
| `--module-keep-max-mib` | `2048` | How many MiB the module trees this worker keeps may occupy together, from `1` to `1048576`, the oldest dropped first. |
| `--facts-interval` | `24h` | Minimum spacing between retained host state snapshots. Applies only with `--db`: a worker using `--server` reports what it gathered to the control node, which spaces the history with its own setting. |
| `--retain-facts` | `400` | Snapshots kept per host. Applies only with `--db`, for the same reason. |
| `--allow-container-ee` | `false` | Allow container execution environments on this worker. Needs Docker. |
| `--default-image` | none | Fallback execution image for runs that pinned none when they were submitted. It never applies to an approved run: one approved to run on the host is refused rather than moved into this image. |
| `--require-image-digest` | `false` | Reject a container run whose image is not pinned to an `@sha256:` digest. |
| `--plugins-dir` | none | Directory of extension plugin binaries loaded at startup. Also `SWITCHTENDER_PLUGINS_DIR`. |
| `--container-memory` | `2g` | Memory cap for containerized runs, as docker `--memory`. Empty removes the cap. |
| `--container-cpus` | `2` | CPU cap for containerized runs, as docker `--cpus`. Empty removes the cap. |
| `--container-pids-limit` | `2048` | Process cap for containerized runs, as docker `--pids-limit`. Zero removes the cap. |
| `--container-network` | `bridge` | Network mode for containerized runs, as docker `--network`. |
| `--container-runtime` | `docker` | Container CLI for containerized runs: docker or podman. |
| `--container-pull-policy` | `missing` | Image pull policy for containerized runs, as docker `--pull`: always, missing, or never. |
| `--container-runfiles-size` | `64m` | Size of the in-memory filesystem a containerized run's private directory is mounted as, nosuid and nodev with exec allowed. It counts against `--container-memory` as it fills. See [Run files](run-files.md). |
| `--runfiles-dir` | none | Private directory each run stages its keys, tokens, and secret files under. Empty picks the systemd runtime directory, then a private `XDG_RUNTIME_DIR`, then the temporary directory, which the doctor warns about. It must be on a known local filesystem, a tmpfs for preference, and startup refuses one that is not. Also `SWITCHTENDER_RUNFILES_DIR`. See [Run files](run-files.md). |
| `--galaxy-server` | none | Private Ansible Galaxy or Automation Hub URL for project collection installs. Token from `SWITCHTENDER_GALAXY_TOKEN`. |
| `--ansible-bin` | none | Directory holding the `ansible-playbook` and `ansible-inventory` runs use, or `system` for the ones on PATH. A relative value is resolved against the working directory at startup. Empty uses the [managed Ansible runtime](ansible-runtime.md) when one is in use, then PATH. Also `SWITCHTENDER_ANSIBLE_BIN`. |
| `--ansible-runtime-dir` | `ansible/` in the data directory | Managed Ansible runtime directory. Also `SWITCHTENDER_ANSIBLE_RUNTIME_DIR`. |
| `--federation-issuer` | none | The same issuer URL serve is given. The worker signs the identity tokens of the runs it executes with the keys every process on the database shares, under this URL. Falls back to `SWITCHTENDER_FEDERATION_ISSUER`. |
| `--policy-file` | none | YAML file holding the approval policies, the same file the control node reads, with the Rego modules it names beside it. |
| `--delivery-key` | none | Private key file a relay worker opens the run secrets its control node seals to the worker's pool with, made by `switchtender worker key new`. Repeatable, so a worker holds its pool's old and new key while the pool rotates. Only with `--server`. A file another account can read is refused. See [Delivering secrets to relay workers](#delivering-secrets-to-relay-workers). |

### worker key

`switchtender worker key new --out FILE` generates a delivery key for a relay worker pool. It writes
the private key to `--out`, mode 0600, and never over an existing file, then prints the public key
and its key id as one JSON line:

    {"key_id":"f4e8a76a457da19cf3742de37986d439","delivery_key":"x25519:x9f9/kkB6Jq3Wqht/7TWzDGUeTRfbABrj1CO+8/6hHc=","file":"/etc/switchtender/delivery.key"}

`switchtender worker key public FILE` prints the same line for a key file that already exists, and
refuses one another account can read. The file is a standard PKCS #8 X25519 private key, so `openssl
genpkey -algorithm X25519 -out FILE` makes an equivalent one, and `openssl pkey -in FILE -pubout
-outform DER | tail -c 32 | base64` prints the base64 that follows `x25519:`.

## ansible

Installs, lists, and removes the [managed Ansible runtime](ansible-runtime.md): a Python virtual
environment holding one pinned ansible-core release, installed with pip in hash-checking mode from a
lock this binary carries. Runs use it when no `--ansible-bin` is configured. A `python3` the release
supports must be installed.

- `ansible install [--version <release>] [--python <path>] [--wheels <dir>] [--index-url <url>]`
  installs a release and makes it the one runs use. `--version` takes a release such as `2.21.4` or
  a minor version such as `2.21`, and defaults to the newest supported release. `--python` names the
  interpreter to build with, and defaults to `python3`, then `python3.N`, on PATH. `--wheels`
  installs offline from a directory of wheels, relative to where the command runs, checked against
  the same hashes. `--index-url` downloads from a mirror instead of PyPI. pip's own environment
  variables and configuration files are not read. Installing an installed release verifies it and
  changes nothing else.
- `ansible list` prints the installed releases and which one is current.
- `ansible remove [--version <release>]` removes one release, or every release and the directory.
- `ansible lock [--version <release>]` prints the requirements lock a release installs from.

Every `ansible` subcommand takes `--dir` for the runtime directory, which defaults to
`SWITCHTENDER_ANSIBLE_RUNTIME_DIR`, then `ansible/` in the data directory of `--db`, and `--pretty`
for indented JSON.

## token

Manages API tokens. A public bind on an empty database mints an initial admin token at startup.

- `token new --name <label> [--user <username>] [--ttl <duration>] [--agent]` mints a token, printed
  once. A zero TTL never expires.
- `--user` binds the token to an account, and the token carries that account's role. A token minted
  without `--user` is unscoped and acts as admin, so bind every token you hand to a person, a
  service, or an AI agent. See [running an agent](agents.md).
- `--agent` marks the token as held by an AI agent rather than a person. It caps the token at
  operator whatever role its account holds, so an agent can launch and propose work but can never
  manage identity, access, or secrets, and can never approve its own held run. Every action it takes
  is recorded in the chain as `actor_type: agent` with the account it acts for beside it, which is
  what `actor_kind: agent` policy rules match on. It requires `--user`, so the chain always records
  the human the agent acts for. Without one the command refuses. Mint every agent token with it: a
  token without `--agent` is indistinguishable from a person's in the record.
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

## license

Shows or installs this install's license. No license is Community, which is complete in itself. What
each tier covers is listed at <https://switchtender.com/pricing>.

- `license status` shows the tier this install runs and when a license lapses.
- `license install <file>` verifies a license file and installs it beside the database.

A license is read from the file `SWITCHTENDER_LICENSE` names, or from `switchtender-license.json` in
the same directory as the database. A license this install cannot parse or that has lapsed reads as
Community rather than failing the server, so an expiry never takes an install down.

## assess

Reports what an automation export holds and what governing it would change, without writing
anything and without a database. It reads an export the same way `import` does, then grades every
template it found through the same risk and reversibility graders the product uses at run time, so
a number in an assessment cannot disagree with what a run would later say.

    switchtender assess awx awx-export.json
    switchtender assess chef chef-nodes.json

Formats are `awx`, `semaphore`, `chef`, and `puppet`. An AWX-format export also covers Ansible
Automation Platform, Tower, and Ascender.

An export saved as UTF-8 with a byte order mark, or as UTF-16 the way Windows PowerShell writes a
file by default, is read the same as plain UTF-8, here, by `import`, and by the assessment page.

The output has three parts: what the import would create, what does not survive the move as it was,
and what changes about how it is governed. Everything that does not come across is listed, one line
each. The third part names the templates that cannot be undone, the ones carrying a
destructive signal, the credentials more than one template shares, and the templates targeting no
stored inventory.

Grades are a floor rather than a measurement. An Ansible template keeps its work in a playbook, and
at assessment time that playbook is in a repository nothing has fetched, so reading one can only
ever raise a grade. The report says how many templates that applies to rather than leaving the
numbers looking more settled than they are.

## import

Migrates from AWX, Semaphore, Chef, Puppet, Rundeck, Jenkins, or cron. Which objects each one carries across is in
[what each source brings over](migration.md#what-each-source-brings-over).

- `import awx <export.json> [--awx-template-ids <list.json>] [--apply]` brings projects,
  inventories static and dynamic, credential shells, job templates and workflows, surveys, and
  schedules. `--awx-template-ids` takes AWX's job template list, `GET /api/v2/job_templates/` saved
  as JSON, repeatable for each page. A job template that accepted provisioning callbacks is then
  bound to its AWX id, so it also answers at its old AWX callback address. An export that carries
  each job template's `id` needs no list. See
  [the AWX-compatible callback address](migration.md#the-awx-compatible-callback-address).
- `import semaphore <export.json> [--apply]` brings the same kinds from a Semaphore export, apart
  from the dynamic inventory sources AWX alone carries.
- `import chef <nodes.json> [--apply]` brings one inventory of the fleet: every node a host,
  grouped by its `chef_environment` and by every `role[...]` in its run list, carrying the ohai
  facts that identify a machine, with `ipaddress` also set as `ansible_host`. Accepts an array of
  node documents, a single node, or an object keyed by node name. Cookbooks and recipes are not
  imported and the recipes seen are named in the report.
- `import puppet <nodes.json|facts.json|nodes.txt> [--apply]` brings one inventory of the fleet,
  grouped by environment, from a PuppetDB nodes query, a PuppetDB facts query, or the plain
  certname list `puppet node list` prints. Deactivated and expired nodes are left out and counted.
  Manifests and modules are not imported.
- `import rundeck <jobs.yaml|project-archive.zip> [--inventory <name>] [--apply]` brings templates,
  surveys, and schedules from either a job export or a project archive, told apart by content. An
  archive brings one project as well, but only when its source control configuration names a
  repository this can reach. Neither artifact carries a node definition, so no inventory is imported
  from either and `--inventory` names the hosts its jobs target.
- `import jenkins <JENKINS_HOME|jobs-dir|config.xml> [--inventory <name>] [--apply]` brings
  templates, surveys, and schedules from freestyle jobs. Jenkins picks an agent by label, so
  `--inventory` names the machines.
- `import cron <crontab-file> [--inventory <name>] [--system] [--apply]` brings schedules alone, one
  per crontab line, each carrying its own one-step bash pipeline rather than a template. `--system`
  parses the six-field `/etc/crontab` form, whose user column sits before the command. That step
  runs on the SwitchTender host, not on the machine the crontab came from.

All seven take `--db` for the target database. Without `--apply` the command only reports what it
would create. Rundeck, Jenkins, and cron import no credentials, so a job that needed a login needs
one built by hand afterward.

## audit

Audit trail tools.

- `audit bundle` emits the chain as a signed LoomSeal bundle. Anyone verifies it offline with the
  open `loomseal` verifier, or in a browser, without trusting the server that produced it.
- `audit anchor` has a public timestamp authority sign the current head, so a chain that has lost
  its tail no longer reaches its anchor.
- `audit receipt <seq:link>` redeems a receipt the server issued, proving that entry is still in the
  chain.
- `audit report` renders the period's change register as a self-contained HTML evidence report.
- `audit run <id>` emits one run's evidence dossier as a self-contained HTML document.

Flags on the subcommands:

- `audit bundle --limit <n>` carries only the newest N entries. The default carries the whole chain.
- `audit anchor --type <kind>` picks the anchor: `rfc3161` for a signed timestamp, or `git` or
  `https` for one checked by fetching it. `--ref` is the authority URL for `rfc3161`, otherwise the
  URL a verifier fetches. `--tree` anchors the Merkle root over the whole chain, which is the
  coordinate a sparse receipt proves membership against.
- `audit report --from <when> --to <when>` bounds the change register's period. `--from` defaults to
  ninety days before `--to`, and `--to` defaults to now and is exclusive.

## receipt

Writes a signed receipt for one finished run, which a third party verifies offline with `verify`. A
receipt is the chain segment from the request that created the run through the entry recording what
it did, signed with this install's key and carrying any anchors that fix its position.

| Flag | Default | Purpose |
|------|---------|---------|
| `--db` | `switchtender.db` | Database holding the run and its chain. |
| `--out` | stdout | File to write the receipt to. |
| `--sparse` | off | Disclose only this run's own chain entries, proving each belongs to the log without carrying the entries around it. |
| `--append-only-from` | unset | With `--sparse`, prove the log only appended since this size. Use a size a reader already saw, such as an anchored head. |

Publish the key fingerprint the command prints so a verifier can pin it.

## verify

Verifies a receipt written by `receipt`. It trusts nothing this server says: it recomputes every
chain link from the receipt's own claims, checks the signature covers the exact bytes, and confirms
any anchors name an entry the receipt holds. It reads only the file, reaches no database and no
network, and does not run the server, so a relying party can check a receipt on a machine that has
never seen this install.

    switchtender verify run.receipt --pubkey sha256:...

Pass `--pubkey` with the fingerprint the producer published to tie the result to a key obtained out
of band. Without it the receipt is checked against the key it names, which proves it was not altered
but not who signed it, and the verdict says so: `INTACT, BUT UNIDENTIFIED` rather than `VERIFIED`.
A `--pubkey` given an empty value, which is what a failed key fetch leaves behind, is refused rather
than treated as no pin.

## witness

Watches a server's span beat feed from outside it. A chain proves what it holds was not altered, but
not that nothing was removed from the end, because the process running the chain also decides what
gets written down. A witness on another machine remembers what the feed served, keeps that memory in
a signed checkpoint, and raises a finding when a beat goes missing, an already-witnessed beat comes
back rewritten, or the head regresses. Run it where the server's operator has no hand.

The server has to be writing beats for there to be anything to watch, and beats are off by default.
Start the server with `--span-cadence`, for example `--span-cadence 60s`. Against a server with
beats off, the witness reports `empty_feed`, and a `--once` run exits nonzero.

| Flag | Default | Purpose |
|------|---------|---------|
| `--server` | required | Base URL of the server to watch. |
| `--state` | `switchtender-witness.json` | Signed checkpoint holding what this witness has seen. |
| `--interval` | `1m` | How often to poll the feed. |
| `--key-dir` | the state file's directory | Directory holding the witness signing key. |
| `--once` | off | Run one check and exit nonzero on findings, for cron. |
| `--webhook` | unset | URL that receives each finding as a JSON POST. |

- `witness serve` watches many servers from one process and answers auditors with countersigned
  attestations.
- `witness verify-attestation` verifies an attestation offline against a pinned witness key.

## witness serve

Runs the witness. Its flags are checked against that subcommand rather than `witness` itself.

| Flag | Default | Purpose |
|------|---------|---------|
| `--listen` | `127.0.0.1:9440` | Address the witness API serves on. |
| `--state-dir` | `switchtender-witness` | Directory holding the signed checkpoints and the findings record. |
| `--api-token` | none | Bearer token the witness API requires. Prefer `SWITCHTENDER_WITNESS_TOKEN`. Required when `--listen` is not loopback. |
| `--watch` | none | Base URL of a server to watch, repeatable, at least one. |
| `--interval` | `1m0s` | How often every watched server is checked. At least 10s. |
| `--key-dir` | the state directory | Directory holding the witness signing key. |
| `--webhook` | none | URL that receives every finding as a JSON POST. |

## witness verify-attestation

Verifies one attestation offline, for the relying party. It reads the JSON, recomputes the signature,
and prints the verdict with the signer named in both published forms: `signed_by` is the raw hex
public key the attestation carries, and `signed_by_key_id` is the sha256 key id the witness prints at
startup and serves from its API.

    switchtender witness verify-attestation attestation.json --pubkey sha256:...

The verdict carries `pinned`. Without a pin, `ok` means only that the document is internally
consistent, which an attestation a forger signed with their own key also is, so an unpinned verdict
carries a note saying so. An empty `--pubkey` is refused.

| Flag | Default | Purpose |
|------|---------|---------|
| `--pubkey` | none | Pinned witness key, as either the sha256 key id or the raw hex public key. |

Pass `--pubkey` with the key you pinned out of band. Either form is accepted, since the key id is
what the witness tells operators to publish and the hex key is what the document carries. Without it
the check proves only that the attestation is internally consistent, which a forger with their own
key satisfies trivially.

## mcp

Serves the Model Context Protocol over stdio, so an agent can list templates, propose a run, and read
what happened. Every tool call is an ordinary authenticated API request carrying the token given
here, so it passes the same authorization, the same approval policy, and the same fail-closed audit
append as a request from a person. See [Agents](agents.md).

    export SWITCHTENDER_MCP_TOKEN=swt_...
    switchtender mcp --server https://switchtender.internal

The token is read from `SWITCHTENDER_MCP_TOKEN`, falling back to `SWITCHTENDER_TOKEN`. Prefer the
environment variable to a flag, whose value is visible in the host's process list. The command
refuses to start on an admin token. There is deliberately no approve tool, so an agent cannot release
its own work, and no credential, account, token, grant, or policy tool, so it cannot widen its own
reach.

| Flag | Default | Purpose |
|------|---------|---------|
| `--server` | required | SwitchTender API base URL. |
| `--allow-adhoc` | off | Also expose the ad-hoc run tool, letting the agent compose a run rather than launch a template an operator defined. Approval policy still applies. |
| `--token` | none | API token the agent presents. Prefer `SWITCHTENDER_MCP_TOKEN`. |
| `--timeout` | `1m0s` | Bounds one API call. |
| `--allow-admin-token` | `false` | Start even when the token has admin rights. An agent should hold an operator-bound token, so this is a deliberate override. |

## demo

Seeds a fresh database with sample data and real runs, then serves it read-only, so a public
instance is safe to expose. It needs ansible on the PATH to run the sample playbooks.

| Flag | Default | Purpose |
|------|---------|---------|
| `--addr` | `127.0.0.1:8080` | Address the demo listens on. Loopback by default. |
| `--db` | temporary file | Database to seed and serve. Empty uses a fresh temporary SQLite file, removed when the demo stops. |
| `--seed-only` | off | Seed the database and exit without serving. |
| `--no-seed` | off | Serve the database as it already stands instead of seeding it. |
| `--anchor-tsa` | a public authority | RFC 3161 authority that anchors the seeded chain, so the demo shows a real anchor. Empty anchors nothing. |
| `--trusted-proxy` | none | As `serve`. |
| `--client-ip-header` | none | As `serve`. |
| `--span-cadence` | `0` | As `serve`. |

Seeding runs real playbooks and takes a couple of minutes, which is a visible gap if a public
demo reseeds in place. The two flags split that work in half so it can happen off to the side:

    switchtender demo --db next.db --seed-only     # build the next database, serving continues
    mv next.db demo.db                             # swap it in
    switchtender demo --db demo.db --no-seed       # serves the prepared data in under a second

A host wired this way reseeds without a visible outage, since the running instance keeps
answering the whole time the replacement is being built.

## examples

Seeds a handful of starter templates, so a first launch works on the spot rather than opening on an
empty list. They use the Bash tool and print or read something local, needing no project, inventory,
or credential. Delete them once you have your own.

Run it against the same database `serve` uses. It skips a template whose name is already present, so
it is safe to run twice. Takes `--db`.

## backup

Writes an encrypted, portable backup of the control-plane configuration and secrets. The whole file
is sealed with the deployment encryption key, so it stays confidential and tamper-evident, and it
restores into either the SQLite or the PostgreSQL backend. See [Backup and restore](backup.md).

| Flag | Default | Purpose |
|------|---------|---------|
| `--db` | `switchtender.db` | SQLite file path, or a `postgres://` DSN, to back up. |
| `--out` | stdout | File to write the backup to. |

Run history and the audit chain are not included. The audit chain has its own signed export, through
`audit bundle`.

## restore

Reads a backup and upserts its objects into the store by id. It needs the same encryption key the
backup was written with, and it never deletes objects absent from the file.

| Flag | Default | Purpose |
|------|---------|---------|
| `--db` | `switchtender.db` | SQLite file path, or a `postgres://` DSN, to restore into. |
| `--in` | stdin | Backup file to read. |

## version

Prints the SwitchTender version.

`--verify` fetches this version's published binary hashes and verifies the running executable.

## help and completion

Both are the standard Cobra built-ins. `help` prints usage for any command, and `completion` emits a
shell completion script for bash, zsh, fish, or PowerShell.

## Output flags

JSON goes to stdout compact by default. Four commands take `--pretty` to indent it. It is not a
global flag, so passing it elsewhere is an error rather than a no-op.

| Flag | Where | Purpose |
|------|-------|---------|
| `--pretty` | `token` and its subcommands, `ansible` and its subcommands, `audit anchor`, `audit receipt` | Indent JSON output instead of the compact default. |


## Separation between organizations is opt-in

Organizations, teams, and grants exist, but with `--strict-grants` off an object nobody has granted
falls back to the caller's global role. On that default any operator may use any project, inventory,
or credential in the install, whichever organization it belongs to. That is deliberate: a small team
should not have to grant every object before anything works, and an upgrade should not lock people
out of what they were already using.

It does mean an install with several organizations on it is not separated until `--strict-grants` is
on. If you are running work for more than one team, more than one customer, or anything where one
group must not reach another's credentials, turn it on and grant deliberately. Objects created
before you do carry no grants, so plan to assign them.

A refusal from these rules says so: the body reads `forbidden: this object requires a grant you do
not hold`, which is the same sentence whether the object is delegated elsewhere, ungranted under
strict grants, or does not exist. It tells you a grant is what is missing rather than a role or an
account, and nothing about the object itself.

What `--strict-grants` decides is only the default for an object nobody has granted. An object that
carries a grant is access-controlled in both modes: writing a grant on it is what declares it so,
and a caller who does not hold one is denied whether or not strict grants are on. Every listing
answers the same rule the by-id read does, so a run you are refused by name is also absent from the
run list, the fleet and drift views, the host pages, and the change log.

One consequence is worth knowing before you write your first grant. Install-wide figures that carry
no per-row id are served whole only to a caller nothing is hidden from, because such a total is
every other group's volume in one number. On an install with no grants at all that is everybody,
which is the common case and is unaffected. It stays unaffected by a grant on a template or a worker
queue, since neither can hide a run. What changes it is a grant on a project, an inventory, or a
credential that the caller does not hold.

For a caller in that position each surface answers differently, and the difference is worth knowing
before you point a dashboard at one:

| Surface | What a restricted caller gets |
|---------|-------------------------------|
| The run-count cards on the run list | Their own visible totals, with `summary.scope` reading `visible` rather than `install`. |
| The task-duration table at `/v1/tasks` | No rows, and `withheld: true` beside them, so an empty table is not mistaken for a quiet install. |
| The Prometheus exposition at `/metrics` | An empty body with status 200. Every series is absent rather than zero, so give the scrape an admin token if you want the install's counters. |

The empty `/metrics` body is deliberate: a scraper reads a 5xx as the install being down and pages
somebody. It does mean an alert written against a series going to zero will not fire for a scrape
token that has become restricted, because the series is gone rather than zero. Scrape with a token
that holds everything, or alert on the scrape's own staleness as well as its values.

## Directory sign-in and existing accounts

An account records what created it: an administrator, or the directory that provisioned it. A
directory identity signs in to an account only when that account is its own, so an identity provider
asserting the username of a local administrator is refused rather than handed that administrator's
account and role.

This matters most when the username does not come from a stable subject. With
`--jwt-username-claim email` or a SAML username attribute pointing at an email, against an issuer
that lets a user set their own address, matching on username alone was account takeover. OIDC has
always refused the equivalent when the provider does not vouch for the address.

An account created before this was recorded carries no source, so it cannot be told apart from a
local one of the same name. Those are still signed in to, because refusing them would lock out every
directory user on upgrade, and each is logged so an administrator can set the source and remove the
ambiguity.

## Attention thresholds

The overview's Needs attention panel, `GET /v1/attention`, sorts waiting work into four answers to
what is stopping it: a lost worker, no worker for its queue, an approval, or a block behind another
run. Five thresholds decide when work shows as blocked and when each answer alerts. An alert is
listed by the doctor and told, once per condition, to the server-wide chat channels, webhooks, ntfy,
and email, to a run's own targets of those kinds, and to the named targets attached for the
`attention` event, on every kind including PagerDuty. The [API
reference](api.md#what-needs-attention) describes the answer.

| Threshold | Default | What it sets |
|-----------|---------|--------------|
| `blocked_after` | `15m` | How long a run waits behind another run or a full worker before it shows as blocked at all. It cannot be off. |
| `alert_no_worker` | `15m` | How long a run waits with no connected worker serving its queue before it alerts. |
| `alert_blocked` | `15m` | How long a run stays blocked before it alerts. |
| `alert_worker_lost` | `1m` | How long after a lost worker last reported its run alerts, when the lease sweep has not reclaimed it by then. The default is two lease periods. |
| `alert_approval` | off | How long an approval waits before it alerts. Off unless a file turns it on, since an approval waiting is the gate working. |

Each value is a Go duration such as `90s`, `15m`, or `4h`, or `off`. Zero is refused rather than
read as off. The file states the organization's defaults and may override them for one organization
on the install, one queue, or one template, and the most specific wins: a template over a queue, a
queue over an organization, an organization over the defaults. Each override replaces only what it
states. The default queue is written as `""`.

    defaults:
      alert_no_worker: 10m
      alert_approval: 8h
    orgs:
      org_ab12cd34ef56:
        alert_approval: 2h
    queues:
      prod:
        alert_no_worker: 2m
        blocked_after: 5m
      "":
        alert_blocked: 30m
    templates:
      tpl_0123456789ab:
        alert_approval: 30m

An unknown key, a value that is not a duration, and `blocked_after: off` are refused when the server
starts, so a typo never silently leaves an alert on its default or off.

## Confining relay workers to their queues

A relay worker runs in a segment the control node cannot reach, which means the least trusted machine
in the estate holds a worker token. With a single `--worker-token`, that machine may name any queue
it likes and lease from it, so a compromised host in a DMZ can take a production run and execute it
with production credentials.

`--worker-pools` binds each token to the queues it may serve. The file stores the SHA-256 of each
token, never the token itself, the same way a webhook secret is stored:

    workers:
      - name: dmz
        token_sha256: 9f2c...           # sha256 of that pool's bearer token
        queues: [dmz]
      - name: production
        token_sha256: 41ab...
        queues: [prod, canary]

A pool that declares no queues may lease from all of them, which is the single-token shape stated
out loud. A pool that declares queues is refused anything else, including the default queue when it
names none, so confinement cannot be escaped by omission.

Generate a digest with `printf %s "$TOKEN" | shasum -a 256`. A malformed file stops the server rather
than falling back to no confinement, because an install that believes it is segmented and is not is
worse than one that refuses to start.

That confines the lease side. The submit side is confined by granting the queue, which is a grantable
object named `queue:<name>`:

    curl -X POST localhost:8080/v1/grants \
      -H "Authorization: Bearer $ST_TOKEN" \
      -d '{"subject": "team_sre", "object": "queue:prod", "access": "use"}'

A queue nobody has granted follows the same rule every other object does: the global role decides,
unless `--strict-grants` is on, in which case an ungranted queue is refused. Granting a queue makes
it access-controlled, so only the subjects named may route work to it. The grant is checked wherever
a queue is chosen: on a run, on a template, on an inventory, and at launch against the template's own
queue.

An install that has not turned strict grants on can gate a queue with a rule instead, since a policy
matches on `queue`:

    policies:
      - name: hold anything headed for production
        queue: prod
        require_distinct_approver: true

A policy with `queue` holds only what is routed to that queue, and a run on a named queue executes
only when a worker serving that queue claims it. The control node's own runner takes unqueued runs
alone. On an install without relay workers, a queued run therefore waits as `pending` indefinitely,
even after approval. To gate runs the server itself executes, write the policy without `queue`.


## Delivering secrets to relay workers

A relay worker holds no database and no encryption key, so on its own it cannot open a credential,
and a run that needs one fails on it. A pool opts in to sealed delivery by registering a delivery
key. When one of its workers claims a run, the control node opens that run's secrets and nothing
else: its credentials and those of the inventory it targets, the registry login its image is pulled
with, its custom credential types' field values, from which the worker renders their files, its
secret survey answers, and, for a federated credential, an identity token minted for the run. It
seals them to the pool's key, bound to that claim, and sends them with the claim. The worker opens
them in memory when the run first needs one, writes them into the run's private directory exactly as
a worker with database access does, and removes them when the run ends, whether it succeeded, failed,
was canceled, timed out, or never started.

To opt a pool in:

1. Generate its key, on a worker or wherever you then copy the private key from:

       switchtender worker key new --out /etc/switchtender/delivery.key

2. Register the printed public key on the pool in the worker pool file every control node reads with
   `--worker-pools`:

       workers:
         - name: dmz
           token_sha256: 9f2c...
           queues: [dmz]
           delivery_key: x25519:x9f9/kkB6Jq3Wqht/7TWzDGUeTRfbABrj1CO+8/6hHc=

3. Restart each control node so it reads the file, and start every worker of the pool with the
   private key:

       switchtender worker --server https://switchtender.example.com --queue dmz \
         --delivery-key /etc/switchtender/delivery.key

The rules it follows:

- Delivery is off until a pool registers a key. A pool without one is sent nothing, and a run that
  needs a secret fails on its workers with a message naming the pool and how to register a key,
  which is what every relay worker did before delivery existed.
- Only a pool bound to explicit queues is sent secrets. A pool's queues are the runs it can claim,
  and so the runs whose secrets its key unlocks, and a pool with no queues could claim anything. A
  pool file that gives a key to a pool with no `queues`, or the same key to two pools, stops the
  server at startup.
- A worker refuses a key file another account can read, the way ssh refuses an unprotected identity
  file, and refuses to start with one.
- Credential sources resolve on the control node, so a worker in an isolated segment needs no route
  to Vault or a cloud secret manager. A dynamic secret minted for the run is revoked when the worker
  reports the run finished, or within about thirty seconds of its claim ending any other way: a
  worker that died, a run the janitor interrupted or requeued, or a finish recorded through another
  replica.
- For a Vault dynamic secret the control node also records a revoke handle in the database: the
  lease id and the Vault address, sealed with the encryption key, beside the run and the credential
  that minted it. The secret itself is never stored, and the handle is never logged or returned by
  the API. Every control node with the encryption key sweeps these records, so a secret is revoked
  once its claim ends even when the node that minted it was stopped or restarted in between. The
  revoke uses the credential's token, or `VAULT_TOKEN` for a credential that names the Vault in
  `VAULT_ADDR`, and is refused when the credential now names another Vault. A revoke that fails is
  retried with backoff until the lease would have expired anyway.
- Two kinds of dynamic source give no handle. A dynamic source a plugin adds, whether compiled in
  through the SDK or loaded as an external plugin, is revoked only by the control node that minted
  it, so when that node stops first its secret expires on its own lifetime. AWS STS credentials
  cannot be revoked early at all and always expire on their own lifetime.
- A federated credential's token is minted on the control node, whose signing key never leaves it,
  for the mode the worker will execute: a plan for a dry run or for an apply a plan-content rule plans
  first, and an apply otherwise. A worker about to execute in the other mode refuses the token.
- Anything that goes wrong is refused rather than worked around. A secret that does not open on the
  control node, a delivery that cannot be recorded in the audit trail, a delivery sealed to a key the
  worker does not hold, or one that does not open for the claim fails the run with the reason. A run
  never executes with a secret missing.

The seal is HPKE (RFC 9180) in base mode with DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, and
ChaCha20-Poly1305, from the Go standard library. HPKE was chosen over NaCl's anonymous sealed box
because it authenticates associated data, so the binding is checked by the decryption itself. Each
delivery uses a fresh ephemeral key, and the run id, a hash of the claim's lease, the worker's lease
name, the pool, the key id, and a random delivery id are all authenticated. So:

- A worker of another pool holds another key and cannot open the delivery.
- A delivery presented for another run, for a later claim of the same run, or by another worker does
  not open.
- A worker opens a given delivery once and refuses it if it is presented again.
- A worker opens a delivery only after the control node has accepted its fenced start under the same
  lease, so a claim answer replayed after its run started or finished elsewhere is never decrypted.
  The start is refused once the janitor has taken the claim back, and to a worker that never claimed
  the run.
- Only ciphertext crosses the relay, so a proxy or load balancer that logs relay traffic logs nothing
  readable without the pool's private key.

The worker holds the opened values only until it has written them where the tool reads them, then
drops them. Go cannot overwrite a string in place, so what is dropped is the reference, and the
decrypted bytes themselves are zeroed. Disable core dumps on worker hosts, as on any host that runs
credentialed work, and see [Secrets](secrets.md) for how the run's private directory is handled.

### Rotating a pool's key

A worker can hold several keys, and a delivery names the key it was sealed to, so a rotation needs no
moment where runs fail:

1. Generate the new key and install it on every worker of the pool beside the old one, restarting
   each with both: `--delivery-key old.key --delivery-key new.key`.
2. Replace the pool's `delivery_key` with the new public key in the worker pool file on every control
   node, and restart them. Deliveries are sealed to the new key from then on, and every worker opens
   them.
3. Remove the old key from the workers and restart them with the new one alone.

Each control node logs, at startup, the key id every pool registered, and each worker logs the key
ids it holds, so the step a fleet has reached shows in the logs. Every delivery's audit record names
the key id it was sealed to.

When a key may be compromised, replace the pool's `delivery_key` and restart the control nodes first,
then install the new key on the workers. Until a worker has it, credentialed runs on that pool fail
rather than run. Treat the secrets of runs the pool executed as exposed and rotate them at their
source, and rotate the pool's worker token too, since whoever read the key file could read the token
beside it.

## What a relay worker writes into the audit trail

A relay worker runs where the control node cannot see it, so two moments are recorded: the run
leaving for that machine, and the outcome coming back. A run whose secrets were delivered records a
third, between them, naming which pool and worker received which credential ids and secret answer
names, under which key, and never a value:

    RELAY  /relay/claim/run_4f21a9                       pool:dmz worker:build-dmz-01
    RELAY  /relay/delivered/run_4f21a9/key/f4e8a76a457da19cf3742de37986d439/credentials/cred_1a2b3c,cred_4d5e6f/answers/db_password
                                                         pool:dmz worker:build-dmz-01
    RELAY  /relay/finished/run_4f21a9/succeeded          pool:dmz worker:build-dmz-01

Captured output, structured events, per-host and per-task summaries, and heartbeats are not recorded.
They are the content and liveness of a run that is already stored on the run itself, they arrive
several times a second, and writing each into a hash chain would drown the record it exists to make
readable.

The claim and finish records do not fail closed, unlike every mutation through the API. Refusing a
worker's report because the audit store is unhealthy does not un-finish the run. It loses the outcome
of work that already ran on real hosts, so a failure to record is logged loudly instead. The delivery
record is the exception: it is written before anything is sent, and a delivery that cannot be
recorded is not sent, so the run fails on the worker with that reason.
