# Scenario suite

A scenario is a YAML file describing a whole SwitchTender install: the environment it runs in, the
dependencies it stands on and whether each is real or standing in, the people and objects it starts
with, the requests to drive through it, and what must be true afterward.

Run it with `go test ./test/scenario/`. It is part of `go test ./...`, so the release gate already
runs it.

## Why this exists beside the unit tests

The defects that survive a package's own tests are rarely wrong handlers. They are handlers that are
each correct and together inconsistent:

- a list that shows what the by-id read refuses, or hides what it serves
- a view derived from runs that names a run both of them refused
- a receipt that verifies under one of an install's names and not the other

None of those is visible from inside one handler. So every scenario, whatever it was written for, is
checked against the whole invariant battery, as every actor it declares. A scenario about tenant
isolation catches a regression in the fleet view for free, because the battery does not care what
the scenario was about.

## Anatomy

```yaml
name: one project delegated on an install that never turned strict grants on
why: |
  Why this scenario exists. Required: a case nobody can explain is a case nobody can judge the
  failure of.

environment:
  target: inprocess     # inprocess | kind
  grants: both          # open | strict | both. Both is the default and runs the scenario twice.
  store: memory         # memory | sqlite
  tier: community       # community | team
  producer: false       # give the install a signing identity

dependencies:
  submitter:
    mode: broken        # real | fake | broken
    behavior: {fail_after: 1, message: the executor refused this run}

fixtures:
  orgs:        [{id: org_a, members: {user_a: member}}]
  users:       [{id: user_a, name: a, role: operator}]
  projects:    [{id: proj_a, name: a, org: org_a}]
  grants:      [{subject: user_a, object: proj_a, access: use}]
  runs:        [{id: run_a, project: proj_a, host: web1, extra_vars: {k: SECRET}}]
  credentials: [{id: cred_a, kind: ssh_key, secret: KEY-MATERIAL}]
  templates:   [{id: tpl_a, project: proj_a, extra_vars: {api_token: PASTED}}]
  schedules:   [{id: sched_a, cron: "0 2 * * *", template: tpl_a}]
  secrets:
    - {name: survey answer, value: SECRET, on_run: run_a}
    - {name: token-shaped variable, value: PASTED, to_roles: [admin]}

cases:
  - name: what this case establishes
    why: required, same reason as above
    steps:
      - note: what this step establishes
        as: user_a
        get: /v1/runs/run_a
        expect:
          status: 200
          includes: []
          excludes: []
          state:
            - runs: {count: 1, project: proj_a}
```

A step may send `body` (a YAML mapping encoded as JSON) or `raw_body` (a literal string, for the
malformed input no mapping can express). Not both.

## The invariant battery

Every scenario is held to all of these unless it names one in `skip_invariants` with a reason.

| Invariant | What it protects |
|-----------|------------------|
| `list_fetch_parity` | A listing and the by-id read behind its rows must give the same answer, for every actor. |
| `derived_views_agree` | No view built out of runs may name a run its own by-id read refuses. |
| `changes_agree` | A change must not appear to a caller refused every run in it. |
| `no_secret_leak` | A declared value must not reach an actor not entitled to it. |
| `no_internal_errors` | No well-formed read may answer 5xx, including reads for ids nobody minted. |
| `refusals_explain_themselves` | Every 4xx must carry a reason the reader can act on. |
| `writes_are_recorded` | Every write a case makes, whatever it answered, must append an audit entry. |
| `unauthenticated_reads_nothing` | A caller with no credentials is not a caller with a role. |

`writes_are_recorded` is checked after each write rather than over the finished install, since only
the write itself knows what the chain held a moment before. A refused write counts: a trail holding
only what succeeded cannot answer what was attempted, which is most of what an investigation asks.

One more runs across the two builds rather than within one: **`strict_only_narrows`**. What
`--strict-grants` decides is the default for an object nobody has granted, and nothing else, so
every difference it makes has to be a removal. If strict mode ever shows an actor something the open
install did not, the two modes have drifted into two authorization systems sharing a flag. The
scenario has already been built twice, so the comparison costs only the reads.

### Who is entitled to a value

`no_secret_leak` needs to know who may legitimately see each value, or it either misses leaks or
fails on content. A secret fixture declares that three ways:

| Declaration | Entitled to |
|-------------|-------------|
| `on_run: run_a` | whoever the by-id fetch of `run_a` serves. It is that run's own content to them. |
| `to_roles: [admin]` | those global roles only. This is the shape of a value the install deliberately shows one audience and masks for the rest. |
| neither | nobody, through any endpoint, at any role. A credential's material is this. |

Every declared credential joins the hunt automatically, in both its plaintext and its sealed form:
shipping the material at rest out of the install is still shipping the material out.

A value that is masked for everyone is as wrong as one shown to everyone, so declare both halves.
Asserting only that a value is hidden passes on an install that hid it from the people who maintain
it.

`parityRoutes` is held against the routes the server actually mounts, so a by-id read added later
fails the suite until it is either covered or excluded with a stated reason. A gap is a task, never
a blind spot.

The same rule applies to the run-derived views. `derivedViews` holds the paths whose rows carry a
run id, and every one must name a fixture run somewhere in the suite or the suite fails: a path no
scenario can put a row into answers an empty body, agrees with every by-id answer, and reports
coverage that does not exist. The paths that carry no run id are named in `derivedAggregates` with
the reason, so a surface is never simply absent from both lists.

Presence is decided by decoding the body and looking for the id under a member that references a
row, not by scanning bytes. A run's extra variables are free text somebody typed, and one naming
another run's id would otherwise read as that row being present, which masks a real list-versus-fetch
disagreement in one direction and invents one in the other.

## Dependencies and stand-ins

`mode: real` uses the actual dependency, which only the `kind` target can provide; declaring it on
the in-process target is refused rather than silently downgraded. `mode: fake` and `mode: broken`
select a registered Go stand-in by name (`go:vault`), or an executable (`script:./path`).

Every registered stand-in must have a contract test, enforced by the suite. A stand-in that has
drifted from what it stands in for turns the suite into something testing itself. The shared half of
that contract, the failure-injection knobs, is held to one meaning across every stand-in: `fail_after`
counts the calls that succeed, `unavailable` is down from the first call.

## Targets

`inprocess` builds the stores and handlers in the test process. It answers every question about the
server layer at the speed that lets the whole matrix run on every push.

`kind` deploys the built image into a cluster through the shipped chart, which is the only place the
packaging, the migrations, and the upgrade path are real. It is **not implemented yet**, and a
scenario declaring it **fails** rather than being skipped or quietly run in process. A scenario that
silently did not run is worse than one that is missing, because it reads as coverage.

## Adding a scenario

Write the file, run the suite, and then break the thing it is about and confirm the scenario goes
red. A test that has never failed has not been shown to test anything.
