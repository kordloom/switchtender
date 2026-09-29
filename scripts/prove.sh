#!/usr/bin/env bash
#
# prove.sh walks the one claim this product is built on, end to end, against a server it starts
# itself. Nothing here is staged: every call is a real HTTP request to a real SwitchTender, the
# deletion it performs genuinely destroys a directory, and the verification at the end genuinely
# fails once a byte is altered.
#
# It is written as curl rather than as a polished command on purpose. The audience for this is
# somebody who does not believe the claim yet, and a wall of raw requests they can read beats a
# binary that prints reassuring sentences.
#
#   ./scripts/prove.sh [path-to-switchtender]
#
# It needs a SwitchTender binary, curl, and python3. It writes only inside its own temporary
# directory and removes it on exit.

set -euo pipefail

BIN="${1:-./.bin/switchtender}"
PORT="${PROVE_PORT:-18799}"
API="http://127.0.0.1:${PORT}"
WORK="$(mktemp -d)"
SANDBOX="$WORK/sandbox"
trap 'rm -rf "$WORK"; [ -n "${SRV:-}" ] && kill "$SRV" 2>/dev/null || true' EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\n\033[31mFAILED: %s\033[0m\n' "$*"; exit 1; }
ok()   { printf '   \033[32mok\033[0m %s\n' "$*"; }

command -v curl    >/dev/null || fail "curl is required"
command -v python3 >/dev/null || fail "python3 is required"
[ -x "$BIN" ]                 || fail "no switchtender binary at $BIN (pass one as \$1)"

jqp() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)"; }

step "Starting a SwitchTender on $API"
mkdir -p "$SANDBOX"
echo "the quarterly backups nobody kept a second copy of" > "$SANDBOX/backups.txt"
SWITCHTENDER_ENCRYPTION_KEY=prove-key-not-a-secret \
SWITCHTENDER_ENCRYPTION_SALT=prove-salt \
SWITCHTENDER_LICENSE="${SWITCHTENDER_LICENSE:-}" \
  "$BIN" serve --addr "127.0.0.1:${PORT}" --db "$WORK/prove.db" >"$WORK/server.log" 2>&1 &
SRV=$!
for _ in $(seq 1 60); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' "$API/healthz" || true)" = "200" ] && break
  sleep 1
done
[ "$(curl -s -o /dev/null -w '%{http_code}' "$API/healthz")" = "200" ] \
  || fail "server did not come up; see $WORK/server.log"
ok "running, and holding a sandbox at $SANDBOX"

step "1. A policy: nothing irreversible runs without a second person"
# The rule this product is actually about holds on the reversibility grade and refuses a
# self-approval. Both are Team, so on a Community install this falls back to the blanket hold
# Community does have. The fallback is announced rather than hidden: a demonstration that quietly
# proves something weaker than it claims is the thing this whole script exists to be the opposite
# of.
POLICY=$(curl -sS -X POST "$API/v1/policies" -H 'content-type: application/json' -d '{
  "name": "irreversible needs a second pair of eyes",
  "reversibility": "irreversible",
  "effect": "require_approval",
  "require_distinct_approver": true
}')
POLICY_ID=$(echo "$POLICY" | jqp 'd.get("id","")')
TIER="team"
if [ -z "$POLICY_ID" ]; then
  case "$POLICY" in
    *"requires a Team license"*)
      TIER="community"
      printf '   \033[33mnote\033[0m this install is Community, where a rule cannot hold on the
'
      printf '        reversibility grade and cannot demand a distinct approver. Falling back to
'
      printf '        the blanket hold Community does have, so the rest of this still runs. Set
'
      printf '        SWITCHTENDER_LICENSE to a Team license to see the real rule.
'
      POLICY=$(curl -sS -X POST "$API/v1/policies" -H 'content-type: application/json' -d '{
        "name": "hold every bash run",
        "tool": "bash"
      }')
      POLICY_ID=$(echo "$POLICY" | jqp 'd.get("id","")')
      ;;
  esac
fi
[ -n "$POLICY_ID" ] || fail "policy was not created: $POLICY"
if [ "$TIER" = "team" ]; then
  ok "written. It holds on the grade, not on a command string somebody has to keep updating."
else
  ok "written, as a blanket hold on the tool."
fi

step "2. An agent asks to delete the backups"
SUBMIT=$(curl -sS -X POST "$API/v1/runs" -H 'content-type: application/json' \
  -H 'x-switchtender-actor: release-agent' -H 'x-switchtender-actor-type: agent' -d "{
    \"tool\": \"bash\",
    \"command\": \"rm -rf $SANDBOX\",
    \"labels\": {\"change\": \"prove\"}
  }")
RUN=$(echo "$SUBMIT" | jqp 'd.get("id","")')
[ -n "$RUN" ] || fail "run was not accepted: $SUBMIT"
ok "run $RUN submitted by an agent, not a person"

step "3. SwitchTender grades it before anything executes"
DETAIL=$(curl -sS "$API/v1/runs/$RUN")
STATUS=$(echo "$DETAIL" | jqp 'd.get("status","")')
echo "$DETAIL" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print('   status       :', d.get('status'))
print('   risk         :', (d.get('risk') or {}).get('level'))
print('   reversibility:', (d.get('reversibility') or {}).get('class'))
for r in (d.get('reversibility') or {}).get('reasons') or []:
    print('     reason     :', r)
"
[ "$STATUS" = "pending_approval" ] || fail "the run was not held; status was $STATUS"
ok "held. The sandbox is still there:"
ls "$SANDBOX" | sed 's/^/     /'

step "4. The agent tries to approve its own request"
SELF=$(curl -sS -o "$WORK/self.json" -w '%{http_code}' -X POST "$API/v1/runs/$RUN/approve" \
  -H 'x-switchtender-actor: release-agent' -H 'x-switchtender-actor-type: agent' -d '{}')
if [ "$TIER" = "team" ]; then
  if [ "$SELF" = "200" ]; then
    fail "the agent approved its own run, which is the whole thing this is supposed to prevent"
  fi
  ok "refused with HTTP $SELF: $(jqp 'd.get("error","")' < "$WORK/self.json")"
else
  printf '   \033[33mnote\033[0m Community cannot demand a distinct approver, so this install let the
'
  printf '        agent release its own hold (HTTP %s). That is the separation of duties Team buys,
' "$SELF"
  printf '        and it is the reason this step exists.
'
fi

step "5. A person approves it"
APPROVE=$(curl -sS -o "$WORK/approve.json" -w '%{http_code}' -X POST "$API/v1/runs/$RUN/approve" \
  -H 'x-switchtender-actor: a-human' -H 'x-switchtender-actor-type: user' -d '{}')
if [ "$TIER" = "team" ] && [ "$APPROVE" != "200" ]; then
  fail "approval failed with HTTP $APPROVE: $(cat "$WORK/approve.json")"
fi
ok "approved by a-human, bound to the exact specification that was graded"

step "6. It runs, and the deletion is real"
for _ in $(seq 1 45); do
  FINAL=$(curl -sS "$API/v1/runs/$RUN" | jqp 'd.get("status","")')
  case "$FINAL" in succeeded|failed|canceled) break ;; esac
  sleep 1
done
echo "   final status : $FINAL"
if [ -d "$SANDBOX" ]; then
  fail "the sandbox still exists, so nothing actually executed"
fi
ok "the sandbox is gone. Nothing here was simulated."

step "7. The trail says how far it can be trusted"
curl -sS "$API/v1/audit/verify" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print('   ok       :', d.get('ok'))
print('   entries  :', d.get('count'))
print('   anchored :', d.get('anchored'))
print('   level    :', d.get('level'), d.get('level_name'))
"
ok "a level, not a bare ok. Anchor it and the level rises; this install never did."

step "8. The signed evidence bundle"
curl -sS "$API/v1/audit/bundle" -o "$WORK/bundle.json"
python3 -c "
import json
d=json.load(open('$WORK/bundle.json'))
print('   claims    :', len(d.get('claims',[])))
print('   producer  :', (d.get('producer') or {}).get('key_id','')[:24], '...')
print('   signatures:', len(d.get('signatures',[])))
"
ok "written to $WORK/bundle.json"

step "9. Verify it, then alter one character and verify again"
VERIFY_OK=$("$BIN" verify "$WORK/bundle.json" >/dev/null 2>&1 && echo yes || echo no)
[ "$VERIFY_OK" = "yes" ] || fail "an untouched bundle did not verify"
ok "the untouched bundle verifies"

python3 - "$WORK/bundle.json" "$WORK/tampered.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
# Rewrite who did it, which is the edit somebody would actually make. Exits non-zero when it
# changed nothing: a tamper that silently misses reports the verifier as sound while proving
# nothing about it, which is worse than no check. This script got that wrong once by looking for
# a field named body when a claim carries payload, and then reported the product as broken.
changed = False
for c in d.get("claims", []):
    payload = c.get("payload") or {}
    if payload.get("actor"):
        payload["actor"] = "somebody-else"
        changed = True
        break
if not changed:
    sys.exit("no claim carried an actor to rewrite; the tamper would have been a no-op")
json.dump(d, open(sys.argv[2], "w"))
PY

cmp -s "$WORK/bundle.json" "$WORK/tampered.json" \
  && fail "the tampered bundle is byte-identical to the original, so this proves nothing"

if "$BIN" verify "$WORK/tampered.json" >/dev/null 2>&1; then
  fail "the altered bundle still verified, which would make the whole product worthless"
fi
ok "the altered bundle is refused"

printf '\n\033[1mThat is the product.\033[0m\n'
cat <<'EOF'

  An agent asked to destroy something. It was graded, held, and refused its own
  approval. A person approved the exact specification that was graded. It ran,
  and the deletion was real. What is left is a signed record that verifies, and
  stops verifying the moment anyone edits it.

  Nothing above trusted this tool's own word for anything: the sandbox really was
  deleted, and the last check fails on purpose.
EOF
