import json
import sys
import time
import urllib.request

key = sys.argv[1]
label = sys.argv[2]
base = "http://127.0.0.1:8080"
queries = [
    # tech (10)
    "golang context cancellation best practices", "python asyncio taskgroup vs gather",
    "kubernetes readiness probe", "postgres index bloat diagnosis", "redis aof vs rdb",
    "docker compose healthcheck", "linux cgroup v2 memory", "nginx websocket upgrade",
    "react server components", "sqlite wal mode",
    # news (10)
    "india union budget 2026 highlights", "latest ai regulation news", "isro launch news",
    "russia ukraine ceasefire", "us election latest", "monsoon forecast india 2026",
    "eu ai act enforcement", "openai latest announcement", "oil prices opec", "japan election",
    # finance (10)
    "nifty 50 outlook 2026", "rbi repo rate decision", "sip vs lump sum", "best index funds india",
    "gold price forecast", "crypto tax india", "ppf vs nps", "home loan balance transfer",
    "term insurance vs whole life", "sovereign gold bond",
    # how-to (10)
    "how to lower blood pressure naturally", "how to improve sleep hygiene",
    "how to reduce back pain at desk", "how to make paneer at home", "how to start running safely",
    "how to improve posture", "how to treat common cold", "how to manage diabetes diet",
    "how to improve gut health", "how to stretch tight hamstrings",
    # people/company (8)
    "Satya Nadella biography", "Infosys latest earnings", "Tata Motors leadership",
    "Elon Musk companies", "Sam Altman background", "Nvidia company profile",
    "Zomato founder", "Adani group companies",
    # India local (7)
    "best restaurants in Nanded", "Nanded weather today", "maharashtra government schemes",
    "Pune traffic news", "Mumbai local train timings", "hyderabad tech jobs", "nanded municipal election",
    # Hindi (5)
    "कृत्रिम बुद्धिमत्ता क्या है", "भारत की अर्थव्यवस्था", "स्वास्थ्य के लिए योग",
    "मौसम पूर्वानुमान", "शिक्षा प्रणाली सुधार",
]
out = []
for q in queries:
    body = json.dumps({"query": q, "search_depth": "basic", "max_results": 10}).encode()
    req = urllib.request.Request(base + "/search?nocache=1", data=body,
                                 headers={"Content-Type": "application/json", "Authorization": "Bearer " + key})
    t = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=40) as r:
            d = json.loads(r.read())
    except Exception as exc:
        out.append({"query": q, "error": str(exc), "results": [], "ms": 0})
        continue
    ms = (time.perf_counter() - t) * 1000
    results = []
    for item in (d.get("results") or [])[:5]:
        results.append({"title": item.get("title", ""), "url": item.get("url", ""),
                        "snippet": (item.get("content") or "")[:400], "score": item.get("score")})
    out.append({"query": q, "ms": ms, "results": results, "count": len(d.get("results") or [])})

json.dump({"label": label, "queries": out}, open(f"/tmp/eval-{label}.json", "w"), ensure_ascii=False)
print("wrote", f"/tmp/eval-{label}.json", "n", len(out))
