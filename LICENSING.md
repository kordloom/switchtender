# Licensing

SwitchTender is source-available under the [Business Source License 1.1](LICENSE). This page explains
what that means in practice: what you can do for free, the single thing you cannot, and how to
arrange a commercial license.

## What self-hosting grants you

Run the binary anywhere, in production, at any scale, for free. Every feature SwitchTender ships is in
that binary. There is no separate paid build, nothing is unlocked with a license key, and there are no
seat, host, or execution caps. The self-hosted platform never requires KordLoom infrastructure to
operate: no license server, no online activation, no phone-home:

- All execution engines: Ansible, Terraform, OpenTofu, Bash, PowerShell, Python, and Go.
- Single sign-on through OIDC, SAML, LDAP, and JWT, with directory group-to-role mapping.
- Role-based access control, per-object grants, and teams.
- The tamper-evident, hash-chained audit trail with signed, offline-verifiable export.
- The whole policy engine: approval gates, per-agent rules, risk floors, denials, separation of
  duties, and N-of-M sign-off, with each approval bound to the exact content digest and pinned
  commit the approver saw.
- SCIM provisioning alongside single sign-on.
- Active-active high availability on PostgreSQL.
- External secrets through nine managers: HashiCorp Vault (static and dynamic), AWS Secrets
  Manager, AWS STS, Google Secret Manager, Azure Key Vault, CyberArk Conjur, CyberArk CCP, and
  1Password Connect, plus any store through a command.
- Notifications over eleven channels: webhook, Slack, Mattermost, Rocket.Chat, Discord, Microsoft
  Teams, ntfy, PagerDuty, Grafana, Twilio SMS, and email, plus retention policies.

Internal production use by a company, on its own hosts, for its own infrastructure, is fully covered
at no cost.

Two commitments follow from this, and they are permanent. Nothing that ships free today ever becomes
paid: whatever KordLoom sells is never carved back out of the binary you already run, so what
shipped free stays free in that release and every release after it. And the verifier stays open: the LoomSeal library that checks a
receipt is Apache 2.0 and independent of this project, so your evidence remains verifiable offline
whatever happens to KordLoom.

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
