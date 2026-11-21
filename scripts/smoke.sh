#!/usr/bin/env bash
# chatmux end-to-end smoke test: build the binary, boot it with a
# throwaway config, drive turns through the dashboard chat API, and
# verify every accounting surface (web log, usage.jsonl, /metrics,
# daemon turn log). No model key, no external network needed:
#
#   phase 1 (success): generic CLI agent backed by /bin/echo — the reply
#                      must echo the marker back into the chat log.
#   phase 2 (failure): openai agent aimed at a dead endpoint — the turn
#                      must fail AND still land in usage.jsonl / metrics.
#   phase 3 (fallback): dead primary + fallbackAgent → a tiny local
#                      fake SSE model server answers; the user must see
#                      the backup's marker, not a failure notice.
#   phase 4 (flood):   turnRatePerMinute 2 — the third rapid turn in a
#                      dedicated chat gets the refusal notice and bumps
#                      the rate-limit metric.
#
# Also exercises `chatmux check` on the generated config along the way.
#
# Usage: scripts/smoke.sh [port]     (default 18097; needs go + curl)
set -euo pipefail
cd "$(dirname "$0")/.."

PORT="${1:-18097}"
FBPORT=$((PORT + 1))
TOKEN="smoke"
BASE="http://127.0.0.1:${PORT}"
WORK="$(mktemp -d /tmp/cm-smoke-e2e.XXXXXX)"
BIN="$WORK/chatmux"
MARKER="ECHO-SMOKE-7f3a"
FBMARKER="FALLBACK-SMOKE-9c4"
PID=""
FAKE_PID=""

cleanup() {
  if [ -n "$PID" ]; then
    kill "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
  if [ -n "$FAKE_PID" ]; then
    kill "$FAKE_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  # full daemon log on any failure: the step log is where the evidence
  # lives, and a partial tail has already cost us one blind flake hunt
  if [ -f "$WORK/daemon.log" ]; then
    echo "== full daemon.log ($WORK) ==" >&2
    cat "$WORK/daemon.log" >&2
  fi
  exit 1
}
note() { echo "== $*"; }

# ---- build + config ----
note "building"
GOFLAGS=-buildvcs=false go build -o "$BIN" . || fail "build"

cat > "$WORK/cm.json" <<EOF
{
  "defaultAgent": "smoke-ok",
  "dataDir": "$WORK/data",
  "turnRatePerMinute": 3,
  "http": {"listen": "127.0.0.1:$PORT", "adminToken": "$TOKEN"},
  "agents": [
    {"name": "smoke-ok", "cli": {"type": "generic", "bin": "/bin/echo"}},
    {"name": "smoke-dead", "provider": "openai", "model": "gpt-4o-mini",
     "baseUrl": "http://127.0.0.1:9/v1", "apiKey": "sk-smoke"},
    {"name": "smoke-fb", "provider": "openai", "model": "gpt-4o-mini",
     "baseUrl": "http://127.0.0.1:9/v1", "apiKey": "sk-smoke",
     "fallbackAgent": "smoke-fbb"},
    {"name": "smoke-fbb", "provider": "openai", "model": "fake-model",
     "baseUrl": "http://127.0.0.1:$FBPORT/v1", "apiKey": "sk-smoke"}
  ]
}
EOF

# fake OpenAI-compatible SSE server (stdlib python3 only): serves one
# content chunk + [DONE] on any POST — the healthy backup provider.
note "starting fake model server on :$FBPORT"
python3 - "$FBPORT" "$FBMARKER" > "$WORK/fake.log" 2>&1 <<'PYEOF' &
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get("content-length", 0)))
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.end_headers()
        def chunk(obj):
            self.wfile.write(b"data: " + json.dumps(obj).encode() + b"\n\n")
        chunk({"id": "smoke", "object": "chat.completion.chunk", "model": "fake",
               "choices": [{"index": 0, "delta": {"content": MARKER}, "finish_reason": None}]})
        chunk({"id": "smoke", "object": "chat.completion.chunk", "model": "fake",
               "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]})
        self.wfile.write(b"data: [DONE]\n\n")
    def log_message(self, *a):
        pass

MARKER = sys.argv[2]
HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PYEOF
FAKE_PID=$!
FAKE_UP=""
for _ in $(seq 1 25); do
  kill -0 "$FAKE_PID" 2>/dev/null || { cat "$WORK/fake.log" >&2; fail "fake model server died"; }
  if curl -sf -m 2 -X POST "http://127.0.0.1:$FBPORT/v1/chat/completions" -d '{}' 2>/dev/null | grep -q "data:"; then
    FAKE_UP=1
    break
  fi
  sleep 0.2
done
[ -n "$FAKE_UP" ] || { cat "$WORK/fake.log" >&2; fail "fake model server never answered on :$FBPORT"; }

note "chatmux check"
"$BIN" check "$WORK/cm.json" || fail "config check"

# ---- boot ----
note "starting daemon on :$PORT"
"$BIN" -config "$WORK/cm.json" > "$WORK/daemon.log" 2>&1 &
PID=$!

for _ in $(seq 1 50); do
  curl -sf "$BASE/healthz" > /dev/null 2>&1 && break
  kill -0 "$PID" 2>/dev/null || fail "daemon exited during boot"
  sleep 0.2
done
curl -sf "$BASE/healthz" > /dev/null || fail "healthz never came up"

STATUS="$(curl -sf -H "X-Admin-Token: $TOKEN" "$BASE/admin/status")" || fail "admin/status"
echo "$STATUS" | grep -q '"version"' || fail "admin/status missing version"

# post text into the chat console and wait until the log contains WANT.
# optional 4th arg: console user (isolates the per-chat rate window).
send_and_wait() { # $1=text  $2=want substring  $3=deadline seconds  [$4=user]
  local body="{\"text\":$1}"
  if [ "${4:-}" != "" ]; then
    body="{\"text\":$1,\"user\":\"$4\"}"
  fi
  curl -sf -X POST -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
    -d "$body" "$BASE/admin/chat" > /dev/null || fail "POST /admin/chat"
  for _ in $(seq 1 $((5 * $3))); do
    if curl -sf -H "X-Admin-Token: $TOKEN" "$BASE/admin/chat?since=0" | grep -q "$2"; then
      return 0
    fi
    sleep 0.2
  done
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
echo "$METRICS" | grep -Eq 'chatmux_stream_retries_total [1-9]' || fail "metrics missing stream retry counter for the dead-endpoint turn"
echo "$METRICS" | grep -q 'chatmux_usage_calls_total{agent="smoke-ok"} ' || fail "metrics missing smoke-ok call counter"

# ---- phase 3: dead primary falls back to the healthy backup ----
note "phase 3: fallback agent turn"
send_and_wait '"/smoke-fb hello"' "$FBMARKER" 30
grep -q "turn fallback:" "$WORK/daemon.log" || fail "no fallback hop in daemon log"
grep -q "turn status=err agent=smoke-fb " "$WORK/daemon.log" || fail "no err turn log for smoke-fb"
grep -q "turn status=ok agent=smoke-fbb " "$WORK/daemon.log" || fail "no ok turn log for smoke-fbb"
grep -q '"agent":"smoke-fb","mode":"api"' "$USAGE" || fail "usage.jsonl missing smoke-fb failed attempt"
grep -q '"agent":"smoke-fbb"' "$USAGE" || fail "usage.jsonl missing smoke-fbb success record"

# ---- phase 4: per-chat rate limit (turnRatePerMinute: 3) ----
# every console turn shares one chat key, so the window holds phase 1
# plus the two turns below; the third rapid one must be refused. The
# whole run stays far inside the 60s window (phases take ~15s total).
note "phase 4: per-chat rate limit"
send_and_wait '"洪流一"' "洪流一" 20 flood
send_and_wait '"洪流二"' "洪流二" 20 flood
send_and_wait '"洪流三"' "消息太密集" 20 flood
METRICS="$(curl -sf -H "X-Admin-Token: $TOKEN" "$BASE/metrics")" || fail "metrics (rate)"
echo "$METRICS" | grep -Eq 'chatmux_turns_rate_limited_total [1-9]' || fail "metrics missing rate-limit counter"

# ---- graceful stop ----
note "stopping"
kill -TERM "$PID"
for _ in $(seq 1 50); do kill -0 "$PID" 2>/dev/null || break; sleep 0.2; done
kill -0 "$PID" 2>/dev/null && fail "daemon did not exit on TERM"
grep -q "panic" "$WORK/daemon.log" && fail "panic in daemon log" || true
PID=""

echo "PASS: echo turn, failed-turn accounting, fallback hop, rate limit, metrics, graceful stop all verified"
