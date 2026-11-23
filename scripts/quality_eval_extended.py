import json
import os
import re
import statistics
import sys
import time
import urllib.request
import concurrent.futures as cf

base = "http://127.0.0.1:8080"
key = sys.argv[1]
label = sys.argv[2]
LLM = os.environ.get("LLM_BASE_URL", "http://127.0.0.1:8000/v1")
MODEL = os.environ.get("LLM_MODEL", "Qwen3.8-27B-FP8")
TMPL = open("/tmp/judge_relevance.tmpl").read()

queries = [
    "golang context cancellation best practices", "python asyncio taskgroup vs gather",
    "kubernetes readiness probe", "postgres index bloat diagnosis", "redis aof vs rdb",
    "docker compose healthcheck", "linux cgroup v2 memory", "nginx websocket upgrade",
    "react server components", "sqlite wal mode",
    "india union budget 2026 highlights", "latest ai regulation news", "isro launch news",
    "russia ukraine ceasefire", "us election latest", "monsoon forecast india 2026",
    "eu ai act enforcement", "openai latest announcement", "oil prices opec", "japan election",
    "nifty 50 outlook 2026", "rbi repo rate decision", "sip vs lump sum", "best index funds india",
    "gold price forecast", "crypto tax india", "ppf vs nps", "home loan balance transfer",
    "term insurance vs whole life", "sovereign gold bond",
    "how to lower blood pressure naturally", "how to improve sleep hygiene",
    "how to reduce back pain at desk", "how to make paneer at home", "how to start running safely",
    "how to improve posture", "how to treat common cold", "how to manage diabetes diet",
    "how to improve gut health", "how to stretch tight hamstrings",
    "Satya Nadella biography", "Infosys latest earnings", "Tata Motors leadership",
    "Elon Musk companies", "Sam Altman background", "Nvidia company profile",
    "Zomato founder", "Adani group companies",
    "best restaurants in Nanded", "Nanded weather today", "maharashtra government schemes",
    "Pune traffic news", "Mumbai local train timings", "hyderabad tech jobs",
    "nanded municipal election",
    "कृत्रिम बुद्धिमत्ता क्या है", "भारत की अर्थव्यवस्था", "स्वास्थ्य के लिए योग",
    "मौसम पूर्वानुमान", "शिक्षा प्रणाली सुधार",
    "rust async runtime comparison", "typescript decorators guide", "postgres vacuum full",
    "kafka exactly once semantics", "http3 quic explained", "linux io_uring tutorial",
    "wasm component model", "terraform state locking",
    "india election results 2026", "climate summit latest", "stock market crash news",
    "semiconductor policy india", "cyberattack news", "israel gaza ceasefire",
    "sensex forecast next week", "best small cap funds", "nps tax benefits",
    "credit card rewards india", "real estate investment pune", "usd inr forecast",
    "how to cook dal makhani", "how to fix leaking tap", "how to improve focus",
    "how to do pushups correctly", "how to treat acidity", "how to grow tomatoes at home",
    "Ratan Tata biography", "Google CEO", "Microsoft company profile", "Tesla earnings",
    "best cafes in Nanded", "Nanded colleges list", "maharashtra scholarship 2026",
    "Pune metro routes", "Mumbai airport news", "nagpur weather",
    "कैसे पढ़ाई में मन लगाएं", "भारत में नौकरी", "स्वस्थ आहार", "योगासन",
]


def call(q):
    body = json.dumps({"query": q, "search_depth": "basic", "max_results": 10}).encode()
    req = urllib.request.Request(base + "/search?nocache=1", data=body,
                                 headers={"Content-Type": "application/json", "Authorization": "Bearer " + key})
    t = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=45) as r:
            d = json.loads(r.read())
        return {"q": q, "ms": (time.perf_counter() - t) * 1000, "n": len(d.get("results") or []),
                "res": [{"title": x.get("title", ""), "url": x.get("url", ""),
                         "snippet": (x.get("content") or "")[:350], "score": x.get("score")}
                        for x in (d.get("results") or [])[:5]]}
    except Exception as e:
        return {"q": q, "ms": 0, "n": 0, "res": [], "err": str(e)}


def judge(item):
    res = item["res"]
    if not res:
        return []
    parts = ["[%d] title: %s\nurl: %s\nsnippet: %s" % (i, r["title"], r["url"], r["snippet"])
             for i, r in enumerate(res)]
    prompt = TMPL.replace("{QUERY}", item["q"]).replace("{RESULTS}", "\n".join(parts))
    body = json.dumps({"model": MODEL, "messages": [{"role": "user", "content": prompt}],
                       "max_tokens": 120, "temperature": 0,
                       "chat_template_kwargs": {"enable_thinking": False}}).encode()
    req = urllib.request.Request(LLM + "/chat/completions", data=body,
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=45) as r:
            d = json.loads(r.read())
        m = re.search(r"\{.*\}", d["choices"][0]["message"]["content"], re.S)
        return [int(x) for x in json.loads(m.group(0))["scores"]][:5] if m else []
    except Exception:
        return []


def run_pass():
    with cf.ThreadPoolExecutor(max_workers=6) as ex:
        items = list(ex.map(call, queries))
    with cf.ThreadPoolExecutor(max_workers=4) as ex:
        scores = list(ex.map(judge, items))
    for it, sc in zip(items, scores):
        it["judge"] = (sc + [0] * (len(it["res"]) - len(sc)))[:5]
    return items


def stats(items):
    p3, p5, junk, lat, cnt = [], [], [], [], []
    for it in items:
        sc = it["judge"]
        n = len(sc)
        if n:
            p3.append(sum(1 for x in sc[:3] if x >= 1) / min(3, n))
            p5.append(sum(1 for x in sc if x >= 1) / n)
            junk.append(sum(1 for x in sc if x == 0) / n)
        lat.append(it["ms"])
        cnt.append(it["n"])
    return (statistics.fmean(p3) if p3 else 0, statistics.fmean(p5) if p5 else 0,
            statistics.fmean(junk) * 100 if junk else 0,
            statistics.fmean(cnt) if cnt else 0, statistics.median(lat) if lat else 0)


passes = []
for pi in range(3):
    t = time.time()
    items = run_pass()
    passes.append(items)
    print("pass", pi + 1, "done", round(time.time() - t), "s", flush=True)

per = [stats(p) for p in passes]
for i, s in enumerate(per):
    print("pass%d P3=%.3f P5=%.3f junk5=%.1f%% avg=%.1f p50=%.0f" % (i + 1, *s))
mean = [statistics.fmean(x[i] for x in per) for i in range(5)]
spread = [max(x[i] for x in per) - min(x[i] for x in per) for i in range(5)]
print("mean P3=%.3f P5=%.3f junk5=%.1f%% avg=%.1f p50=%.0f" % tuple(mean))
print("spread P3=%.3f P5=%.3f junk5=%.1f avg=%.1f p50=%.0f" % tuple(spread))

agg = {}
for p in passes:
    for it in p:
        sc = it["judge"]
        agg.setdefault(it["q"], []).append(sum(sc) / len(sc) if sc else 0)
worst = sorted(agg.items(), key=lambda kv: statistics.fmean(kv[1]))[:20]
print("--- worst 20 ---")
for q, v in worst:
    print("%.2f %s" % (statistics.fmean(v), q))

json.dump({"label": label, "passes": passes, "per_pass": per, "mean": mean,
           "spread": spread, "worst20": worst},
          open("/tmp/eval-extended-%s.json" % label, "w"), ensure_ascii=False)
