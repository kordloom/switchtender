# Contributing to SwitchTender

Contributions are welcome. Two things are required before a pull request can merge: a signed
Contributor License Agreement, and a Developer Certificate of Origin sign-off on each commit.

## Contributor License Agreement

SwitchTender is source-available under the Business Source License 1.1 and is offered under commercial
and hosted licenses as well. To keep both possible, every contributor signs the
[Contributor License Agreement](CLA.md) once. It grants the Maintainer the right to include your
contribution in every edition of the Project, including commercially licensed ones. You keep the
copyright to your work.

Sign once on your first pull request. A contribution cannot be merged without it.

## Developer Certificate of Origin

Sign off every commit to certify you wrote the change, or have the right to submit it under the
Project's license. Add a trailer to your commit message with your legal name and email:

    Signed-off-by: Your Name <you@example.com>

`git commit -s` adds this line for you.

## Making a change

- Open an issue first for anything substantial, so the design is agreed before you build it.
- Keep each pull request focused on one change.
- Run `./scripts/ci-local.sh` before you push. It runs every job of the workflows that gate a
  pull request and a push to main, with their own steps, in a Linux container built to match
  GitHub's runner, and runs the suite on your own machine as well. It needs Docker, and
  `./scripts/ci-local.sh --list` shows what each phase runs and the few steps it cannot run.
  `./scripts/ci-local.sh fast` is the inner loop: build, vet, gofmt, and the race suite on your
  own machine only.
- Match the surrounding code style.

Questions about licensing or a larger contribution: licensing@switchtender.com
