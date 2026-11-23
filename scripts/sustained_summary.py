#!/usr/bin/env python3
"""Summarise the sustained SearXNG benchmark per query category."""
import json
import os
import statistics
import sys

dir_path = sys.argv[1] if len(sys.argv) > 1 else "/tmp/sustained"
plan = json.load(open("/tmp/sustained_plan.json"))
rows = []
for line in open(os.path.join(dir_path, "times.csv")):
    parts = line.strip().split(",")
    if len(parts) < 2:
        continue
    try:
        rows.append((float(parts[0]), int(parts[1])))
    except ValueError:
        continue

by_cat = {}
for idx, item in enumerate(plan):
    if idx >= len(rows):
        break
    latency, code = rows[idx]
    path = os.path.join(dir_path, f"{idx:03d}.json")
    results = 0
    blocked = code != 200
    empty = False
    engines = set()
    try:
        d = json.load(open(path))
        results = len(d.get("results") or [])
        empty = results == 0
        for r in d.get("results") or []:
            e = r.get("engine")
            if e:
                engines.add(e)
        queried = item.get("engine", "").lower()
        for u in d.get("unresponsive_engines") or []:
            name = str(u[0]).lower() if u else ""
            reason = " ".join(str(x) for x in u).lower()
            if queried and name != queried:
                continue  # engines we did not ask for are always "unresponsive"
            if "captcha" in reason or "too many" in reason or "rate" in reason or "timeout" in reason:
                blocked = True
    except Exception:
        empty = True
        blocked = True
    rec = by_cat.setdefault(item["cat"], {"lat": [], "results": [], "empty": 0, "blocked": 0, "engines": set(), "n": 0})
    rec["n"] += 1
    rec["lat"].append(latency * 1000)
    rec["results"].append(results)
    rec["empty"] += 1 if empty else 0
    rec["blocked"] += 1 if blocked else 0
    rec["engines"] |= engines

print("| category | n | results/query | empty rate | blocked/timeout rate | p50 ms | p95 ms | engines |")
print("|---|---|---|---|---|---|---|---|")
for cat, r in by_cat.items():
    lat = sorted(r["lat"])
    p50 = statistics.median(lat) if lat else 0
    p95 = lat[min(len(lat) - 1, int(len(lat) * 0.95))] if lat else 0
    avg = statistics.fmean(r["results"]) if r["results"] else 0
    print(f"| {cat} | {r['n']} | {avg:.1f} | {100*r['empty']/r['n']:.0f}% | {100*r['blocked']/r['n']:.0f}% | {p50:.0f} | {p95:.0f} | {len(r['engines'])} |")

# Trend: first vs last 100 requests
def window(a, b):
    lat = [x[0] * 1000 for x in rows[a:b]]
    return (statistics.median(lat) if lat else 0, sum(1 for x in rows[a:b] if x[1] != 200) / max(1, b - a))
first = window(0, min(100, len(rows)))
last = window(max(0, len(rows) - 100), len(rows))
print(f"\nfirst-100: p50 {first[0]:.0f} ms, non-200 {first[1]*100:.0f}%")
print(f"last-100:  p50 {last[0]:.0f} ms, non-200 {last[1]*100:.0f}%")
