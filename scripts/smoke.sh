#!/usr/bin/env bash
# chatmux end-to-end smoke test: build the binary, boot it with a
# throwaway config, drive two turns through the dashboard chat API, and
# verify every accounting surface (web log, usage.jsonl, /metrics,
# daemon turn log). No model key, no network needed:
#
#   phase 1 (success): generic CLI agent backed by /bin/echo — the reply
#                      must echo the marker back into the chat log.
#   phase 2 (failure): openai agent aimed at a dead endpoint — the turn
#                      must fail AND still land in usage.jsonl / metrics.
#
# Also exercises `chatmux check` on the generated config along the way.
#
# Usage: scripts/smoke.sh [port]     (default 18097; needs go + curl)
set -euo pipefail
cd "$(dirname "$0")/.."

PORT="${1:-18097}"
TOKEN="smoke"
BASE="http://127.0.0.1:${PORT}"
WORK="$(mktemp -d /tmp/cm-smoke-e2e.XXXXXX)"
BIN="$WORK/chatmux"
MARKER="ECHO-SMOKE-7f3a"
PID=""

cleanup() {
  if [ -n "$PID" ]; then
    kill "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
note() { echo "== $*"; }

# ---- build + config ----
note "building"
GOFLAGS=-buildvcs=false go build -o "$BIN" . || fail "build"

cat > "$WORK/cm.json" <<EOF
{
  "defaultAgent": "smoke-ok",
  "dataDir": "$WORK/data",
  "http": {"listen": "127.0.0.1:$PORT", "adminToken": "$TOKEN"},
  "agents": [
    {"name": "smoke-ok", "cli": {"type": "generic", "bin": "/bin/echo"}},
    {"name": "smoke-dead", "provider": "openai", "model": "gpt-4o-mini",
     "baseUrl": "http://127.0.0.1:9/v1", "apiKey": "sk-smoke"}
  ]
}
EOF

note "chatmux check"
"$BIN" check "$WORK/cm.json" || fail "config check"

# ---- boot ----
note "starting daemon on :$PORT"
"$BIN" -config "$WORK/cm.json" > "$WORK/daemon.log" 2>&1 &
PID=$!

for _ in $(seq 1 50); do
  curl -sf "$BASE/healthz" > /dev/null 2>&1 && break
  kill -0 "$PID" 2>/dev/null || { tail -5 "$WORK/daemon.log"; fail "daemon exited during boot"; }
  sleep 0.2
done
curl -sf "$BASE/healthz" > /dev/null || fail "healthz never came up"

STATUS="$(curl -sf -H "X-Admin-Token: $TOKEN" "$BASE/admin/status")" || fail "admin/status"
echo "$STATUS" | grep -q '"version"' || fail "admin/status missing version"

# post text into the chat console and wait until the log contains WANT.
send_and_wait() { # $1=text  $2=want substring  $3=deadline seconds
  curl -sf -X POST -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
    -d "{\"text\":$1}" "$BASE/admin/chat" > /dev/null || fail "POST /admin/chat"
  for _ in $(seq 1 $((5 * $3))); do
    if curl -sf -H "X-Admin-Token: $TOKEN" "$BASE/admin/chat?since=0" | grep -q "$2"; then
      return 0
    fi
    sleep 0.2
  done
  tail -10 "$WORK/daemon.log" >&2
  fail "chat log never contained: $2"
}

# ---- phase 1: successful turn via /bin/echo CLI agent ----
note "phase 1: echo turn"
send_and_wait "\"$MARKER\"" "$MARKER" 20
grep -q "turn status=ok agent=smoke-ok" "$WORK/daemon.log" || fail "no ok turn log for smoke-ok"

# ---- phase 2: failed model turn, still accounted ----
note "phase 2: dead-endpoint turn"
send_and_wait '"/smoke-dead booom"' "模型调用失败" 30
grep -q "turn status=err agent=smoke-dead" "$WORK/daemon.log" || fail "no err turn log for smoke-dead"

USAGE="$WORK/data/usage.jsonl"
[ -s "$USAGE" ] || fail "usage.jsonl missing/empty"
grep -q '"agent":"smoke-ok"' "$USAGE" || fail "usage.jsonl missing smoke-ok record"
grep -q '"agent":"smoke-dead","mode":"api"' "$USAGE" || fail "usage.jsonl missing smoke-dead record"
grep '"agent":"smoke-dead"' "$USAGE" | grep -q '"error":"[^"]' || fail "smoke-dead record has no error field"

METRICS="$(curl -sf -H "X-Admin-Token: $TOKEN" "$BASE/metrics")" || fail "metrics"
echo "$METRICS" | grep -q 'chatmux_usage_errors_total{agent="smoke-dead"} ' || fail "metrics missing smoke-dead error counter"
echo "$METRICS" | grep -q 'chatmux_usage_calls_total{agent="smoke-ok"} ' || fail "metrics missing smoke-ok call counter"

# ---- graceful stop ----
note "stopping"
kill -TERM "$PID"
for _ in $(seq 1 50); do kill -0 "$PID" 2>/dev/null || break; sleep 0.2; done
kill -0 "$PID" 2>/dev/null && fail "daemon did not exit on TERM"
grep -q "panic" "$WORK/daemon.log" && fail "panic in daemon log" || true
PID=""

echo "PASS: echo turn, failed-turn accounting, metrics, graceful stop all verified"
