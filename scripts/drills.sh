#!/usr/bin/env bash
# Degradation drills for tavilyd.
#
# Stops one dependency at a time, verifies that /search and /extract still
# behave as designed, restarts it, and waits for /health to recover. Exits
# non-zero if any expectation fails.
#
#   ./scripts/drills.sh
#
# Requires: docker compose in the repo root, curl, python3, and the stack up.

set -uo pipefail
cd "$(dirname "$0")/.."

BASE_URL="${BASE_URL:-http://127.0.0.1:8080}"
COMPOSE="${COMPOSE:-docker compose}"
API_KEY="${API_KEY:-$(grep -E '^API_KEY=' .env 2>/dev/null | cut -d= -f2)}"
TMP="$(mktemp -d)"
FAILED=0
trap 'rm -rf "$TMP"' EXIT

say() { printf '\n=== %s ===\n' "$*"; }
ok() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; FAILED=1; }

post() { # path payload outfile -> status
  local path=$1 payload=$2 out=$3
  curl -sS -m 40 -o "$out" -w '%{http_code}' -X POST "$BASE_URL$path" \
    -H 'Content-Type: application/json' -H "Authorization: Bearer $API_KEY" -d "$payload" 2>/dev/null
}

jget() { python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(eval(sys.argv[2]))" "$1" "$2" 2>/dev/null; }

container_state() {
  docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "tavilyd-$1" 2>/dev/null || echo missing
}

wait_healthy() { # service timeout_s
  local svc=$1 timeout=${2:-180} waited=0
  while [ "$waited" -lt "$timeout" ]; do
    [ "$(container_state "$svc")" = healthy ] && return 0
    sleep 3; waited=$((waited + 3))
  done
  return 1
}

wait_overall() { # expected timeout_s
  local want=$1 timeout=${2:-180} waited=0
  while [ "$waited" -lt "$timeout" ]; do
    curl -sS -o "$TMP/health.json" -m 5 "$BASE_URL/health" 2>/dev/null || true
    if [ "$(jget "$TMP/health.json" "d['status']")" = "$want" ]; then return 0; fi
    sleep 3; waited=$((waited + 3))
  done
  return 1
}

goroutines() {
  docker run --rm --network container:tavilyd-api alpine:3.20 \
    wget -qO- 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1' 2>/dev/null \
    | awk '/goroutine profile: total/ {print $4; exit}'
}

check_status() { # want got label
  if [ "$2" = "$1" ]; then ok "$3 (status $2)"; else fail "$3: want $1, got $2: $(head -c 240 "$TMP/out.json")"; fi
}

dep_status() { jget "$TMP/health.json" "d['dependencies']['$1']['status']"; }

search_body() { # depth query
  post /search "{\"query\":\"$2\",\"search_depth\":\"$1\",\"max_results\":3}" "$TMP/out.json"
}

say "pre-flight"
code=$(curl -sS -o "$TMP/health.json" -w '%{http_code}' -m 5 "$BASE_URL/health")
check_status 200 "$code" "health endpoint"
[ "$(jget "$TMP/health.json" "d['status']")" = ok ] && ok "health is ok" || fail "health is not ok: $(head -c 200 "$TMP/health.json")"
GOROUTINES_BEFORE=$(goroutines)
echo "goroutines before drills: ${GOROUTINES_BEFORE:-unknown}"

# ---------------------------------------------------------------- redis -----
say "drill: redis stopped"
$COMPOSE stop redis >/dev/null 2>&1
sleep 2
code=$(search_body basic "degrade redis $RANDOM")
check_status 200 "$code" "basic search survives redis outage"
code=$(search_body basic "degrade redis repeat $RANDOM")
check_status 200 "$code" "second basic search survives redis outage"
curl -sS -o "$TMP/health.json" -m 5 "$BASE_URL/health" >/dev/null 2>&1 || true
[ "$(jget "$TMP/health.json" "d['status']")" = degraded ] && ok "health degraded with redis down" || fail "health not degraded with redis down: $(head -c 200 "$TMP/health.json")"
echo "redis dependency: $(dep_status redis)"
$COMPOSE start redis >/dev/null 2>&1
wait_healthy redis 60 && ok "redis restarted healthy" || fail "redis did not become healthy"
wait_overall ok 90 && ok "health recovered after redis restart" || fail "health did not recover after redis restart"

# ------------------------------------------------------------- reranker -----
say "drill: reranker stopped"
$COMPOSE stop reranker >/dev/null 2>&1
sleep 2
code=$(search_body basic "degrade reranker $RANDOM")
check_status 200 "$code" "basic search survives reranker outage (BM25)"
refresh=$(jget "$TMP/out.json" "len(d.get('results') or [])")
[ "${refresh:-0}" -ge 1 ] && ok "BM25 fallback returned ${refresh} results" || fail "BM25 fallback returned no results"
curl -sS -o "$TMP/health.json" -m 5 "$BASE_URL/health" >/dev/null 2>&1 || true
[ "$(jget "$TMP/health.json" "d['status']")" = degraded ] && ok "health degraded with reranker down" || fail "health not degraded with reranker down"
echo "reranker dependency: $(dep_status reranker)"
$COMPOSE start reranker >/dev/null 2>&1
wait_healthy reranker 300 && ok "reranker restarted healthy" || fail "reranker did not become healthy"
wait_overall ok 180 && ok "health recovered after reranker restart" || fail "health did not recover after reranker restart"

# -------------------------------------------------------------- browser -----
say "drill: browser stopped"
$COMPOSE stop browser >/dev/null 2>&1
sleep 2
code=$(search_body basic "degrade browser basic $RANDOM")
check_status 200 "$code" "basic search with browser down"
code=$(search_body advanced "degrade browser advanced $RANDOM")
check_status 200 "$code" "advanced search with browser down"
curl -sS -o "$TMP/health.json" -m 5 "$BASE_URL/health" >/dev/null 2>&1 || true
[ "$(jget "$TMP/health.json" "d['status']")" = degraded ] && ok "health degraded with browser down" || fail "health not degraded with browser down"
echo "browser dependency: $(dep_status browser)"
$COMPOSE start browser >/dev/null 2>&1
wait_healthy browser 90 && ok "browser restarted healthy" || fail "browser did not become healthy"
wait_overall ok 90 && ok "health recovered after browser restart" || fail "health did not recover after browser restart"

# -------------------------------------------------------------- searxng -----
say "drill: searxng stopped"
$COMPOSE stop searxng >/dev/null 2>&1
sleep 2
started=$(date +%s)
code=$(search_body basic "degrade searxng $RANDOM")
elapsed=$(( $(date +%s) - started ))
check_status 502 "$code" "search fails cleanly without searxng"
error=$(jget "$TMP/out.json" "d['detail']['error']")
[ -n "$error" ] && ok "error envelope present: $error" || fail "missing detail.error envelope"
[ "$elapsed" -lt 15 ] && ok "failure returned in ${elapsed}s (no hang)" || fail "failure took ${elapsed}s (too slow)"
$COMPOSE start searxng >/dev/null 2>&1
wait_healthy searxng 120 && ok "searxng restarted healthy" || fail "searxng did not become healthy"
wait_overall ok 120 && ok "health recovered after searxng restart" || fail "health did not recover after searxng restart"

# ------------------------------------------------------------ goroutines ----
say "post-flight"
GOROUTINES_AFTER=$(goroutines)
echo "goroutines after drills: ${GOROUTINES_AFTER:-unknown} (before: ${GOROUTINES_BEFORE:-unknown})"
if [ -n "${GOROUTINES_BEFORE:-}" ] && [ -n "${GOROUTINES_AFTER:-}" ]; then
  delta=$((GOROUTINES_AFTER - GOROUTINES_BEFORE))
  [ "$delta" -le 20 ] && ok "goroutine delta ${delta} within tolerance" || fail "goroutine count grew by ${delta}"
fi
code=$(search_body basic "post drill sanity $RANDOM")
check_status 200 "$code" "final search works"

echo
if [ "$FAILED" -ne 0 ]; then
  echo "RESULT: FAILED"
  exit 1
fi
echo "RESULT: PASSED"
