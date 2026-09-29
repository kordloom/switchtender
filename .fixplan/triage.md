# The 71, triaged

## TIER 1 — CRASHES (5). A panic in a server takes the process.
- schedule/NextFire panics on a bare "CRON_TZ=" spec. WORST: a stored row with that
  expression is read on every tick, so it kills the scheduler on every restart.
- util/Clip(s, negative) panics with a slice bounds error.
- witness/clip panics on a negative limit.
- importer/dtstartZone panics on a DTSTART whose uppercase form is longer.
- relay/NewClient accepts a nil Transport, panicking mid-run instead of at startup.

## TIER 2 — SECRETS AND AUTH (11). The product's core promise.
- util/SecretKey misses hyphenated stems, so X-Api-Key and private-key are NOT redacted.
- util: an X-Api-Key value survives RedactAssignments and REACHES THE AUDIT DIGEST.
- server/auditPath records the raw path for /hooks/<token>/../x, writing the webhook
  token into the permanent audit record.
- secretsource: vault, vault_dynamic, gsm, and command return an EMPTY SECRET WITH NO
  ERROR. A run proceeds with no credential and no signal. Fails open.
- secretsource: a Conjur secret larger than httpMaxBody silently truncates.
- secretsource/resolveCommand reads stdout into an UNBOUNDED buffer.
- secretsource/resolveCommand ignores its context deadline while a grandchild holds the pipe.
- secretsource/checkResolveURL renders url.Parse's error, repeating the raw address.
- project/redactRepoURL leaves a secret in scp-like shorthand untouched.
- cmd: token new discards a positional argument and mints an unscoped admin token.
- cmd: a mistyped subcommand under audit/token/user/license/import exits 0.

## TIER 3 — CORRECTNESS AND INTEGRITY (rest)
policy: canonicalRule omits Queue, so moving a rule between queues does not change the
in-force digest; deny rules match case-sensitively so "DROP DATABASE" evades a "drop
database" refusal. witness: one hostile feed becomes 999 findings from a single poll.
dispatch: waitChildren reports a succeeded shard as canceled. dossier: any decision not
/rejected is labeled Approved. Plus importer, sqlitestore, mcp, run, roundhouse, backup,
project, identity, demo, receipt, relay, ui, user.
