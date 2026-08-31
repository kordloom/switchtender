# Vendor risk review

The answers a security questionnaire asks about SwitchTender and about KordLoom, in the order
reviews usually ask them. Each answer links to the page that carries the detail, and every claim
here is checkable against the source, the signed releases, or your own running install.

## The vendor

KordLoom LLC, a United States company. SwitchTender is source-available under BSL 1.1: read,
modify, self-host, and run it in production for your own organization, free. Each release
converts to Apache 2.0 two years after it ships. The bus-factor question is answered in full on
the [continuity page](continuity.md): the install keeps running, the evidence stays verifiable
offline forever, and every release carries its own source tarball inside the signed manifest.

## Where it runs and what data leaves

Self-hosted, one binary, your infrastructure. There is no SaaS control plane, no telemetry, no
usage reporting, no license server, and no update phone-home. Outbound connections happen only
when you configure them, and each is optional:

| Connection | When | Where |
| --- | --- | --- |
| Timestamp anchoring | You run `switchtender audit anchor` | The RFC 3161 timestamp authority you name; the default is freetsa.org |
| Witness countersignatures | You point the install at a witness | The hosted witness, or one you run yourself; the witness command ships in this binary |
| Advisory AI | You configure a provider | Your provider, including local Ollama; read-only, executes nothing |
| Project sync | You add a git project | Your repository host |
| Notifications and webhooks | You configure them | Your endpoints |

## Authentication and access

SSO over OIDC, SAML, and LDAP, with JIT provisioning and group-to-role mapping. Role-based
access control with per-object grants, organizations, and teams. API access is bearer tokens; an
agent's token can be bound to an account so its actions attribute to both. Details in
[configuration](configuration.md).

## Secrets

Secrets are stored sealed, masked in run logs, and injectable from external managers: Vault
(static and dynamic), AWS Secrets Manager, Google Secret Manager, Azure Key Vault, Conjur,
CyberArk CCP, and 1Password, plus AWS STS and generic command and HTTP connectors. The
[secrets page](secrets.md) covers masking rules and injection boundaries.

## Change control and audit

Every change lands on a tamper-evident hash chain or is refused: an entry that cannot be
recorded blocks the change rather than proceeding unrecorded. Approvals bind the exact content
digest and pinned commit the approver saw, separation of duties is enforced at two layers, AI
agents pass the same gates under their own policy rules, and every run can produce a signed
receipt a relying party verifies offline with the open verifier. The [threat model](threat-model.md)
states the claims and their mechanisms, including what the record cannot prove. The
[compliance mapping](compliance.md) walks SOC 2 CC8.1, change-management ITGCs, and log-integrity
controls row by row, and the [sample evidence pack](sample-evidence-pack.md) shows the artifact
itself.

## Supply chain

Releases are built in CI from a tagged public tree. The checksums manifest is signed with
cosign (keyless, verifiable against the repository identity), artifacts carry SLSA build
provenance verifiable with `gh attestation verify`, archives include SBOMs, and the binary
verifies itself against the signed manifest with `switchtender version --verify`. GitHub Actions
are SHA-pinned. The release gate runs the full suite against both database backends and refuses
to publish on any failure.

## Vulnerability management

`govulncheck` runs in CI on every change, and the Go toolchain floor is raised when a stdlib
CVE lands in the dependency path. Reports go to the address on the [security policy](https://github.com/kordloom/switchtender/security).
There is no auto-update: you choose when to upgrade, and `switchtender version --verify` proves
what you are running afterward.

## Data retention and portability

Your data sits in your SQLite file or Postgres database, covered by your own backups
([backup and restore](backup.md)). Everything exports: runs, hosts, audit bundles, receipts,
evidence registers, CSV from every table in the UI. Exported evidence verifies offline forever,
without this vendor.

## Subprocessors

None in the self-hosted product. If you use the hosted witness, KordLoom operates that single
service; the anchor default, freetsa.org, is a third party you can replace with any RFC 3161
authority, including your own.
