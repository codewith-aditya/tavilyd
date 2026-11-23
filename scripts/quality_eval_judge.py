import json
import os
import re
import statistics
import sys
import urllib.request

LLM = os.environ.get("LLM_BASE_URL", "http://127.0.0.1:8000/v1").rstrip("/")
MODEL = os.environ.get("LLM_MODEL", "Qwen3.8-27B-FP8")
LABELS = sys.argv[1:] or ["old", "floor0", "new"]
TMPL_PATH = os.environ.get("JUDGE_TMPL", os.path.join(os.path.dirname(__file__), "judge_relevance.tmpl"))
TMPL = open(TMPL_PATH, encoding="utf-8").read()


def judge(query, results):
    parts = []
    for i, r in enumerate(results):
        parts.append(f"[{i}] title: {r.get('title','')}\nurl: {r.get('url','')}\nsnippet: {r.get('snippet','')[:350]}")
    prompt = TMPL.replace("{QUERY}", query).replace("{RESULTS}", "\n".join(parts))
    body = json.dumps({"model": MODEL, "messages": [{"role": "user", "content": prompt}],
                       "max_tokens": 120, "temperature": 0,
                       "chat_template_kwargs": {"enable_thinking": False}}).encode()
    req = urllib.request.Request(LLM + "/chat/completions", data=body, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=40) as r:
            d = json.loads(r.read())
        text = d["choices"][0]["message"]["content"]
        m = re.search(r"\{.*\}", text, re.S)
        scores = json.loads(m.group(0))["scores"] if m else []
        return [int(x) for x in scores][:5]
    except Exception:
        return []

rows = []
for label in LABELS:
    path = f"/tmp/eval-{label}.json"
    if not os.path.exists(path):
        print("missing", path)
        continue
    data = json.load(open(path))
    p3, p5, junk, counts, lat = [], [], [], [], []
    judged_cases = []
    for item in data["queries"]:
        lat.append(item.get("ms", 0))
        counts.append(item.get("count", 0))
        results = item.get("results", [])
        if not results:
            continue
        scores = judge(item["query"], results)
        if len(scores) < len(results):
            scores = scores + [0] * (len(results) - len(scores))
        rel = [1 if s >= 1 else 0 for s in scores]
        if len(rel) >= 3:
            p3.append(sum(rel[:3]) / 3)
        if rel:
            p5.append(sum(rel) / len(rel))
            junk.append(1 if scores[0] == 0 else 0)
        judged_cases.append((item["query"], results, scores))
    def pctl(a, p):
        a = sorted(a)
        return a[min(len(a) - 1, int(len(a) * p))] if a else 0
    rows.append({"label": label, "n": len(counts), "precision3": statistics.fmean(p3) if p3 else 0,
                 "precision5": statistics.fmean(p5) if p5 else 0,
                 "junk_top5": statistics.fmean(junk) * 100 if junk else 0,
                 "avg_results": statistics.fmean(counts) if counts else 0,
                 "p50": statistics.median(lat) if lat else 0, "p95": pctl(lat, .95)})
    json.dump(judged_cases[:20], open(f"/tmp/eval-{label}-cases.json", "w"), ensure_ascii=False)
    print("judged", label, "cases", len(judged_cases))

print("\n| config | n | precision@3 | precision@5 | junk in top5 | avg results | p50 | p95 |")
print("|---|---|---|---|---|---|---|---|")
for r in rows:
    print(f"| {r['label']} | {r['n']} | {r['precision3']:.2f} | {r['precision5']:.2f} | {r['junk_top5']:.0f}% | {r['avg_results']:.1f} | {r['p50']:.0f} | {r['p95']:.0f} |")
