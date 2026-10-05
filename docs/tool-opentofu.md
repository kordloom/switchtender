<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# OpenTofu runs

An OpenTofu run provisions infrastructure from a working directory of `.tf` files with the `tofu`
binary. It behaves exactly like a [Terraform run](tool-terraform.md). The run's command names the
directory, relative to the project checkout. Pick the tool that matches the binary your
configurations are written for; the two are configuration-compatible for most modules.

## What runs

`tofu init` runs first. If it fails the run stops there with init's result. Then `tofu apply
-auto-approve` applies the configuration. A dry run runs `tofu plan` instead and saves the plan
file, so it previews the change without touching infrastructure. All three run with
`-input=false -no-color`, so a run never blocks on a prompt.

### A gated apply carries out the approved plan

An apply runs in two steps when a rule covers it: a plan-content rule, one with `max_destroy` or a
Rego `plan_gate`, or any approval rule that would hold it. First it plans and saves the plan file
with `-out`. The gate measures what the plan destroys from that saved file, rendered with
`tofu show -json`, never from the text the tool printed. The apply it proposes carries the saved
plan, the rules decide on that proposal, and an apply they hold waits with the plan its approver is
shown. Once released it carries out exactly that file with `tofu apply <planfile>`, and the
approval binds the plan file's digest. A rule that holds an apply therefore holds the planned
proposal rather than the request, so the approval covers the plan that runs. An apply whose own
submission asks for approval, through `require_approval` or an AI proposal, is planned first the same
way: the request is not held, and the apply its plan proposes waits for a person with the saved plan,
whatever the rules say.

An apply an AI agent asks for is the exception. It is held before anything plans, since planning
runs provider code with the server's credentials. Once a person releases it, it plans, and the apply
its plan proposes is held again with the saved plan, so it takes two approvals. An apply an
exemption covers takes the path above, the way a person's does, as [agent runs are held by
default](policy.md#agent-runs-are-held-by-default) describes.

If anything changed the state after the plan was made, OpenTofu itself refuses the stale plan with
`Saved plan is stale`, and the apply fails without changing anything rather than planning again and
applying what nobody weighed. The run then says the proposal has to be made again: submit the apply
again, and it is planned afresh.

A [drift check](drift.md) works the same way. A dry run saves its plan file too, and a check that
finds drift keeps it, sealed, so the [reconcile](drift.md#reconcile-a-drifted-target) proposed from
the check carries out exactly that plan, the one shown with the check, with its digest bound into the
approval. A later check of the same working directory drops the plan an earlier check kept, whether
or not it finds drift. A reconcile is refused for a check that kept no plan, and one whose plan went
stale before its approval landed fails the same way an apply does, saying to run the check again and
propose the reconcile from it.

A plan file holds the values the configuration was planned with, sensitive ones included, so it is
handled as a secret. It is sealed with the server's key while the apply waits, written only into the
run's private directory, never written to the run log, and wiped when the apply ends. A drift check's
plan stays sealed at rest until a newer check of the same working directory replaces it. The values
a plan marks sensitive are added to the masker before the tool prints anything. On a relay worker,
the plan's worker, or the drift check's, hands the saved plan file to the control node, which seals
it, and the apply's worker receives it sealed to its pool's delivery key, like a credential, so a
pool with no delivery key cannot run a gated apply. A plan file crosses the relay only up to 19 MiB.
A larger one is refused with its size and the limit stated: the plan fails without proposing an
apply, and a drift check keeps no plan and says a reconcile cannot be proposed from it. Run it on a
queue the control node executes or split the configuration.

An apply that no rule covers still runs `apply -auto-approve`, which plans and applies in one step
with nothing saved in between.

## What a plan runs, and what the gate reads

A plan changes no infrastructure, but it is not free of execution. It runs the provider plugins the
configuration names, with the run's credentials, and it reads every data source with the same
credentials and environment: an `external` data source runs the program it names, an
`aws_lambda_invocation` data source invokes its function, and an `http` data source sends its
request. Providers are trusted code the team chose and installed, and the gate does not judge them.
Before it exempts a plan from a rule with `exclude_dry_run`, it reads the configuration and every
module it calls for `external` and `aws_lambda_invocation` data sources, for `http` data sources
whose method is anything but GET or HEAD or that carry a request body, and for anything it cannot
read. A method it cannot read as a plain value counts as a write. A plan that declares one, or that
the gate cannot read in full, is held as the real run it may be and named by address, such as
`module.network.data.external.lookup`, and a pull request's plan of it is refused rather than run.
An agent's plan waits for a person whatever the scan finds, unless an exemption covers it. Before it
reads registry and remote modules, the gate downloads them with `tofu get` in a private copy of the
configuration, with the run's own credentials and only where the plan runs, and reads them where the
get installed them. A `.terraform` the gate did not install, committed or left in a working
directory, is never read, and no setting makes it trusted: vendor a module and call it by a local
path to have it read from the repository. The get installs no provider and runs nothing the
configuration names. A module it could not download is left unread. The run then executes exactly
the modules the gate read: their digest is in the scan evidence and the approval, the gate's copy is
put in place, and `tofu init` runs with `-get=false`, so it resolves no version again. A run that
cannot get the same modules is refused. Files ending in `.tofu` are read as well as `.tf`, and a
module source built from a variable, which OpenTofu evaluates at init, is left unread, since its
value is only known when the run plans. The scan targets those three data sources and unreadable
configuration only, so it does not prove a plan harmless, and a rule that exempts plans trusts the
configurations it lets through. See [dry runs and
`exclude_dry_run`](concepts.md#dry-runs-and-exclude-dry-run).

## How values reach the configuration

- Extra vars, including survey answers and template vars, arrive as `TF_VAR_` environment entries,
  which OpenTofu reads the same way Terraform does. Scalars pass through as strings, lists and maps
  pass as JSON.
- Credentials arrive in the environment, so an `env` credential of cloud keys or a `token`
  credential as `SWITCHTENDER_TOKEN` authenticates the provider. A command-source credential resolves
  fresh each run.
- The command directory cannot escape the project with `..`, so a run stays inside its checkout.
- Every OpenTofu process the runner starts, the module download, the init, and the plan or apply,
  runs with `CHECKPOINT_DISABLE=1`, so it makes no version check against HashiCorp's checkpoint
  service. A run whose own environment sets `CHECKPOINT_DISABLE` keeps the value it sets.

## Requirements

The `tofu` binary must be on the PATH of the process executing runs, the server or a worker. Nothing
else is configured; selecting the OpenTofu tool routes the run to it.

## Example

A directory `infra/network` holding:

    variable "region" { type = string }
    output "vpc" { value = "vpc-${var.region}" }

Launch an OpenTofu run with the command set to `infra/network` and a survey field `region`. A dry
run shows the plan. A real run applies it.

See also [Terraform runs](tool-terraform.md), [Bash runs](tool-bash.md), and the
[features overview](features.md).
