#!/usr/bin/env bash
# Proxy-layer drills: relay outage, direct-first behaviour, health reporting.
set -uo pipefail
cd "$(dirname "$0")/.."
BASE_URL="${BASE_URL:-http://127.0.0.1:8080}"
COMPOSE="${COMPOSE:-docker compose}"
KEY="${API_KEY:-$(grep -E '^API_KEY=' .env 2>/dev/null | cut -d= -f2)}"
TMP=$(mktemp -d); FAILED=0
trap 'rm -rf "$TMP"' EXIT

ok(){ echo "PASS: $*"; }
fail(){ echo "FAIL: $*"; FAILED=1; }
post(){ curl -sS -m 45 -o "$2" -w '%{http_code}' -X POST "$BASE_URL$1" -H 'Content-Type: application/json' -H "Authorization: Bearer $KEY" -d "$3"; }
jget(){ python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(eval(sys.argv[2]))" "$1" "$2" 2>/dev/null; }
wait_healthy(){ local n=$1 t=${2:-120} w=0; while [ $w -lt $t ]; do [ "$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "tavilyd-$n" 2>/dev/null)" = healthy ] && return 0; sleep 3; w=$((w+3)); done; return 1; }
goroutines(){ docker run --rm --network container:tavilyd-api alpine:3.20 wget -qO- 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1' 2>/dev/null | awk '/goroutine profile: total/ {print $4; exit}'; }

echo "=== pre-flight ==="
code=$(curl -sS -o "$TMP/h.json" -w '%{http_code}' -m 5 "$BASE_URL/health")
[ "$code" = 200 ] && ok "health 200" || fail "health $code"
[ -n "$(jget "$TMP/h.json" "d.get('proxy')")" ] && ok "health exposes proxy block" || fail "proxy block missing"
G0=$(goroutines); echo "goroutines before: ${G0:-unknown}"

echo "=== drill: proxy relay stopped ==="
$COMPOSE stop proxyrelay >/dev/null 2>&1
sleep 2
code=$(post /search "$TMP/s.json" "{\"query\":\"proxy drill $RANDOM\",\"search_depth\":\"basic\",\"max_results\":2}")
[ "$code" = 200 ] && ok "basic search survives relay outage" || fail "basic search $code"
code=$(post /search "$TMP/a.json" "{\"query\":\"proxy drill adv $RANDOM\",\"search_depth\":\"advanced\",\"max_results\":2}")
[ "$code" = 200 ] && ok "advanced search survives relay outage (tier-1 fallback)" || fail "advanced search $code"
curl -sS -o "$TMP/h2.json" -m 5 "$BASE_URL/health" >/dev/null 2>&1 || true
[ "$(jget "$TMP/h2.json" "d['status']")" != "" ] && ok "health still answers while relay is down" || fail "health failed while relay down"
$COMPOSE start proxyrelay >/dev/null 2>&1
wait_healthy proxyrelay 90 && ok "relay restarted healthy" || fail "relay did not become healthy"

echo "=== drill: normal fetch after restart ==="
code=$(post /extract "$TMP/e.json" '{"urls":"https://kubernetes.io/docs/concepts/workloads/pods/probes/","format":"markdown"}')
[ "$code" = 200 ] && ok "extract works after relay restart" || fail "extract $code"
G1=$(goroutines); echo "goroutines after: ${G1:-unknown}"
if [ -n "${G0:-}" ] && [ -n "${G1:-}" ]; then d=$((G1-G0)); [ $d -le 20 ] && ok "goroutine delta $d" || fail "goroutine growth $d"; fi

[ $FAILED -eq 0 ] && { echo "RESULT: PASSED"; exit 0; }
echo "RESULT: FAILED"; exit 1
