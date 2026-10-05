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
only at execution, and are never returned by the API, so the agent cannot read one out of the
credential store or copy one to somewhere you cannot see. Every use of one is recorded against the
run that used it.

What that does not mean is that a credential attached to a run is hidden from the run. A run is code
you asked to execute with that credential in scope, so it can print the value in a form the log
masker does not recognize, exactly as it can in Jenkins, GitHub Actions, or any other runner. The
masker removes a secret that appears verbatim, which stops the ordinary accident of echoing a
variable. It is not a boundary against a script written to defeat it. Scope the operator token to
the credentials it genuinely needs, and treat the ability to attach a credential to a run as
equivalent to holding it.

The operator role bounds what the door allows. An operator can submit, cancel, and retry runs and
launch templates. It cannot approve runs, manage configuration, or read the audit page. Approving a
held run is admin-only, so an operator-bound agent can never approve its own work.

Agents can propose changes. Only authorized humans can approve them. Separation of duties can
require an independent human when policy demands it. An approval comes from a person's session, a
person's own token, or a pull request comment from the forge account a person linked, never from an
agent's.

That holds on two independent locks. The door caps an agent's token below the role every approval
route needs. Behind it, the dispatcher refuses any approval whose decider is an agent, for a held run
and for a workflow approval step alike, so a token that somehow reached the route still releases
nothing and records nothing. Each lock is tested on its own, and a test that enumerates every path
that can release work, the API, MCP, workflow steps, callbacks, relay proposals, and pull request
review, fails the build when a new path appears without a test proving an agent cannot use it.

## Onboarding an agent

1. Pick the person the agent answers to. Its token is bound to that person's account, so every
   entry the agent writes names who it acted for. Create the account if they have none:

       switchtender user new dev-lead --role operator

   It asks for their password, or reads one from `SWITCHTENDER_PASSWORD` when there is no terminal,
   as in a provisioning script. The agent never signs in: the token in the next step is its only
   credential. An agent token is capped at the operator role whatever the account holds, so binding
   it to an admin's account gives the agent nothing more.

2. Mint a token bound to that account, marked as an agent and labeled as the agent:

       switchtender token new --user dev-lead --name agent-bot --agent --ttl 720h

   Or from the interface, on the Users page under API tokens, or over the API when you have no shell
   on the server:

       curl -X POST https://switchtender.example.com/v1/tokens \
         -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
         -d '{"name":"agent-bot","username":"dev-lead","kind":"agent","ttl_hours":720}'

   The API refuses a token bound to no account, so a credential minted over the network always names
   the account it acts as and can never exceed that account's role.

   The `--agent` flag is what makes the record precise. It marks the token as held by an agent, so
   every chain entry it produces carries `actor_type: agent` alongside its label and the human it
   acts for, set when the token is minted rather than guessed from how a request looks. It also caps
   the token at the operator role no matter what account it is bound to, so an agent can launch and
   propose work but can never manage identity, access, or secrets, and can never approve its own held
   run. That cap is enforced at the door, so it holds however the agent reaches the API, not only
   through the client below. `--agent` requires `--user`, because an agent acting on behalf of nobody
   is exactly the accountability gap the identity closes. The TTL forces rotation, here monthly.

   The token records who minted it. An organization admin can provision an agent for somebody
   else's account, so the account the agent acts under and the person who issued it can differ, and
   every run the agent asks for records both.

   A public bind on an empty database already minted an initial admin token at first start, so
   authentication is on before the agent holds any credential. Make sure a human admin account
   exists too, so the agent's held runs have somebody who can approve them.

3. Decide what a human must approve. You do not need a policy for the agent's own runs: every run
   an agent token asks for waits for a person's approval unless a written policy exempts it. The
   hold applies to tokens minted with `switchtender token new --user <account> --agent`, and a token
   minted without `--agent` gets no agent hold. A dry run waits too, since check mode and a plan
   still run code with this server's credentials, and a Terraform or OpenTofu apply takes two
   approvals: one before it plans, and one for the apply its plan proposes with the saved plan.
   To let routine work through, write an exemption, a rule with `effect: exempt`, which Community
   covers. An exemption that names the agent's `actor` label must also name the `account` its
   token is bound to, since a label repeats across accounts. The receipt proves who approved each
   run or which rule exempted it.
   [Agent runs are held by default](policy.md#agent-runs-are-held-by-default) covers the exemption
   and what it risks.

   Policies gate people's runs too. An approval policy with no criteria matches every run, so one
   empty policy is a gate-everything switch:

       policies:
         - name: hold-everything

   Or scope policies to the dangerous cases and let a person's routine runs flow:

       policies:
         - name: prod-terraform-destroy
           tool: terraform
           command_contains: destroy

   Policies also see who is asking, how risky the operation grades, and how hard it is to undo, and
   they can refuse outright rather than hold. The default hold already makes an agent's runs wait
   for a person, so the rules worth writing for an agent are the ones it does not cover: what the
   agent may never do at all, and which of its changes need a second person:

       policies:
         - name: agents-never-drop-databases
           actor_kind: agent
           command_contains: "drop database"
           effect: deny
         - name: agent-high-risk-needs-another-person
           actor_kind: agent
           min_risk: high
           require_distinct_approver: true

   `actor_kind: agent` scopes a rule to runs an agent submitted, identified by its minted token,
   never guessed from traffic. `actor: prod-remediator` pins a rule to one named principal, and
   `account: dev-lead` to the account a token is bound to, which tells apart two agents that share
   a label. `min_risk` matches on the run's assessed risk grade. `effect: deny` refuses the
   submission outright, so no person can release it either, and the refused request is still on the
   chain, because the gate records every mutation before anything acts on it.
   `require_distinct_approver: true` refuses a decision made by whoever asked for the change, which
   for an agent's run is the account the agent is bound to, so the person the agent acts for cannot
   release its high-risk work alone. The requirement is copied onto each run the rule holds, so
   editing the rule later cannot weaken a pending decision. A rule that holds still applies to a run
   an exemption covers, since an exemption lifts only the default hold.

   Every criterion in that file is the full policy engine, which a Team license covers: actor
   scoping, risk floors, deny rules, and distinct-approver separation of duties. A server started
   with a policy file it is not licensed for refuses at startup and says so, rather than running with
   the rules quietly dropped. The gate itself is free on every tier: every run an agent submits
   waits for a person with no rule written, and that is the whole containment story on this page. A
   Community install also holds one plain rule, either a require-approval policy for everyone's runs
   or one exemption. What Team buys is scoping holds by actor, account, risk, and reversibility, and
   a rule that can refuse outright rather than wait.

4. Pin the policies by starting the server with `serve --policy-file policies.yml`. The file is the
   source of truth and the API refuses policy writes, so even an admin API caller cannot rewrite
   them. An agent cannot loosen its own gate, and neither can a leaked admin token.

5. Hand the agent its token and the API base URL, and nothing else.

## Connecting the agent over MCP

There are two ways an agent reaches the gate. The direct one is the HTTP API above: the agent holds
the token and the base URL and makes ordinary authenticated calls. The second is the Model Context
Protocol, which many agent runtimes speak natively. Put the operator-bound token in
`SWITCHTENDER_MCP_TOKEN` and run:

    switchtender mcp --server https://switchtender.internal

The agent gets a small, deliberate set of tools: list job templates, propose a run, read a run and
its log, pull a run's evidence dossier, list recent runs, and list the workflow approval steps
waiting for a person. Every tool call is an ordinary
authenticated API request under that same token, so it passes the same authorization, the same
approval policy, and the same fail-closed audit append as a call from a person. A proposed run lands
in the chain under the agent's account before it executes, and it waits for a human to release it
unless a written policy exempts it.

The tool set is narrow on purpose. There is no approve tool, so an agent cannot release its own work
however it is prompted, and no credential, account, token, grant, or policy tool, so it cannot widen
its own reach. The command refuses to start on an admin token. Ad-hoc runs, where the agent composes
a command instead of launching a template a person defined, stay off unless you pass `--allow-adhoc`,
and the default hold and the approval policy still cover them when they are on.

The narrowness holds inside a template launch too, which is where it would otherwise leak, and it
holds on the server, so it applies the same whether the agent connects over MCP or calls the API
directly:

- An agent cannot supply extra vars. Extra vars sit at Ansible's highest precedence, above everything
  the template and the inventory set, so an agent allowed to send them would rewrite what a vetted
  template does while the audit trail recorded the template's name. Survey answers are the supported
  channel: the operator declares which fields a caller may fill, and the template says so. That
  includes a secret field, so an agent can supply a password a template asks for. The answer is
  sealed at once, and no caller, the agent included, can read it back.
- A `limit` can only narrow. A template that pins its own target refuses a different one, and a
  pattern meaning every host is refused outright, because the risk grade approval policies key on is
  computed partly from how wide a run reaches.
- An agent cannot aim a template at another inventory or choose its credentials. It launches the
  template against the target and with the credentials the operator gave it.
- An argument the tool does not define is refused rather than dropped. A model writing `check_mode`
  instead of `dry_run` is told so, rather than having the flag silently ignored and a real change
  reported back as a preview.
- An agent can read the evidence and the signed receipt for runs it proposed. Another actor's
  evidence needs an admin.

## Waiting for a decision

A held run does not block the agent. The submission answers at once with the run, and the run's
`status` says where it stands: `pending_approval` until a person decides, then it runs and ends
`succeeded` or `failed`, or it ends `rejected`, with `error` reading "rejected by an approver". The
reason the person gave, when they gave one, is in `GET /v1/runs/{id}/decisions`, which the agent may
read for a run it proposed, and in the run's evidence that the `get_run_evidence` tool returns. The
agent
learns the decision by reading the run again, with `GET /v1/runs/{id}` or the `get_run` tool, and
`GET /v1/runs?status=pending_approval` lists everything still waiting. An agent that would rather
not poll can stream the run instead: `POST /v1/runs/{id}/stream-ticket` returns a short-lived ticket,
and `GET /v1/runs/{id}/stream?ticket=...` delivers the run's events as it is released, executes, and
ends. Reading is all it can do about the decision: an agent that tries to approve its own run is
refused with 403.

A workflow the agent launched can also stop partway, at an approval step. The workflow's steps up
to it run, and then it waits. `GET /v1/approvals`, or the `list_pending_approvals` tool, shows the
step waiting, what already ran, and what an approval or a denial runs next, so the agent can tell a
workflow waiting for a person from one that is stuck. It cannot approve or deny the step either: the
decision route is an admin route, an agent's token never reaches it, and the dispatcher refuses an
agent's approval a second time, the same lock it holds for a held run.

The person who decides is told when the run is held: the chat channels and webhooks the server
sends finished runs to also carry each hold, naming the rule that held it and who asked, and so
does email when `--notify-on` is `finish`, since an install set to email failures alone is not
sent approval requests it never asked for.
The same channels carry how it ended, whether it ran, was rejected, or was canceled while it waited.
They decide on the run's page in the interface, or with `POST /v1/runs/{id}/approve` or
`POST /v1/runs/{id}/reject` under an admin token, so a chat bot or a script can carry the decision
to wherever the approver already is.

## What the record shows

The actor on every chain entry the agent produces is its token's label, `agent-bot` above, and, for
a token minted with `--agent`, the entry also carries `actor_type: agent` and `on_behalf_of` naming
the human account behind it, `dev-lead` above.

Every run an agent asks for also records its identity evidence, committed with the run's outcome:
initiated by, the agent, bound to, the account it acts under, and provisioned by, whoever minted its
token. Each approval of it records approved by, the decider, and the separation-of-duties
evaluation: whether a rule required an independent approver, the account that counts as the
requester, which for an agent's run is the account it is bound to, whether the decider is
independent of it, and the result. The account an agent is bound to may approve the agent's runs and
workflows unless a separation-of-duties rule requires an independent approver, and then that account
is refused as the requester. The dossier shows all of it in one section, and `switchtender verify`
prints it from the receipt. The chain hash commits to all three along with the
method, path, time, and sequence, so who acted, what kind of actor it was, and whose authority it
used are part of what the chain proves, not fields someone could rewrite later. That is the
difference between a log, which is the operator's word for what happened, and a signed chain, which
a third party recomputes and checks without trusting the operator or the agent.

Every mutation response other than signing in or out carries an `Audit-Receipt: seq:hash` header.
The agent, or the system driving it, can retain receipts and later check each one against the chain,
so the party an entry belongs to can detect an omission.

Runs record their source, one of api, template, schedule, trigger, callback, rerun, relaunch,
reconcile, propose, review, or review_apply, along with the actor. `actor:agent-bot` in run search
pulls everything the agent ran, and `source:api` separates direct API submissions from scheduled or
triggered work.

An approval is a chain entry of its own. When a person releases a held run, the chain gains a
DECISION entry naming the approver and committing a digest of the exact spec released, and the
executor refuses a spec that changed after the decision. So the record does not say "someone
approved run 123". It says this person approved exactly this change, and exactly this change ran.
The entry names the approver exactly as the request that carried the decision is recorded: the
token's label, how it authenticated, and the account it is bound to.

## The whole path, end to end

This is the golden path a governed agent change takes, with the commands to watch it happen.

1. The agent proposes a change through MCP or the API. The request lands on the chain before
   anything acts, attributed `actor_type: agent`, on behalf of its human.
2. Policy sees an agent asking. A deny rule refuses it outright, with the rule named. Unless a
   written policy exempts it, the run is held: it is born `pending_approval` and no executor can
   claim it.
3. A person reviews the held run, its assessed risk, and its parameters, and approves. The DECISION
   entry commits who approved, the account they approved under, and the digest of exactly what.
4. The run executes, on whichever worker claims it, after re-checking the approved digest. The
   outcome lands on the chain: status, exit code, per-host results, the log digest, and the same
   spec digest.
5. Seal it and verify it anywhere:

       switchtender receipt run_abc123 --out run.receipt
       switchtender verify run.receipt --pubkey sha256:<the fingerprint the install published>

   Verify reads one file, touches no database and no network, and prints who asked, who approved
   exactly what, and what happened, with every claim recomputed from the chain. `--sparse` produces
   a receipt that proves the same chain facts while disclosing nothing about the entries around the
   run, for handing to an outside auditor on a shared install.

`switchtender audit bundle` exports the whole chain the same way, and a third party can verify
either artifact with the open loomseal verifier, with no trust in the server that produced it. The
install publishes its signing key at `/.well-known/loomseal.json`, so a verifier pins the key from
a channel independent of the bundle it was handed.

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
The advisory AI is a feature you switch on. An agent is a client you let in.

**Can the agent approve its own runs?** No. Approving a held run is admin-only, the agent's token is
operator-bound, and the dispatcher refuses an agent's approval even if one reached the route. A held
run the agent submits waits for a human admin. The account the agent is bound to may be that admin,
unless a separation-of-duties rule requires an independent approver.

That answer is a claim, so it is also tested. [Red team: can an agent get a change past the
gate?](agent-red-team.md) is the transcript of an agent holding an admin-role token trying
thirteen ways through, including approving its own work, minting itself a wider token, and
rewriting the specification while it waited.
