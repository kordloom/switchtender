# Roadmap

What is being built next, in the order it is being built. Shipped work moves off this page and
into the [release notes](https://github.com/kordloom/switchtender/releases).

## Engines

SwitchTender governs automation estates: approvals, drift, host history, and sealed evidence
over the tools a fleet already runs.

- **Ansible** is engine one, shipped, alongside Terraform, OpenTofu, Bash, PowerShell, Python,
  and Go, with one-command imports from AWX, AAP, Tower, Ascender, Semaphore, Rundeck, Jenkins,
  and crontabs.
- **Chef and Puppet fleets import today**, as inventories: every node, grouped by environment
  and, for Chef, by every role in its run list. Cookbooks and manifests do not convert and are
  not going to, because a partial translation of a program in another language would read like
  the original without doing what it does.
- **OpenVox is engine two.** Report ingestion, node classification, and orchestration for
  Puppet estates running the open fork, with the same approval gates and offline-verifiable
  evidence. That is the part still ahead: importing a fleet is not the same as running it. We
  are scoping it with Puppet operators now; if that is you,
  [we want to talk](mailto:hello@kordloom.com?subject=OpenVox%20on%20SwitchTender).

## Near term

- SQLite to PostgreSQL full-history migration in one command, so a Community install moves to
  Team's Postgres and HA without leaving its run history behind.
- Card checkout for Team, replacing the email-and-invoice path.
- A hosted witness enrollment command, so an Enterprise install registers for countersigned
  attestations in one line.

Dates are deliberately absent. Everything here ships when it holds up under the same gates as
everything before it.
