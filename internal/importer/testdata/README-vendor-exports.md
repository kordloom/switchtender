# Vendor-shaped AWX exports

Three files here are built key for key from each vendor's published export code. None was captured
from a running server. Each supports one claim: the importer reads the file that export code writes
at the named version. None supports a claim that the importer was tested against the product.

All sources were read on 2026-11-12 from GitHub at the tags named.

| File                              | Client and tag              | Server fields from                                      |
|-----------------------------------|-----------------------------|---------------------------------------------------------|
| `awxkit-24.6.1-export.json`       | ansible/awx awxkit, 24.6.1  | ansible/awx `awx/api/serializers.py`, 24.6.1            |
| `awxkit-17.1.0-export.json`       | ansible/awx awxkit, 17.1.0  | ansible/awx `awx/api/serializers.py`, 17.1.0            |
| `ascender-kit-25.6.3-export.json` | ctrliq/ascender-kit, 25.6.3 | ctrliq/ascender `ascender/api/serializers/*.py`, 25.6.3 |

## How each object was built

The export code is `ApiV2._export` and `export_assets` in `awxkit/awxkit/api/pages/api.py` (and
`ascenderkit/api/pages/api.py` for ascender-kit). For every object it writes:

- The endpoint's POST fields that the object holds, which the server decides. `get_post_fields` in
  `awxkit/awxkit/api/utils.py` reads them from OPTIONS, and `Metadata.determine_actions` in
  `awx/api/metadata.py` (24.6.1) drops every read-only field from POST. So each object here carries
  the fields its serializer's `Meta.fields` lists, less `id`, `type`, `url`, `related`,
  `summary_fields`, `created`, `modified`, fields the serializer declares read-only, and model
  fields that are not editable.
- A foreign key that is set as the related object's natural key, and one that is not set as null.
- A `related` block holding each relation in `EXPORTABLE_RELATIONS` the endpoint can attach, as
  natural keys, and each pair in `DEPENDENT_EXPORT`, as whole objects.
- `natural_key`, from the page class's `NATURAL_KEY` in `awxkit/awxkit/api/pages/*.py`.

The top-level keys are `EXPORTABLE_RESOURCES`: eleven at 17.1.0, fourteen at 24.6.1 and in
ascender-kit 25.6.3. Every key is present. Users, teams, credential types, notification templates,
inventory sources, execution environments, and applications are empty lists. A real server holds
at least its admin user, and the importer reads none of those kinds, so the key alone is what the
files need to carry. The Machine credential type is managed, and `_export` skips a managed object,
so it would not be exported in any case.

The estate is invented: one organization, one git project, one inventory of three hosts in two
groups, one Machine credential with a password and a become password, and job templates and
workflows over them. The workflow `Release` opens with an approval: deploy on approval, page the
on-call on a denial or a timeout, with a nightly schedule.

Left out on purpose: an organization's `credentials`, `execution_environments`, and
`notification_templates` lists. Whether those endpoints accept an attach, which decides whether
`_export` writes them, is set by the server's view classes, which these files were not built from.

## What differs by client

- awxkit 24.6.1 writes the approval as `related.create_approval_template` with `name`,
  `description`, and `timeout`, and leaves `unified_job_template` off the approval node
  (`api.py` 122 to 125). Schedules are written at the top level and under their workflow.
  Secrets are written as `$encrypted$`.
- awxkit 17.1.0, the Tower 3.8 era client, has no page for a workflow approval template
  (`workflow_approval_templates.py` is absent at 17.1.0 and present at 21.4.0). The approval node's
  template has no natural key, so `_export` returns None for the node (`api.py` 118 to 122) and the
  list drops it (`api.py` 159). The node's own edges go with it, and no other node points at an
  opening approval, so the file holds no trace of it. There is no top-level schedules list, since
  `schedules` joins `EXPORTABLE_RESOURCES` at 21.3.0, so the schedule is under its workflow only.
  `_export` passes every object through `remove_encrypted` (`api.py` 172), so every secret is an
  empty string rather than `$encrypted$`.
- ascender-kit 25.6.3 writes what awxkit 24.6.1 writes, plus Ascender's own fields. Every node
  carries `max_retries` and a `related.condition_nodes` list, empty when the node has no conditional
  connector, and each connector holds `id` (the target's natural key), `trigger`, `artifact_key`,
  `operator`, and `expected_value` (`api.py` 191 to 214). An approval carries `context_template`,
  `required_approvals`, and `on_timeout` (`WorkflowJobTemplateNodeCreateApprovalSerializer` in
  `ascender/api/serializers/workflow.py`). `webhook_key` is never written, since its related page
  has no natural key (`api.py` 127 to 133). Its three workflows are `Release` with a two-person
  approval, `Canary` with a conditional connector, and `Nightly` with an approval that approves
  itself on a timeout and carries a context template, beside a node retried twice.

## Source files

- https://github.com/ansible/awx/blob/24.6.1/awxkit/awxkit/api/pages/api.py
- https://github.com/ansible/awx/blob/24.6.1/awxkit/awxkit/api/utils.py
- https://github.com/ansible/awx/blob/24.6.1/awx/api/serializers.py
- https://github.com/ansible/awx/blob/24.6.1/awx/api/metadata.py
- https://github.com/ansible/awx/blob/17.1.0/awxkit/awxkit/api/pages/api.py
- https://github.com/ansible/awx/blob/17.1.0/awxkit/awxkit/api/pages/page.py
- https://github.com/ansible/awx/blob/17.1.0/awxkit/awxkit/api/utils.py
- https://github.com/ansible/awx/blob/17.1.0/awx/api/serializers.py
- https://github.com/ansible/awx/blob/21.4.0/awxkit/awxkit/api/pages/workflow_approval_templates.py
- https://github.com/ctrliq/ascender-kit/blob/25.6.3/ascenderkit/api/pages/api.py
- https://github.com/ctrliq/ascender-kit/blob/25.6.3/ascenderkit/api/pages/workflow_job_template_nodes.py
- https://github.com/ctrliq/ascender/blob/25.6.3/ascender/api/serializers/workflow.py
- https://github.com/ctrliq/ascender/blob/25.6.3/ascender/main/models/workflow.py

The natural keys come from `awxkit/awxkit/api/pages/` `organizations.py`, `projects.py`,
`inventory.py`, `credentials.py`, `job_templates.py`, `workflow_job_templates.py`,
`workflow_job_template_nodes.py`, and `schedules.py` at each awxkit tag.
