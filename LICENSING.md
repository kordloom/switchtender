# Licensing

SwitchTender is source-available under the [Business Source License 1.1](LICENSE). This page explains
what that means in practice: what you can do for free, the single thing you cannot, and how to
arrange a commercial license.

## What self-hosting grants you

Run the Community binary anywhere, in production, at any scale, for free. Community is complete
for leaving AWX and running governed automation: no seat cap, no host cap, no execution cap, no
expiry, and no license key. It never requires KordLoom infrastructure to operate: no license
server, no online activation, no phone-home:

- All execution engines: Ansible, Terraform, OpenTofu, Bash, PowerShell, Python, and Go.
- One-command importers: AWX, AAP, Tower, Ascender, Semaphore, Rundeck, Jenkins, and crontabs.
- Role-based access control, per-object grants, organizations, and teams, plus JWT sign-in.
- The whole evidence engine: the tamper-evident hash chain, RFC 3161 anchoring, signed per-run
  receipts, run dossiers, and offline verification with the open verifier. The proofs are free
  forever, on every tier.
- One approval policy, with each approval bound to the exact content digest and pinned commit
  the approver saw. An agent behind the MCP gate faces that same approval and can never release
  its own work.
- Sealed credentials, decrypted only at execution, through nine external managers: HashiCorp
  Vault (static and dynamic), AWS Secrets Manager, AWS STS, Google Secret Manager, Azure Key
  Vault, CyberArk Conjur, CyberArk CCP, and 1Password Connect, plus any store through a command.
- Notifications over eleven channels: webhook, Slack, Mattermost, Rocket.Chat, Discord, Microsoft
  Teams, ntfy, PagerDuty, Grafana, Twilio SMS, and email, plus retention policies.
- Drift detection, the live host-by-task matrix, balanced splits, fleet memory, pipelines,
  schedules, and webhooks.

Internal production use by a company, on its own hosts, for its own infrastructure, is fully covered
at no cost.

The commercial model is open core, and paid features ship in the same binary, unlocked by a
small signed file verified offline against a key compiled into the binary.

Pro, at $490 a year flat per organization to 250 hosts, adds directory sign-in (OIDC, SAML,
and LDAP, with group-to-role mapping and just-in-time provisioning) and five approval
policies instead of one. That is deliberately the same price the rest of this market charges
for single sign-on, because a tier nobody can afford to cross is not a tier.

Team adds the full policy engine (unlimited policies, outright denials, risk floors,
agent-scoped rules, and distinct-approver separation of duties), the period change register,
distributed workers, initializing a new PostgreSQL database for active-active high availability,
and one-click drift reconcile. No license server, no activation, no phone-home, no seat counting,
and fleet bands are self-reported and never audited. A lapsed license takes nothing: opening an
existing PostgreSQL database is never gated, in any state, and every Community feature keeps
working. Enterprise adds services that by definition come from outside your install, such as the
hosted witness and evidence custody.

Seven commitments, held for every user from day one. What is free today stays free, and the
Community tier never shrinks. A lapsed license takes nothing: data, evidence, receipts, and every
Community feature keep working, and only paid features stop. No phone-home, no seat counting, no
audits, ever. Every receipt verifies without us, forever, with the open verifier. Your price is
fixed for the term, with 60 days notice before any renewal change. Every release converts to
Apache 2.0 two years after it ships. And pricing is flat per organization within a band, never per
seat and never per run.

## The one reserved right

You may not offer SwitchTender to third parties as a hosted or managed service that provides its
primary functionality. In plain terms: you cannot take SwitchTender, put it behind a login, and sell
it to other people as "SwitchTender in the cloud." That right is reserved for the maintainer.

This restriction reaches service providers and resellers. It does not reach you if you run
SwitchTender for your own organization, build it into an internal platform your own teams use, or
modify it for internal purposes.

## When you need a commercial license

Reach out for a commercial license if you want to:

- Offer SwitchTender, or a service whose primary functionality is SwitchTender's, to third parties.
- Embed SwitchTender in a product you distribute or sell.
- Lift the hosted-service restriction for any other reason.

## What KordLoom sells

KordLoom charges for services around SwitchTender, never for permission to automate. None of these
is required to use the software, and the binary is complete without every one of them:

- **Migration.** A fixed-scope, founder-led engagement that moves you off AWX, Semaphore, or
  Rundeck and into production on SwitchTender.
- **Governance.** Coordination for approvers who sit outside your install, and pinned, scanned
  execution images maintained for your fleet, plus a private support channel with a written
  response time.
- **Assurance.** Evidence operations on your audit calendar, control mappings kept current as
  guidance shifts, long-term custody with re-anchoring so old receipts still verify, and priority
  support with an SLA.
- **Hosted SwitchTender**, for teams that would rather not self-host, under the reserved right
  described above.

See [switchtender.com/pricing](https://switchtender.com/pricing) for what each includes.

## Conversion to open source

The Business Source License is time-limited. Each released version converts to the Apache License
2.0 two years after that version's release, the Change Date recorded in `LICENSE`. After a version
converts, the reserved right no longer applies to it.

## Getting in touch

Email licensing@switchtender.com to start a conversation about a commercial license, support, or
hosting. You can also open an issue at https://github.com/kordloom/switchtender, though email keeps
your inquiry private.
