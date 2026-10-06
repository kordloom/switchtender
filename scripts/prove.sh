#!/usr/bin/env bash
#
# prove.sh walks the one claim this product is built on, end to end, against a server it starts
# itself. Every call is a real HTTP request to a real SwitchTender, made with a real token: the agent
# is an agent token, the person who approves holds a person's token, and the server decides what
# each may do. The deletion it performs genuinely destroys a directory, and the verification at the
# end genuinely fails once a byte is altered.
#
# It is written as curl rather than as a polished command on purpose. The audience for this is
# somebody who does not believe the claim yet, and a wall of raw requests they can read beats a
# binary that prints reassuring sentences.
#
#   ./scripts/prove.sh [path-to-switchtender]
#
# With no argument it uses the switchtender on PATH. PROVE_PORT picks the loopback port (default
# 18799). No approval policy is written at any point: the agent's run is held by the hold every
# install starts with, which is the default this demonstrates. It needs curl and python3, writes
# only inside its own temporary directory, and removes it on exit.

set -euo pipefail

BIN="${1:-$(command -v switchtender || echo ./.bin/switchtender)}"
PORT="${PROVE_PORT:-18799}"
API="http://127.0.0.1:${PORT}"
WORK="$(mktemp -d)"
SANDBOX="$WORK/sandbox"
DB="$WORK/prove.db"
trap 'rm -rf "$WORK"; [ -n "${SRV:-}" ] && kill "$SRV" 2>/dev/null || true' EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\n\033[31mFAILED: %s\033[0m\n' "$*"; exit 1; }
ok()   { printf '   \033[32mok\033[0m %s\n' "$*"; }

command -v curl    >/dev/null || fail "curl is required"
command -v python3 >/dev/null || fail "python3 is required"
[ -x "$BIN" ]                 || fail "no switchtender binary at $BIN (pass one as \$1)"

jqp() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)"; }

step "0. Two people and an agent, each with a token of their own"
# The accounts exist before the server starts, so it enforces tokens from its first request: a
# server with no tokens on a loopback bind answers anybody, which would make every refusal below
# meaningless. The passwords are random and never used; everything here signs in with a token.
# init creates the install and its first admin, the approver, the way any install starts. The user
# and token commands work on an existing install and refuse a database that is not there.
secret() { python3 -c 'import secrets;print(secrets.token_urlsafe(24))'; }
SWITCHTENDER_ADMIN_PASSWORD="$(secret)" \
  "$BIN" init --db "$DB" --admin approver --config "$WORK/switchtender.env" >/dev/null 2>&1 \
  || fail "init could not create the install"
SWITCHTENDER_PASSWORD="$(secret)" "$BIN" user new dev-lead --role operator --db "$DB" >/dev/null
# The agent's token is bound to the person it acts for, so the record names both, and --agent caps
# it below admin whatever that person's role: it can ask for changes and can never decide on one.
AGENT=$("$BIN" token new --user dev-lead --name release-agent --agent --db "$DB" | jqp 'd["token"]')
PERSON=$("$BIN" token new --user approver --name approver --db "$DB" | jqp 'd["token"]')
ok "dev-lead (operator), approver (admin), and release-agent, an agent acting for dev-lead"

step "Starting a SwitchTender on $API"
mkdir -p "$SANDBOX"
echo "the quarterly backups nobody kept a second copy of" > "$SANDBOX/backups.txt"
SWITCHTENDER_ENCRYPTION_KEY="$(python3 -c 'import secrets;print(secrets.token_hex(32))')" \
SWITCHTENDER_ENCRYPTION_SALT="$(python3 -c 'import secrets;print(secrets.token_hex(16))')" \
  "$BIN" serve --addr "127.0.0.1:${PORT}" --db "$DB" >"$WORK/server.log" 2>&1 &
SRV=$!
for _ in $(seq 1 60); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' "$API/healthz" || true)" = "200" ] && break
  sleep 1
done
[ "$(curl -s -o /dev/null -w '%{http_code}' "$API/healthz")" = "200" ] \
  || fail "server did not come up; see $WORK/server.log"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$API/v1/runs")" = "401" ] \
  || fail "the server answered without a token, so nothing below would prove anything"
ok "running, answering nobody without a token, and holding a sandbox at $SANDBOX"

step "1. No approval policy, on purpose"
# Nothing is written here. An agent's run waits for a person by default, so the hold below comes
# from the install as it starts, not from a rule somebody remembered to add. A person reading the
# policy list sees it empty.
POLICIES=$(curl -sS -H "authorization: Bearer $PERSON" "$API/v1/policies")
COUNT=$(echo "$POLICIES" | jqp 'd.get("count", -1)')
[ "$COUNT" = "0" ] || fail "the install already holds approval policies: $POLICIES"
ok "the policy list is empty. Nothing below depends on a rule being written first."

step "2. An agent asks to delete the backups"
SUBMIT=$(curl -sS -X POST "$API/v1/runs" -H 'content-type: application/json' \
  -H "authorization: Bearer $AGENT" -d "{
    \"tool\": \"bash\",
    \"command\": \"rm -rf $SANDBOX\",
    \"labels\": {\"change\": \"prove\"}
  }")
RUN=$(echo "$SUBMIT" | jqp 'd.get("id","")')
[ -n "$RUN" ] || fail "run was not accepted: $SUBMIT"
ok "run $RUN submitted with the agent's token, not a person's"

step "3. SwitchTender grades it before anything executes"
DETAIL=$(curl -sS -H "authorization: Bearer $PERSON" "$API/v1/runs/$RUN")
STATUS=$(echo "$DETAIL" | jqp 'd.get("status","")')
echo "$DETAIL" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print('   asked by     :', d.get('actor'), '(' + str(d.get('actor_type')) + ')')
print('   status       :', d.get('status'))
print('   held by      :', d.get('held_by_policy'))
print('   risk         :', (d.get('risk') or {}).get('level'))
print('   reversibility:', (d.get('reversibility') or {}).get('class'))
for r in (d.get('reversibility') or {}).get('reasons') or []:
    print('     reason     :', r)
"
[ "$STATUS" = "pending_approval" ] || fail "the run was not held; status was $STATUS"
HELD=$(echo "$DETAIL" | jqp 'd.get("held_by_policy","")')
[ "$HELD" = "requested by an agent, held by default" ] \
  || fail "the run was held by \"$HELD\", not by the default hold on an agent's run"
ok "held by default, because an agent asked. No policy was written. The sandbox is still there:"
ls "$SANDBOX" | sed 's/^/     /'

step "4. The agent tries to approve its own request"
SELF=$(curl -sS -o "$WORK/self.json" -w '%{http_code}' -X POST "$API/v1/runs/$RUN/approve" \
  -H "authorization: Bearer $AGENT" -d '{}')
if [ "$SELF" = "200" ]; then
  fail "the agent approved its own run, which is the whole thing this is supposed to prevent"
fi
ok "refused with HTTP $SELF: an agent token can ask for a change and never decide on one"

step "5. A person approves it"
APPROVE=$(curl -sS -o "$WORK/approve.json" -w '%{http_code}' -X POST "$API/v1/runs/$RUN/approve" \
  -H "authorization: Bearer $PERSON" -d '{}')
[ "$APPROVE" = "200" ] || fail "approval failed with HTTP $APPROVE: $(cat "$WORK/approve.json")"
ok "approved by approver, bound to the exact specification that was graded"

step "6. It runs, and the deletion is real"
for _ in $(seq 1 45); do
  FINAL=$(curl -sS -H "authorization: Bearer $PERSON" "$API/v1/runs/$RUN" | jqp 'd.get("status","")')
  case "$FINAL" in succeeded|failed|canceled) break ;; esac
  sleep 1
done
echo "   final status : $FINAL"
if [ -d "$SANDBOX" ]; then
  fail "the sandbox still exists, so nothing actually executed"
fi
ok "the sandbox is gone. Nothing here was simulated."

step "7. The trail says how far it can be trusted"
curl -sS -H "authorization: Bearer $PERSON" "$API/v1/audit/verify" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print('   ok       :', d.get('ok'))
print('   entries  :', d.get('count'))
print('   anchored :', d.get('anchored'))
print('   level    :', d.get('level'), d.get('level_name'))
"
ok "a level, not a bare ok. Anchor it and the level rises; this install never did."

step "8. The signed evidence bundle"
curl -sS -H "authorization: Bearer $PERSON" "$API/v1/audit/bundle" -o "$WORK/bundle.json"
python3 - "$WORK/bundle.json" <<'PY' || fail "the bundle does not record who asked for the deletion"
import json, sys
d = json.load(open(sys.argv[1]))
print('   claims    :', len(d.get('claims', [])))
print('   producer  :', (d.get('producer') or {}).get('key_id', '')[:24], '...')
print('   signatures:', len(d.get('signatures', [])))
# The submission is the claim a reader cares about: who asked for the deletion, and for whom.
for c in d.get('claims', []):
    p = c.get('payload') or {}
    if p.get('method') == 'POST' and p.get('path') == '/v1/runs':
        print('   asked by  :', p.get('actor'), '(' + str(p.get('actor_type')) + '), acting for',
              p.get('on_behalf_of'))
        ok = p.get('actor_type') == 'agent' and p.get('on_behalf_of') == 'dev-lead'
        sys.exit(0 if ok else 1)
sys.exit(1)
PY
ok "written to $WORK/bundle.json, naming the agent and the person it acted for"

step "9. Verify it, then alter one character and verify again"
# Pinned to the key the server publishes, so the check says who signed it as well as that it is
# intact: an unpinned bundle verifies against whatever key it carries, which proves nothing about
# whose it is.
KEY=$(curl -sS "$API/.well-known/loomseal.json" | jqp 'd["key_id"]')
[ -n "$KEY" ] || fail "the server published no signing key to pin"
VERIFY_OK=$("$BIN" verify "$WORK/bundle.json" --pubkey "$KEY" >/dev/null 2>&1 && echo yes || echo no)
[ "$VERIFY_OK" = "yes" ] || fail "an untouched bundle did not verify against the published key"
ok "the untouched bundle verifies against the published key $KEY"

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

if "$BIN" verify "$WORK/tampered.json" --pubkey "$KEY" >/dev/null 2>&1; then
  fail "the altered bundle still verified, which would make the whole product worthless"
fi
ok "the altered bundle is refused"

printf '\n\033[1mThat is the product.\033[0m\n'
cat <<'EOF'

  An agent asked to destroy something with a token of its own. It was graded and
  held with no policy written, and refused when it tried to approve itself. A
  person approved the exact specification that was graded. It ran, and the
  deletion was real. What is left is a signed record, naming the agent and the
  person it acted for, that verifies against the server's published key and
  stops verifying the moment anyone edits it.

  Nothing above trusted this tool's own word for anything: the sandbox really was
  deleted, and the last check fails on purpose.
EOF
