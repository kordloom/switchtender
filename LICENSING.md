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
- One-command importers: AWX, AAP, Tower, Ascender, Semaphore, Chef, Puppet, Rundeck, Jenkins,
  and crontabs.
- Role-based access control, per-object grants, organizations, and teams, with local accounts.
  Directory sign-in, which is OIDC, SAML, LDAP, and JWT, is a Pro feature: the server refuses to
  start with any of them configured unless a Pro or higher license is installed.
- The whole evidence engine: the tamper-evident hash chain, RFC 3161 anchoring, signed per-run
  receipts, run dossiers, and offline verification with the open verifier. The proofs are free
  forever, on every tier.
- The built-in hold on the runs an agent token asks for, and one plain rule of your own: either a
  require-approval policy or one exemption. Each approval is bound to the exact content digest and
  pinned commit the approver saw, and an agent behind the MCP gate can never release its own work.
- Sealed credentials, decrypted only at execution, through nine external managers: HashiCorp
  Vault (static and dynamic), AWS Secrets Manager, AWS STS, Google Secret Manager, Azure Key
  Vault, CyberArk Conjur, CyberArk CCP, and 1Password Connect, plus any store through a command.
- Notifications over eleven channels: webhook, Slack, Mattermost, Rocket.Chat, Discord, Microsoft
  Teams, ntfy, PagerDuty, Grafana, Twilio SMS, and email, plus retention policies.
- Drift detection, the live host-by-task matrix, balanced splits, fleet memory, pipelines,
  schedules, and webhooks.

Internal production use by a company, on its own hosts, for its own infrastructure, is fully covered
at no cost.

The commercial model is open core. The Community build is this repository. Paid features live in a private module, and the paid build that carries them is a separate download, unlocked by a small signed file verified offline against a key compiled into the binary.

The paid code is kept in a private repository, mounted at internal/paid/impl as a git submodule that public clones do not fetch. A Community build refuses each paid feature by naming the tier it needs. The paid engine is not there to patch a key out of. Rules scoped to an actor, an agent, or an account use the same matching code every build carries, because free exemptions are decided on it. A Community build cannot author such a rule, and a rule of that kind restored from a Team backup holds every run its other criteria cover, so it never narrows to the paid scope. Separation of duties is enforced on every build for any run that carries it, because a restored backup must keep its gates. Team is what lets you write the rule.

Reconcile's wall is the private proposal code and the route's refusal naming Team. Plan keeping stays public because a pull request plan, a Community feature, is kept by the same code and read by the comment apply, so there is no plan-keeping line that can be private without taking a free feature away. The database-mode worker, switchtender worker --db, is refused on Community, but that refusal is not a wall: a second switchtender serve on the same PostgreSQL database gives the same capacity, and that is free. The paid part is the relay, worker --server, which runs a worker inside a network the control node cannot reach.

Pro, at $500 a year flat per organization to 500 hosts, adds directory sign-in (OIDC, SAML,
LDAP, and JWT, with just-in-time provisioning, and group-to-role mapping on all but OIDC), five
approval policies instead of one, and deny rules and risk and reversibility floors.

Team, from $7,500 a year by fleet band, adds the full policy engine (unlimited policies, Rego
policies, distinct-approver separation of duties, and rules scoped to an actor or agent), the
period change register, dedicated workers and named queues, and the relay that runs them inside
networks the control node cannot reach, and one-click drift reconcile. PostgreSQL, including
active-active high availability on it, is Community. No license server, no
activation, no phone-home, no seat counting, and fleet bands are self-reported and never audited. A
lapsed license takes nothing: opening or creating a PostgreSQL database is never gated, in any state,
and every Community feature keeps working. Restoring a backup carries your approval policies back
whatever tier the install runs, because an install that came back without the gates it had is a
worse outcome than one holding a rule it is no longer licensed to create. Enterprise, from $50,000 a
year, adds the services that come from outside your install: custody of your evidence, evidence
work on your audit calendar, and the hosted witness, which Team can also buy as an add-on.

One-click drift reconcile, from detection to a held, approvable fix. Terraform and OpenTofu reconcile the exact plan the check kept. Ansible reruns the playbook on the drifted host, bound to the check that found it, a rerun you can also launch by hand on Community.

Seven commitments, held for every user from day one. What is free today stays free, and the
Community tier never shrinks. A lapsed license takes nothing: data, evidence, receipts, and every
Community feature keep working, and only paid features stop. No phone-home, no seat counting, no
audits, ever. Every receipt verifies without us, forever, with the open verifier. Each release converts
to Apache 2.0 two years after it ships, and the terms accelerate that to immediate if
KordLoom ceases business. Your price is
fixed for the term, with 60 days notice before any renewal change. And pricing is flat per
organization within a band, never per seat and never per run.

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

## Services

Beside the tiers, KordLoom runs the Migration Program, a fixed-scope engagement under its own
written agreement that moves an estate off AWX, Semaphore, or Rundeck and into production on
SwitchTender. Enterprise adds the services that come from outside your install: custody of your
evidence, evidence work on your audit calendar, and the hosted witness, which Team can also buy as
an add-on. None of these is required to use the software.

See [switchtender.com/pricing](https://switchtender.com/pricing) for what each tier and service
includes.

## Conversion to open source

The Business Source License is time-limited. Each released version converts to the Apache License
2.0 two years after that version's release, the Change Date recorded in `LICENSE`. After a version
converts, the reserved right no longer applies to it.

Versions before v1.72.0 shipped a `LICENSE` naming a fixed date in 2030 rather than the two-year
span. `LICENSE-GRANT.md` sets their Change Date to two years after each of those versions was
released, which is what the commitment always said. That grant is irrevocable and the set of
versions it covers is closed.

## Getting in touch

Email licensing@switchtender.com to start a conversation about a commercial license or support. You
can also open an issue at https://github.com/kordloom/switchtender, though email keeps
your inquiry private.
