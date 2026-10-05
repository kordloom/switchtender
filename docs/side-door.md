# The side door

SwitchTender governs every change that goes through it. The side door is every way a change reaches
production without going through it: a person's own SSH key, a cloud console login, a script run
from a laptop. No audit system records what it never saw, so the first question a security team asks
is how that door closes, and what happens when something still comes through it.

The answer has three parts. Agents never get a side door. People stop needing one. What still comes
through turns up.

## Agents never get one

An agent holds one credential, its SwitchTender token, minted with `--agent`. The token is capped at
operator whatever account it is bound to, so the agent can submit runs and can never approve one,
manage identity or access, or manage secrets. Every run it submits waits for a person's approval
unless a rule with `effect: exempt` covers it, as [agent runs are held by
default](policy.md#agent-runs-are-held-by-default) describes. The SSH keys, cloud logins, and vault
passwords its runs use are sealed on the server and decrypted only at execution, so the agent never
sees them.

An agent that holds only its SwitchTender token has no way around the gate. For that actor, the
recorded history and the actual history are the same thing. The [agent guide](agents.md) walks
through the setup, and [the red team transcript](agent-red-team.md) is the attempt to break it.

## People stop needing one

A person keeps a side door open by holding a standing credential to production. SwitchTender removes
the reason to hold one.

- **It holds the keys.** SSH keys, machine logins, and cloud credentials live in SwitchTender, sealed
  at rest and decrypted only for the run that uses them. Operators launch runs through the gate
  without ever handling the key.
- **Or it mints them per run.** A Vault dynamic source mints a fresh credential for each run and
  revokes it when the run ends, and an AWS STS source hands each run short-lived role credentials
  that expire on their own. A leaked value is useless minutes later, and there is no standing key to
  leave on a laptop.
- **Or the vault you already run keeps them.** HashiCorp Vault, CyberArk, AWS Secrets Manager, Azure
  Key Vault, Google Secret Manager, and 1Password are all read at launch, so the credential stays in
  the system you already trust. [Secrets](secrets.md) lists every source.

Once the keys live there, taking standing SSH and console write access away from people is a decision
your organization can make without stopping work, because the gate is how the work gets done. Keep
one emergency credential outside SwitchTender, sealed and logged by whatever process your security
team already runs for break-glass access.

## What still comes through turns up

A change made around the gate still leaves a mark. Schedule a dry run, Ansible's check mode or a
Terraform or OpenTofu plan, and every check compares each host and working directory with what your
automation says it should be. A config file edited by hand over SSH, or a security group changed in a
cloud console, shows up as [drift](drift.md) on the next check. One click proposes the fix, held for
approval like any other change, so the repair goes back through the gate.

## What this leaves

- Drift says what diverged, not who changed it or exactly when. The hosts' own logs, or CloudTrail,
  answer who.
- Drift only sees state your automation declares. A cron job no playbook mentions is invisible to it.
- A change turns up at the next scheduled check, not the moment it happens. The interval is yours to
  set.
- Taking standing access away from people is your organization's decision. SwitchTender makes it
  workable, and the decision stays yours.

The [threat model](threat-model.md) covers the rest of what the gate does and does not defend against.
