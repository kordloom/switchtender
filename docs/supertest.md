# The supertest

Every push to main deploys SwitchTender into a Kubernetes cluster that has never seen it and
proves the whole product against three real machines. Not a mock of a deployment: the Helm chart
this repository ships, installed twice, driven over its own HTTP API, and checked in ways that
never take the product's word for anything.

The harness is [`test/supertest`](../test/supertest), one standalone Go package. Run it yourself
from the repository root with Docker, kind, kubectl, and helm installed:

    go run ./test/supertest -skip-team          # the free tier alone
    go run ./test/supertest -license lic.json   # both tiers, the whole arc

The [supertest workflow](../.github/workflows/supertest.yml) runs it on every push. The report
lands in the job summary as one table of claims and evidence, and the screenshots of the deployed
UI land in the run's artifacts.

## What a run proves

**The cluster is fresh and the images are built from the working tree.** The product image comes
from the repository's own Dockerfile at the commit under test, not from the last release. The
fleet is three SSH servers with stable DNS names, keys only, built from
[one small Dockerfile](../test/supertest/manifests/fleet.Dockerfile) that is honestly a server:
real sshd, real python, a real non-root account.

**Community installs the way the chart installs it and does real work.** No license, no external
database: SQLite on a PersistentVolumeClaim, updating by Recreate because SQLite holds one writer.
The harness adopts the initial admin token the server prints on first boot, exactly as an operator
would, creates accounts, stores an SSH credential and an inventory, and runs a real Ansible
playbook across all three machines. Then it reads the files that playbook wrote back through
`kubectl exec`, because proof that comes from the product's own reporting is not proof.

**Team initializes a PostgreSQL schema it has never seen, under a license, with a dedicated
worker and a shared signing identity.** Then it walks the arc the product exists for:

1. A routine playbook runs ungated, because creating files grades reversible.
2. A destructive playbook, submitted by an AI agent's token, is graded **irreversible from its
   own text**. Nothing in the file announces itself as dangerous; the scanner reads it.
3. A policy holding on that grade holds the run. The data still exists, checked from outside.
4. The agent tries to approve its own request and is refused.
5. A person approves. The run executes, and the deletion is real: the directory is gone on all
   three machines, checked again through `kubectl exec`.
6. The run's receipt is downloaded and verified **offline by a local binary that never spoke to
   the cluster**. Then one recorded name is rewritten and verification is required to refuse it.
7. The estate answers with every machine the runs touched, and the two runs read back as one
   change whose outcome was derived rather than typed.
8. A queue-pinned run is claimed by the dedicated worker, not the server.

**The authorization model holds against the credentials that would abuse it.** An agent token is
refused account creation, token minting, and credential writes. A viewer reads history and is
refused launching and approving. Every refusal is an HTTP request a real overreaching credential
would make, answered by the deployed install.

## Why the checks are shaped this way

Three rules, applied everywhere:

- **Effects are verified outside the product.** Files a playbook wrote are read over
  `kubectl exec`. Receipts are verified by a binary outside the cluster. A test that asks the
  product whether the product worked is a press release.
- **Negative checks must be able to fail.** The tamper check asserts the edit changed bytes
  before it asserts the verifier refused them, because a tamper that silently misses reports the
  verifier sound while proving nothing.
- **Failures explain themselves.** A run that fails brings its own event tail into the report; a
  failed install brings pod state and log tails. A test that says "failed" and nothing else makes
  somebody else do the investigating.

## What it has caught

The supertest earned its place before it was a day old: a chart whose default configuration could
not boot on the free tier, a paid-tier install that looked healthy while unable to sign a single
receipt, and an official image that advertised Bash as a first-class engine and did not contain
it. Each is the kind of defect every unit test misses, because each lived in the seams between
pieces that were individually correct.
