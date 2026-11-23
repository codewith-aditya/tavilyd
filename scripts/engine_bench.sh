#!/usr/bin/env bash
# Per-engine SearXNG benchmark: 10 queries per engine, JSON + wall time.
set -uo pipefail
OUT=${1:-/tmp/engines}
mkdir -p "$OUT"
: > "$OUT/times.csv"
chmod 777 "$OUT"
chmod 666 "$OUT/times.csv"
QUERIES=(
  "golang context cancellation" "python asyncio taskgroup" "kubernetes readiness probe"
  "postgres index bloat" "redis persistence aof rdb" "docker compose healthcheck"
  "linux cgroup v2 memory" "nginx websocket upgrade" "react server components" "sqlite wal mode"
)
INNER=""
for eng in google bing duckduckgo brave; do
  i=0
  for q in "${QUERIES[@]}"; do
    i=$((i+1))
    enc=$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "$q")
    echo "curl -sS -m 12 -o $OUT/${eng}_${i}.json -w '%{time_total},%{http_code}' 'http://searxng:8080/search?q=${enc}&format=json&categories=general&engines=${eng}' >> $OUT/times.csv; echo >> $OUT/times.csv"
  done
done > /tmp/engine_cmds.sh
docker run --rm --network tavilyd_backend -v /tmp:/tmp -v /tmp/engine_cmds.sh:/cmds.sh curlimages/curl:8.10.1 sh -c "sh /cmds.sh"
python3 - "$OUT" <<'PY'
import json, os, sys, statistics
out = sys.argv[1]
times = {}
for line in open(os.path.join(out, "times.csv")):
    parts = line.strip().split(",")
    if len(parts) != 3:
        continue
    t, code = float(parts[0]), parts[1]
    # file order: engines outer, queries inner -> reconstruct from count
    times.setdefault("all", []).append((t, code))
rows = []
for eng in ["google", "bing", "duckduckgo", "brave"]:
    lat, ok, empty, unresp = [], 0, 0, 0
    for i in range(1, 11):
        p = os.path.join(out, f"{eng}_{i}.json")
        if not os.path.exists(p):
            continue
        try:
            d = json.load(open(p))
        except Exception:
            continue
        n = len(d.get("results") or [])
        if n:
            ok += 1
        else:
            empty += 1
        if d.get("unresponsive_engines"):
            unresp += 1
    # times.csv lines are in engine order; recompute offsets by scanning
    rows.append((eng, ok, empty, unresp))
# times: global list is ordered google(10),bing(10),ddg(10),brave(10)
all_times = times.get("all", [])
print("| engine | n | ok | empty | unresponsive | p50 ms | p95 ms |")
print("|---|---|---|---|---|---|---|")
for idx, (eng, ok, empty, unresp) in enumerate(rows):
    lat = [t * 1000 for t, _ in all_times[idx*10:(idx+1)*10]]
    if lat:
        p50 = statistics.median(lat)
        p95 = sorted(lat)[min(len(lat)-1, int(len(lat)*0.95))]
        print(f"| {eng} | {len(lat)} | {ok} | {empty} | {unresp} | {p50:.0f} | {p95:.0f} |")
PY
