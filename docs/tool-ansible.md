<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Ansible runs

An Ansible run executes `ansible-playbook` against an inventory. It is the default tool: a run
that names no tool is an Ansible run. It is also the most instrumented one, because the embedded
callback plugin reports every task on every host as a structured event, and those events paint the
live host-by-task matrix, feed fleet memory, and drive drift detection.

## What runs

The playbook path is the run's target, resolved inside the project checkout when the run sources a
project, so relative paths and roles behave the way they do in the repository. A project's
`requirements.yml` roles and collections install on sync before the play starts. A dry run passes
`--check`, which reports what would change without changing it; drift detection is built from
exactly those check runs.

Ansible runs are the ones that split. A split run shards the inventory across parallel slices,
packs hosts onto shards by their measured durations from past runs, and merges every slice back
into one matrix. Failed shards retry alone.

## How values reach the play

- Extra vars, including survey answers and template vars, arrive as Ansible extra vars, so
  `{{ region }}` in a play reads a survey answer named `region`.
- An `ssh_key` credential is decrypted to a private temp file for the connection and wiped when
  the run ends.
- A `vault_password` credential unlocks `ansible-vault` content for the duration of the run.
- A `become_password` credential supplies privilege escalation.
- An `env` credential's `KEY=VALUE` lines are set in the process environment.
- Credentials attached to the run's inventory arrive the same way, so a fleet's secrets reach the
  play without naming them on the run.

## Example

    - name: Site deploy
      hosts: all
      tasks:
        - name: Apply configuration
          ansible.builtin.template:
            src: app.conf.j2
            dest: "/etc/app/{{ region }}.conf"

Launched from a template with a `region` survey answer, every host paints its own cell in the
matrix as the task lands.

See also [Bash runs](tool-bash.md), [Terraform runs](tool-terraform.md),
[drift detection](drift.md), and the [tutorials](tutorials.md).
