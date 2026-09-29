<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Advisory AI

SwitchTender runs a complete control plane with no model anywhere near it. AI is an optional layer
you switch on for an edge, never a dependency: it is off until you set `--ai-provider`, and
nothing in the product waits on it.

## The guarantees

The rule is one sentence: AI proposes, the control plane governs.

- A provider only ever produces text a human reads or a proposal a human releases. It executes
  nothing and sits in no execution path.
- Anything AI drafts that could become a run is born held at the same approval gate an operator
  faces. An admin reviews the exact generated command before anything moves.
- Credential values are masked out of logs and events before any feature builds a prompt, so a
  model never sees a secret.
- Requests and decisions land in the tamper-evident audit trail, including the plain-language
  request a proposal was built from.

## The five features

| Feature | Where | What it does |
|---------|-------|--------------|
| Run triage | A failed run's page, `POST /v1/runs/{id}/explain` | Explains the failure from the run's masked log and failed task events, on demand. |
| Step drafting | The workflow editor, `POST /v1/ai/draft` | Drafts a bash, python, powershell, or go step script from a description, for you to review, edit, and save. It never executes on its own. |
| Fleet questions | The overview page, `POST /v1/ai/ask` | Answers a plain-language question from a bounded snapshot of run counts, recent runs, host health, and drift. Metadata only, rate limited. |
| Run proposals | The runs page, `POST /v1/ai/propose-run` | Turns a plain-language request into a validated run the server builds, born held for approval and stamped with your exact words. |
| Drift reconcile | The drift page | One click on a drifted host builds the fix run, limited to that host, born held for approval. The run is constructed deterministically, with no model involved, so this one works with no provider configured. |

## Providers

| Provider | Runs where | Setup |
|----------|-----------|-------|
| `ollama` | Your hardware | `--ai-provider ollama --ai-model llama3.1`, optional `--ai-url` for a remote Ollama. |
| `anthropic` | Anthropic's API | `--ai-provider anthropic --ai-model claude-sonnet-5` plus `SWITCHTENDER_AI_KEY`. |
| `openai` | Any OpenAI-compatible endpoint | `--ai-provider openai --ai-model <model> --ai-url <server>` plus `SWITCHTENDER_AI_KEY`. Covers OpenAI itself, vLLM, LM Studio, and OpenRouter. |

A new backend plugs in without touching the core: register a provider through the
[Go SDK](sdk.md), compiled in or as a drop-in plugin binary.

When the Anthropic model is a Fable or Mythos model, SwitchTender opts into server-side fallbacks,
so a request declined by a safety classifier retries on `claude-opus-4-8` in the same call and
the feature keeps working. A Fable model also requires 30-day data retention on the account.

## What a model sees

With `ollama`, nothing leaves your machines. With a cloud provider, prompts carry automation
content: commands, playbook names, masked failed-run logs, drift summaries, and the questions
you type. Credential values never appear, because masking runs before any prompt is built, and
fleet questions send metadata only, never file contents or inventories. If that boundary is too
wide for your environment, run Ollama locally or leave AI off. Every other page of this
documentation works the same either way.

See [configuration](configuration.md) for every flag, the [HTTP API](api.md) for the endpoints,
and [features](features.md) for the full capability list.
