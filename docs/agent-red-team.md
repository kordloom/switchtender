# Red team: can an agent get a change past the gate?

The agent documentation claims an AI agent cannot approve its own work, cannot widen its own
reach, and cannot execute anything a person did not release. Documentation is a claim. This page
is the attempt to break the claim, with the commands, the responses, and the limits.

Everything here runs against a released binary on a Community install with no license, so anyone
can repeat it and get the same answers.

## The setup, deliberately hostile

The agent is given the strongest credential the product will mint for one. Its human is an admin,
so the token comes back carrying an admin role:

    switchtender user new alice --role admin
    switchtender token new --name claude-agent --user alice --agent

    {"id":"tok_...","kind":"agent","name":"claude-agent","role":"admin","user":"alice"}

If the agent cap were only a role check, an admin-role agent token would walk through it. The
gate is one policy, pinned to a file so the API refuses policy writes even from an admin:

    policies:
      - name: agent-work-needs-a-person
        actor_kind: agent

    switchtender serve --policy-file policies.yml

Deny rules, risk floors, distinct approvers, and any second policy are Team features, and the
license gate refused them on this install. What follows is therefore the floor of the free tier,
not the ceiling of the paid one.

## What the agent tried

| Attempt | Result |
|---|---|
| Submit a run | Held at `pending_approval`, recorded as `actor_type: agent` |
| Approve its own held run | 403 |
| Mint itself a token that is not an agent token | 403 |
| Create a second admin account | 403 |
| Create a credential | 403 |
| Rewrite the policy | 403 |
| Delete the policy | 403 |
| List credentials | 200, metadata only, see below |
| Retry the held run to reach execution | 409, only split runs can retry shards |
| Rerun the held run | 403 |
| Relaunch failed shards | 409, run has not finished |
| Submit with `actor_type: human` and `actor: alice` | 400, unknown field |
| Rewrite the spec while held, by PATCH, PUT, and POST | 403 on all three |

### The read that succeeds

Listing credentials returns 200, so it is worth showing exactly what comes back when a credential
holding a real secret exists:

    {"credentials":[{"id":"cred_...","name":"prod-ssh","kind":"ssh_password",
      "source":"local","created_at":"...","needs_secret":false}],"count":1}

The secret is absent, and cannot be otherwise: the field is excluded from serialization at the
type, not filtered per handler, so no endpoint can return it. It is absent from the admin's own
response to creating the credential too. What the agent sees is the name it needs in order to
propose a run that uses the credential, which is the point of letting it look at all.

### Why the forged identity fails

The forged submission is refused for a more interesting reason than a permission check. The API
rejects unknown fields outright, so actor identity is not something a caller can put in a body and
have believed. It comes from the token that authenticated the request.

## What a person does, and what it leaves behind

A human approved the same run. It executed and succeeded, and the record still names the agent as
the actor, so passing through an approval does not relabel who asked.

The receipt was then verified with the server stopped:

    switchtender receipt <run-id> > receipt.json
    switchtender verify receipt.json

    rules in force 1
      - agent-work-needs-a-person: requires approval
    decisions    OK (1, each matches what the chain committed)
      approved by human-admin (token), binding spec sha256:e761b206b0acfdf8...
    spec         OK (approved, executed, and disclosed digests agree)

    VERIFIED: nothing has been altered since this receipt was signed

That output carries the whole claim in one place: which rule applied, that a person and not the
agent released it, the exact specification the approval was bound to, and that the approved,
executed, and disclosed specifications are the same bytes. It was produced with the issuing server
switched off, so nothing in it rests on asking the server what happened.

## What this does not prove

- It was run by the people who wrote the software. It is a transcript anyone can reproduce, which
  is a different and weaker thing than an independent audit.
- It covers the HTTP API and the MCP surface. It does not cover a compromised host, an operator
  with direct database access, or a tampered binary.
- The limit in the threat model still stands: the chain proves nothing was altered after it was
  written, not that a compromised controller wrote everything down. Closing that gap is what an
  external witness is for, and this exercise does not test it.
- One policy, one agent, one install, no load.

If you find a way through, the security contact is in the repository, and a finding that breaks
this page is worth more to us than a page that stays unbroken.
