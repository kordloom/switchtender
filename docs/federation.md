<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Federated cloud credentials

A federated credential stores no secret. Each run that carries one receives a short-lived identity
token that SwitchTender signs, with claims naming the run precisely: its organization, project,
template, environment, who launched it, and whether an approver released it. AWS, Google Cloud, and
Microsoft Entra verify the token against keys SwitchTender publishes and exchange it for credentials
that expire on their own. Nothing durable is stored, so there is nothing to rotate, leak, or re-enter.

This is the same workload identity federation GitHub Actions, Terraform Cloud, Spacelift, and env0
use. It is part of Community and needs no license.

## How it works

1. SwitchTender is an OpenID Connect issuer at the URL you give it. It serves a discovery document at
   `/.well-known/openid-configuration` and its public keys at `/.well-known/jwks.json`.
2. You register that issuer with the cloud once, and write a trust policy that names which runs may
   assume which role, keyed on the token's claims.
3. When a run starts, SwitchTender mints a token for it, valid for minutes, and hands it to the tool.
4. The tool, or SwitchTender when you choose the exchange delivery, trades the token for cloud
   credentials. The cloud fetches the public keys to verify the signature.

SwitchTender makes no outbound call for any of this to work. The cloud fetches the keys, and with the
default file delivery the tool performs the exchange.

## Turning it on

Set the issuer URL on `serve` and on every `worker` that shares its database:

    switchtender serve --federation-issuer https://switchtender.example.com

or `SWITCHTENDER_FEDERATION_ISSUER=https://switchtender.example.com` in the environment file every
process reads. The URL must be https, reachable by the cloud, and served with a certificate the cloud
trusts. It may carry a path when a proxy serves SwitchTender under one. The token's `iss` claim is
this URL exactly, with no trailing slash.

Federation needs `SWITCHTENDER_ENCRYPTION_KEY` and `SWITCHTENDER_ENCRYPTION_SALT`, because the signing
key is sealed under them. They live outside the database, in the environment of each process, so the
database alone cannot open the signing key. A server given an issuer URL and no encryption key refuses
to start rather than run with federation it cannot sign for.

On start, `serve` generates the first signing key if there is none, so a cloud you configure before
the first run already finds a key to read.

A relay worker started with `--server` holds no signing key and mints nothing. When its pool has
registered a delivery key, the control node mints the run's token at claim, for the mode the worker
will execute, and seals it to the pool's key with the run's other secrets. The worker writes the
token file or runs the exchange exactly as above, and refuses a token minted for the other mode, so a
token whose claims say plan never authorizes an apply. A relay worker whose pool registered no key
fails a federated run with the reason. See
[Delivering secrets to relay workers](configuration.md#delivering-secrets-to-relay-workers).

## Credential kinds

A federated credential has settings and no secret. Create one on the Credentials page or with:

    POST /v1/credentials
    {
      "name": "aws-prod-deploy",
      "kind": "aws_oidc",
      "settings": {
        "role_arn": "arn:aws:iam::123456789012:role/switchtender-deploy",
        "region": "us-east-1",
        "environment": "prod"
      }
    }

A secret, a passphrase, or an external source sent with a federated kind is refused, and so is a
settings key the kind does not read, so a misspelling fails when the credential is saved.

| Kind | Required settings | Optional settings | What the run receives |
|------|-------------------|-------------------|-----------------------|
| `aws_oidc` | `role_arn` | `region`, `delivery`, `audience`, `session_name`, `session_duration`, `sts_endpoint`, `token_ttl`, `environment` | With the file delivery, `AWS_ROLE_ARN`, `AWS_WEB_IDENTITY_TOKEN_FILE`, and `AWS_ROLE_SESSION_NAME`. With the exchange delivery, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and `AWS_SESSION_TOKEN`. `AWS_REGION` and `AWS_DEFAULT_REGION` when a region is set. |
| `gcp_oidc` | `provider` | `service_account`, `delivery`, `audience`, `sts_endpoint`, `iam_endpoint`, `token_ttl`, `environment` | With the file delivery, an `external_account` credentials file in `GOOGLE_APPLICATION_CREDENTIALS` and `CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE`, and `GCP_AUTH_KIND=application`. With the exchange delivery, the access token in `GOOGLE_OAUTH_ACCESS_TOKEN` and `GCP_ACCESS_TOKEN`, and `GCP_AUTH_KIND=accesstoken`. The Ansible `google.cloud` collection reads the last two. |
| `azure_oidc` | `client_id`, `tenant_id` | `subscription_id`, `audience`, `authority_host`, `token_ttl`, `environment` | `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, and `AZURE_FEDERATED_TOKEN_FILE`, the workload identity convention, and `ARM_CLIENT_ID`, `ARM_TENANT_ID`, `ARM_USE_OIDC=true`, and `ARM_OIDC_TOKEN_FILE_PATH` for Terraform's azurerm provider. The subscription as `AZURE_SUBSCRIPTION_ID` and `ARM_SUBSCRIPTION_ID` when set. |
| `oidc_token` | `audience` | `token_ttl`, `environment` | The path of a file holding the token in `SWITCHTENDER_OIDC_TOKEN_FILE`, for any other relying party, such as HashiCorp Vault's JWT auth method. The token itself is never put in a variable, which every process the run starts would inherit. |

The settings shared by every kind:

| Setting | Default | Meaning |
|---------|---------|---------|
| `audience` | `sts.amazonaws.com` for AWS, `https://iam.googleapis.com/` followed by the provider for Google Cloud, `api://AzureADTokenExchange` for Azure | The token's `aud` claim. It must match what the cloud expects. |
| `token_ttl` | `15m` | How long the token is valid, from `1m` to `1h`. |
| `environment` | none | The `environment` claim and the `env` segment of the subject. Letters, digits, dots, hyphens, and underscores. |
| `delivery` | `file` | `file` or `exchange`, for `aws_oidc` and `gcp_oidc`. |

The AWS settings: `role_arn` is the role to assume in any partition. `session_name` names the session in
CloudTrail and defaults to `switchtender-` followed by the run id, `switchtender-gate-` followed by
the run id for the approval gate's module download, and `switchtender-review-` followed by the pull
request number for a review's pre-check, which no run exists for. `session_duration` is the session
length in seconds from 900 to 43200 and applies to the exchange delivery. `sts_endpoint` overrides the
STS endpoint, for a FIPS, GovCloud, China, or VPC endpoint. With the exchange delivery SwitchTender sends
the token there itself. With the file delivery the tool makes the exchange, and the setting reaches it
as `AWS_ENDPOINT_URL_STS`. boto3, which the Ansible `amazon.aws` collection uses, the AWS SDK for Go
v2, and the Terraform and OpenTofu S3 state backends send the exchange to that endpoint (checked with
botocore 1.43, the Go SDK's config module 1.32, Terraform 1.9.8, and OpenTofu 1.12.6). For any other
tool, the Terraform AWS provider included, confirm that it reads the variable or set the endpoint in the
tool's own configuration, or choose the exchange delivery.

The Google Cloud settings: `provider` is the workload identity pool provider, as
`projects/NUMBER/locations/global/workloadIdentityPools/POOL/providers/PROVIDER` with or without the
`//iam.googleapis.com/` prefix. `service_account` is a service account to impersonate with the
federated token, which the file delivery writes into the credentials file and the exchange delivery
performs itself. `sts_endpoint` and `iam_endpoint` override the token service URL and the IAM
credentials service base URL, for Private Service Connect.

The Azure settings: `client_id` and `tenant_id` name the Entra application and its tenant.
`authority_host` sets `AZURE_AUTHORITY_HOST` for a sovereign cloud.

A run may carry one federated credential of each kind. Two of the same kind would set the same
variables, one silently replacing the other, so the second is refused when the run is submitted.

## Choosing a delivery

The default is `file`. The token is written to a file only the run's user can read, and the tool's
own SDK exchanges it. SwitchTender makes no call to the cloud. This is the delivery that works for
Ansible, Terraform, OpenTofu, and scripts alike, because every one of them reaches the cloud through an
SDK that reads these variables:

- AWS: boto3, which the Ansible `amazon.aws` collection and the AWS CLI use, the AWS SDK for Go that
  the Terraform and OpenTofu AWS provider and S3 backend use, and the other AWS SDKs all assume a
  role from `AWS_ROLE_ARN` and `AWS_WEB_IDENTITY_TOKEN_FILE`.
- Google Cloud: the Google client libraries, gcloud, the Terraform google provider, and the Ansible
  `google.cloud` collection with `auth_kind: application` all read an `external_account` file. With the
  exchange delivery the collection's modules, its `gcp_compute` inventory plugin, and its lookups take
  `GCP_AUTH_KIND=accesstoken` and the token in `GCP_ACCESS_TOKEN` as their defaults (checked in
  `google.cloud` 1.14.0).
- Azure: the Azure Identity libraries and Terraform's azurerm provider read the variables above. The
  Azure CLI does not, so a script that uses it signs in first:

      az login --service-principal -u "$AZURE_CLIENT_ID" -t "$AZURE_TENANT_ID" \
        --federated-token "$(cat "$AZURE_FEDERATED_TOKEN_FILE")"

Choose `exchange` for a tool that cannot read a token file, such as a script that calls a cloud API
directly. SwitchTender then calls AWS STS `AssumeRoleWithWebIdentity`, or Google's security token
service and, with a service account, the IAM credentials service, as the run starts, and injects the
credentials those return. The call needs no AWS or Google key: the token is the authentication. This
is the only case in which SwitchTender itself contacts a cloud, and it happens on the process that
executes the run.

The token is minted once, when the run starts, and is not refreshed. A tool that first reaches the
cloud late in a long run must do so within `token_ttl`, so raise it to cover that, up to an hour. Once
exchanged, the cloud credentials last for the role's session length. A run that outlives that session
needs a longer session on the role, not a longer token.

## The token

Every token is a JWT signed with RS256. Every claim below is present on every token, empty when it does
not apply, so a policy condition never meets a missing claim.

| Claim | Meaning |
|-------|---------|
| `iss` | The issuer URL. |
| `sub` | The subject, described below. |
| `aud` | The audience the credential sets. |
| `exp`, `iat`, `nbf` | Expiry, issue time, and not-before time, in seconds since the epoch. |
| `jti` | A unique token id. |
| `run_id` | The run the token was minted for, empty for a review's pre-check, which no run exists for. |
| `purpose` | Why the token was minted: `run` for a run executing, `gate_download` for the module download the approval gate runs with the run's credentials before it records the run, or `review_precheck` for the module download a pull request review runs before it plans. |
| `pull_request` | The pull or merge request number a review's plan or pre-check is for, empty otherwise. |
| `parent_run_id` | The pipeline or split run this run is a step or shard of, empty for a top-level run. |
| `org_id` | The organization that owns the run. |
| `project_id` | The git project the run reads from. |
| `template_id` | The job template the run executes. It is stamped when a template is launched directly, by a schedule, or by a trigger, and carried to every run derived from it. |
| `environment` | The environment the credential names. |
| `credential_id` | The federated credential the token was minted for. |
| `tool` | `ansible`, `terraform`, `opentofu`, `bash`, `powershell`, `python`, or `go`. |
| `run_type` | `apply`, or `dry_run` for a run in its tool's no-change mode, including the plan a plan-content policy runs before a gated apply. |
| `source` | What fired the run: `api`, `template`, `schedule`, `trigger`, `rerun`, `reconcile`, or `propose`. |
| `commit_sha` | The project commit the run executes. |
| `launcher_type` | `person`, `agent`, `schedule`, `trigger`, `pipeline`, or `system`. |
| `actor`, `actor_type`, `actor_user_id` | The launching credential's name, how it authenticated, and the account behind it. |
| `approved` | `true` when an approval decision released the run. |
| `approved_by`, `approved_by_type` | The approver the audit chain recorded, and how they authenticated. |
| `approval_policy` | The approval rule that held the run. |

A token can be minted before a run exists. The approval gate downloads the modules a Terraform or
OpenTofu plan calls with the run's own credentials before it records the run, and a pull request
review does the same before it plans, to learn whether the plan is safe to run at all. The gate's
token carries `purpose` `gate_download` and the id the run will be recorded under, so a run id with
no run behind it, because the gate refused the run, says why. A review's pre-check carries `purpose`
`review_precheck`, names no run, and names the pull request in `pull_request` and the commit it read
in `commit_sha`. Both carry the subject the run itself would, so a trust policy grants the download
exactly the access the run would have.

A shard or a pipeline step reads `launcher_type`, the actor, and the approval from the run that was
launched, since that run's requester and that run's approval are what released it. `approved` is true
only when the decision was recorded in the audit chain and bound to the run's spec, and execution has
already refused a run whose spec moved since.

### The subject

The subject joins six pairs with colons, broadest first:

    org:<org_id>:project:<project_id>:template:<template_id>:env:<environment>:run_type:<apply|dry_run>:approved:<true|false>

For example:

    org:org_acme:project:proj_web:template:tpl_deploy:env:prod:run_type:apply:approved:true

An empty part reads `none`. The parts are ids rather than names, because a name can be taken by
renaming another object and an id cannot. Find each id on its page in the interface or in the API. A
value that holds a colon or a wildcard character is refused, so no part can shift another.

The `environment` comes from the credential, set by whoever manages credentials, and never from the
run's labels, which whoever launches a run chooses.

## Trust policy examples

The examples use the issuer `https://switchtender.example.com`.

### AWS

Register the issuer once:

    aws iam create-open-id-connect-provider \
      --url https://switchtender.example.com \
      --client-id-list sts.amazonaws.com

Then give the role a trust policy. This one lets only approved apply runs of one template in the
production environment assume it:

    {
      "Version": "2012-10-17",
      "Statement": [{
        "Effect": "Allow",
        "Principal": {
          "Federated": "arn:aws:iam::123456789012:oidc-provider/switchtender.example.com"
        },
        "Action": "sts:AssumeRoleWithWebIdentity",
        "Condition": {
          "StringEquals": {
            "switchtender.example.com:aud": "sts.amazonaws.com",
            "switchtender.example.com:sub":
              "org:org_acme:project:proj_web:template:tpl_deploy:env:prod:run_type:apply:approved:true"
          }
        }
      }]
    }

A read-only role for plans of any template in the project uses a pattern:

    "StringLike": {
      "switchtender.example.com:sub": "org:org_acme:project:proj_web:template:*:env:prod:run_type:dry_run:*"
    }

AWS evaluates conditions on `aud` and `sub` for an external issuer, which is why the subject carries
the approval and the run type.

### Google Cloud

Create a pool and a provider that maps the claims, and refuses tokens from any other organization:

    gcloud iam workload-identity-pools create switchtender --location=global

    gcloud iam workload-identity-pools providers create-oidc switchtender \
      --location=global --workload-identity-pool=switchtender \
      --issuer-uri=https://switchtender.example.com \
      --attribute-mapping="google.subject=assertion.sub,attribute.template_id=assertion.template_id,attribute.environment=assertion.environment,attribute.approved=string(assertion.approved)" \
      --attribute-condition="assertion.org_id == 'org_acme'"

Then let approved production runs of one template impersonate a service account:

    gcloud iam service-accounts add-iam-policy-binding deployer@my-project.iam.gserviceaccount.com \
      --role=roles/iam.workloadIdentityUser \
      --member="principal://iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/switchtender/subject/org:org_acme:project:proj_web:template:tpl_deploy:env:prod:run_type:apply:approved:true"

The credential's settings are then
`provider=projects/123456789/locations/global/workloadIdentityPools/switchtender/providers/switchtender`
and `service_account=deployer@my-project.iam.gserviceaccount.com`. Google Cloud can also be given the
key set directly when it cannot reach the issuer: save the output of
`curl https://switchtender.example.com/.well-known/jwks.json` and pass it to the provider with
`--jwk-json-path`, and upload it again after each rotation.

### Azure

Add a federated identity credential to the Entra application. Entra matches the subject exactly, so
add one per subject a role should accept:

    az ad app federated-credential create --id <application-id> --parameters '{
      "name": "switchtender-prod-deploy",
      "issuer": "https://switchtender.example.com",
      "subject": "org:org_acme:project:proj_web:template:tpl_deploy:env:prod:run_type:apply:approved:true",
      "audiences": ["api://AzureADTokenExchange"]
    }'

Grant the application its role assignment as usual. The credential's settings are `client_id`, the
application id, and `tenant_id`.

## Signing keys

The signing key is a 2048 bit RSA key of its own, not the audit chain's key and not the license key, so
a key that signs run identity can be rotated on its own schedule and its loss forges no evidence. The
key id is the RFC 7638 thumbprint of the public key.

### Where the key lives

Each key's private half is sealed with the server's encryption key, `SWITCHTENDER_ENCRYPTION_KEY` with
`SWITCHTENDER_ENCRYPTION_SALT`, and stored in the database beside its public half. The server and every
worker on that database sign with the same key, and the server publishes it. The encryption key itself
lives outside the database, in the environment of each process, so a copy of the database cannot open
a signing key. Keep the encryption key with your other root secrets and apart from database backups.

The private half never appears in an API response, a log line, a backup, or an evidence entry. The key
listing and the published key set carry public halves and times only, backups leave the keys out, and
the audit chain names a key by its id alone.

### The four times

Every key records four times:

| Time | Meaning |
|------|---------|
| Created | The key was generated and published. |
| Activated | The key starts signing. |
| Retired | The key stops signing and stays published. |
| Removed | The key leaves the published set, and its private half is erased. |

A key is pending from its creation until it activates, signing until it retires, retired until it is
removed, and removed after that. A removed key stays listed with its public half and its times, as the
record of when it was trusted. `GET /v1/federation/keys` lists every key with its state and its four
times, and the Credentials page shows the same table under Federation signing keys, with both
rotations beside it for an admin.

### Normal rotation

    POST /v1/federation/keys/rotate

A normal rotation is for routine hygiene. It has two phases, on fixed times rather than settings:

1. At once, the new key is published and does not sign. The current key keeps signing.
2. 24 hours later, the new key starts signing and the old key retires. The old key stays published for
   another 24 hours and is then removed.

A cloud has a day to fetch the new key before any token needs it, and every token the old key signed
expires, an hour at most after it was signed, while that key is still published. The whole schedule is
written when the rotation is asked for, so the server and every worker read it from the database and
switch at the same moment with nothing left to trigger. A normal rotation asked for while another is
under way is refused with `409`, naming the pending key and the time it starts signing.

A rotation is one decision for every server and worker on the database. It is written in a single
transaction under a lock all of them take, so two rotations asked for at once on two servers start
one and the second is refused with `409`, and a rotation cut off partway, by a crash or a lost
connection, leaves the keys as they were for the retry. Every process reads the schedule against the
database clock rather than its own, so a host whose clock is wrong neither switches keys early nor
erases a key before its time.

Where a cloud is given the key set directly instead of fetching it, as Google Cloud can be with
`--jwk-json-path`, upload the set again within the day after a rotation, so the new key is in place
before it signs.

### Emergency rotation

    POST /v1/federation/keys/rotate/emergency

An emergency rotation is for a key that may be compromised. A new key signs at once, and every other
key leaves the published set at the same moment with its private half erased, not only the key that
was signing. That means the key that was signing, a key a normal rotation had published and not yet
started signing with, and any retired key still published. They were all stored the same way, so a
suspected compromise of one is treated as a compromise of all.

A removed key stays removed. No server or worker signs with it again, even one that read the keys
just before the rotation, none publishes it again whatever its own clock says, and the database
refuses any write that would give back its private half or postpone its removal. A normal rotation
that was under way when the emergency rotation ran is removed with the rest.

The cost is availability. Nothing a removed key signed verifies again, which is the point, and for a
while some runs fail:

- A run already holding a token from a removed key fails once the cloud reads the new key set.
- A run that starts before a cloud has read the new key set fails until it has. When that happens is
  up to the cloud, so retry the runs that failed.

Use it when a forged token would cost more than failed runs, and a normal rotation for everything else.
After an emergency rotation, upload the key set again anywhere a cloud was given it directly. Both
rotations are admin only and recorded in the audit chain, each under its own path, so the chain tells
an emergency rotation from a routine one.

### Evidence for every token

Each token is recorded in the audit chain before it leaves the server, in an entry whose method is
`TOKEN` and whose path is

    /runs/<run>/federation/<credential>/kid/<key id>/jti/<token id>/exp/<expiry>

with `/parent/<run>` added for a pipeline step or a shard. The entry names the key that signed the
token by its id and the token by its `jti`, and holds neither the token nor any key material. When the
entry cannot be written the token is withheld and the run fails, so no token reaches a tool or a cloud
without its signing key on record. A token sent to a cloud by the exchange delivery is recorded the
same way, even when the cloud refuses it.

A run's receipt and its evidence dossier carry these entries, and the dossier lists them under Identity
tokens issued. The audit trail shows each one as the run, the credential, and the signing key, so the
tokens a key signed, for example before an emergency rotation, can be picked out of the trail or its
export by the key's id.

### Backups and restores

Backups leave the keys out. A restored install generates its own key when it starts, and that key
signs at once, the way an emergency rotation's key does, so a cloud verifies the restored install's
tokens once it has read the new key set.

## Security model

- Nothing that authenticates to a cloud is stored. A federated credential holds settings, which are
  not secret and return from the API. The signing key is the one secret, sealed at rest with the
  encryption key, which lives outside the database.
- A token lives for `token_ttl`, fifteen minutes unless the credential says otherwise and never more
  than an hour, and names one run. A token copied out of a run is worth that run's narrow scope for
  minutes.
- Token files live in the run's one private credential directory, the same one every other
  materialized credential uses, created mode 0700 with each file mode 0600. The directory is removed
  when the run ends, whether it succeeds, fails, is canceled, or times out, and when the run fails
  before the tool starts. A process killed mid-run leaves it locked by nobody, and a sweep by any
  server or worker on the host removes it, as [Run files](run-files.md) describes for every
  credential file.
- The token and every credential an exchange returns are masked out of the run's log, live stream,
  and events. Neither is written to the run record, the audit chain, a receipt, a dossier, an API
  response, or MCP output. The chain records each token's `jti` and the id of the key that signed it,
  never the token. An exchange refusal is reported with the cloud's reason and with the token cut out
  of it.
- Whoever can launch a run with the credential can obtain a token for it, so the trust policy is where
  scope is decided: key it on the template, the environment, and, for production, `approved:true`.
  The subject uses ids, the environment comes from the credential, and approval comes from the audit
  chain, so none of them is chosen by whoever launches the run.
- The discovery document and key set are public and unauthenticated, as a cloud needs them to be. They
  hold public keys only.
- The exchange delivery sends the token only to the token service the credential names, over https,
  refuses redirects, and refuses the cloud metadata addresses. With `SWITCHTENDER_EGRESS_PROXY` set,
  the exchange leaves through that proxy, and only after the token service's address passes the same
  check, so a proxy never stands in for the check.
