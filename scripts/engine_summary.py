import json
import statistics

plan = json.load(open("/tmp/sustained_plan.json"))
rows = []
for line in open("/tmp/sustained/times.csv"):
    parts = line.strip().split(",")
    if len(parts) < 2:
        continue
    try:
        rows.append((float(parts[0]), int(parts[1])))
    except ValueError:
        continue

by = {}
for i, item in enumerate(plan[: len(rows)]):
    latency, code = rows[i]
    try:
        d = json.load(open(f"/tmp/sustained/{i:03d}.json"))
    except Exception:
        continue
    eng = item["engine"]
    b = by.setdefault(eng, {"lat": [], "res": [], "blocked": 0, "n": 0})
    b["n"] += 1
    b["lat"].append(latency * 1000)
    b["res"].append(len(d.get("results") or []))
    if code != 200 or not d.get("results"):
        b["blocked"] += 1

print("| engine | n | avg results | empty/blocked | p50 ms | p95 ms |")
print("|---|---|---|---|---|---|")
for eng, b in by.items():
    lat = sorted(b["lat"])
    p95 = lat[min(len(lat) - 1, int(len(lat) * 0.95))]
    print(
        "| %s | %d | %.1f | %.0f%% | %.0f | %.0f |"
        % (eng, b["n"], statistics.fmean(b["res"]), 100 * b["blocked"] / b["n"], statistics.median(lat), p95)
    )
