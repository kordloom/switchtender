# Roadmap

What is being built next, in the order it is being built. Shipped work moves off this page and
into the [release notes](https://github.com/kordloom/switchtender/releases).

## Engines

SwitchTender governs automation estates: approvals, drift, host history, and sealed evidence
over the tools a fleet already runs.

- **Ansible** is engine one, shipped, alongside Terraform, OpenTofu, Bash, PowerShell, Python,
  and Go, with one-command imports from AWX, AAP, Tower, Ascender, Semaphore, Rundeck, Jenkins,
  and crontabs.
- **OpenVox is engine two.** Report ingestion, node classification, and orchestration for
  Puppet estates running the open fork, with the same approval gates and offline-verifiable
  evidence. We are scoping it with Puppet operators now; if that is you,
  [we want to talk](mailto:hello@kordloom.com?subject=OpenVox%20on%20SwitchTender).

## Near term

- SQLite to PostgreSQL full-history migration in one command, so a Community install moves to
  Team's Postgres and HA without leaving its run history behind.
- Card checkout for Team, replacing the email-and-invoice path.
- A hosted witness enrollment command, so an Enterprise install registers for countersigned
  attestations in one line.

Dates are deliberately absent. Everything here ships when it holds up under the same gates as
everything before it.
