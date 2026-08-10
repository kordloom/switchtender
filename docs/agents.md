<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Run an AI agent through the gate

AI agents are operators now. They patch hosts, roll configs, and apply Terraform, through whatever
credentials they are handed. SwitchTender gives an agent one door: an API that records every change
to a fail-closed hash chain before the change executes, holds whatever policy says a human must
approve, and exports proof a third party can verify without trusting you.

## Why

Two facts make agents different from the operators you already trust.

Agents act faster than review. A person makes a change, then explains it in the ticket. An agent
makes fifty changes while a reviewer reads the first diff. After-the-fact review does not scale to
that pace, so the control has to sit in the path of the change, not behind it.

Auditors discount logs that cannot prove integrity. "The agent's actions are logged" persuades no
one when the log is a file anyone with access could edit. What survives scrutiny is a record that
proves it was not altered, by math a third party can check, not by trust in the server that
produced it.

SwitchTender does both. Every API mutation is recorded to the hash chain before it executes, and a
mutation that cannot be recorded is refused with a 503 rather than performed off the record. GET
and HEAD are reads and are not chained. The chain hash commits to the actor, the method, the path,
the time, and the sequence number, so the record of what the agent did is fixed the moment it acts.

## The one-credential architecture

Give the agent exactly one credential: an operator-bound SwitchTender token. It never holds prod
credentials. SSH keys, cloud credentials, and vault passwords stay sealed in SwitchTender, decrypt
only at execution, and are never returned by the API, so the agent cannot read a secret out and go
around the gate. The gate is its only door.

The operator role bounds what the door allows. An operator can submit, cancel, and retry runs and
launch templates. It cannot approve runs, manage configuration, or read the audit page. Approving a
held run is admin-only, so an operator-bound agent can never approve its own work.

## Onboarding an agent

1. Create an account for the agent with the operator role:

       switchtender user new agent-bot --role operator

2. Mint a token bound to that account:

       switchtender token new --user agent-bot --name agent-bot --ttl 720h

   The token carries the account's operator role, and its label is the actor on everything the
   agent does. A token minted without `--user` is unscoped and acts as admin; never hand one to an
   agent. The TTL forces rotation, here monthly.

   Creating the first token turns authentication on for the whole install, so make sure an admin
   account exists before the agent holds the only credential.

3. Decide what a human must approve. An approval policy with no criteria matches every run, so one
   empty policy is a gate-everything switch:

       policies:
         - name: hold-everything

   Or scope policies to the dangerous cases and let routine runs flow:

       policies:
         - name: prod-terraform-destroy
           tool: terraform
           command_contains: destroy

4. Pin the policies by starting the server with `serve --policy-file policies.yml`. The file is the
   source of truth and the API refuses policy writes, so even an admin API caller cannot rewrite
   them. An agent cannot loosen its own gate, and neither can a leaked admin token.

5. Hand the agent its token and the API base URL, and nothing else.

## Connecting the agent over MCP

*Available in the next release.*

There are two ways an agent reaches the gate. The direct one is the HTTP API above: the agent holds
the token and the base URL and makes ordinary authenticated calls. The second is the Model Context
Protocol, which many agent runtimes speak natively. Put the operator-bound token in
`SWITCHTENDER_MCP_TOKEN` and run:

    switchtender mcp --server https://switchtender.internal

The agent gets a small, deliberate set of tools: list job templates, propose a run, read a run and
its log, pull a run's evidence dossier, and list recent runs. Every tool call is an ordinary
authenticated API request under that same token, so it passes the same authorization, the same
approval policy, and the same fail-closed audit append as a call from a person. A proposed run lands
in the chain under the agent's account before it executes, and a policy-covered run waits for a human
to release it.

The tool set is narrow on purpose. There is no approve tool, so an agent cannot release its own work
however it is prompted, and no credential, account, token, grant, or policy tool, so it cannot widen
its own reach. The command refuses to start on an admin token. Ad-hoc runs, where the agent composes
a command instead of launching a template a person defined, stay off unless you pass `--allow-adhoc`,
and the approval policy still covers them when they are on.

## What the record shows

The actor on every chain entry the agent produces is its token's label, `agent-bot` above. The
chain hash commits to that actor along with the method, path, time, and sequence, so attribution is
part of what the chain proves, not a field someone could rewrite later.

Every mutation response carries an `Audit-Receipt: seq:hash` header. The agent, or the system
driving it, can retain receipts and later check each one against the chain, so the party an entry
belongs to can detect an omission.

Runs record their source, one of api, template, schedule, rerun, reconcile, propose, or trigger,
along with the actor. `actor:agent-bot` in run search pulls everything the agent ran, and
`source:api` separates direct API submissions from scheduled or triggered work.

## Handing evidence to someone else

`switchtender audit bundle` exports a signed LoomSeal bundle. A third party verifies it offline
with the open loomseal verifier, with no trust in the server that produced it. The install
publishes its signing key at `/.well-known/loomseal.json`, so a verifier pins the key from a
channel independent of the bundle it was handed.

That is the shape an auditor accepts: here is what the agent did, here is the math showing the
record was not altered, check it yourself.

## The boundary

The chain proves the record was not altered. It does not prove that changes made outside
SwitchTender went through it: an agent holding your cloud credentials can change infrastructure
directly, and no record here would show it. For agents, credential scoping closes that gap. An
agent whose only credential is its operator-bound token has no way to act except through the gate,
so for that agent the record is not just intact, it is the whole story.

## FAQ

**Is this the built-in advisory AI?** No. The [advisory AI](ai.md) is the other direction: a model
that proposes text and never executes, with anything runnable born held for a human. This page is
about an external agent, yours, that operates SwitchTender through the API the way a person would.
The advisory AI is a feature you switch on; an agent is a client you let in.

**Can the agent approve its own runs?** No. Approving a held run is admin-only, and the agent's
token is operator-bound. A held run the agent submits waits for a human admin.
