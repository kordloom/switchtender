# Licensing

Railwarden is source-available under the [Business Source License 1.1](LICENSE). This page explains
what that means in practice: what you can do for free, the single thing you cannot, and how to
arrange a commercial license.

## What self-hosting grants you

Run the binary anywhere, in production, at any scale, for free. Every feature Railwarden ships is in
that binary. There is no separate paid build and nothing is unlocked with a license key:

- All execution engines: Ansible, Terraform, OpenTofu, Bash, PowerShell, Python, and Go.
- Single sign-on through OIDC, SAML, LDAP, and JWT, with directory group-to-role mapping.
- Role-based access control, per-object grants, and teams.
- The tamper-evident, hash-chained audit trail with signed, offline-verifiable export.
- Approval gates on high-risk runs.
- Active-active high availability on PostgreSQL.
- External secrets through Vault, AWS Secrets Manager, and cloud KMS.
- Notifications over email, Slack, and webhook, plus retention policies.

Internal production use by a company, on its own hosts, for its own infrastructure, is fully covered
at no cost.

## The one reserved right

You may not offer Railwarden to third parties as a hosted or managed service that provides its
primary functionality. In plain terms: you cannot take Railwarden, put it behind a login, and sell
it to other people as "Railwarden in the cloud." That right is reserved for the maintainer.

This restriction reaches service providers and resellers. It does not reach you if you run
Railwarden for your own organization, build it into an internal platform your own teams use, or
modify it for internal purposes.

## When you need a commercial license

Reach out for a commercial license if you want to:

- Offer Railwarden, or a service whose primary functionality is Railwarden's, to third parties.
- Embed Railwarden in a product you distribute or sell.
- Lift the hosted-service restriction for any other reason.

## Support and hosted plans

Commercial support with response-time guarantees, and a hosted Railwarden option for teams that
would rather not self-host, are offered separately from the license. These are paid services, not a
requirement to use the software. If either would help your team, get in touch.

## Conversion to open source

The Business Source License is time-limited. Each released version converts to the Apache License
2.0 on the Change Date recorded in `LICENSE` (2030-07-07) or four years after that version's
release, whichever comes first. After a version converts, the reserved right no longer applies to
it.

## Getting in touch

Email licensing@railwarden.dev to start a conversation about a commercial license, support, or
hosting. You can also open an issue at https://github.com/dcadolph/railwarden, though email keeps
your inquiry private.
