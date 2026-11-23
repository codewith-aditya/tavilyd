import sys
import json
import urllib.request
import statistics
import time

key = sys.argv[1]
base = "http://127.0.0.1:8080"
qs = [
    "nanded municipal corporation election", "how to make paneer at home",
    "best budget smartphone 2026", "infosys q3 results 2026", "maharashtra solar subsidy",
    "how to link aadhaar pan", "nifty bank index today", "who is s jaishankar",
    "yoga for beginners hindi", "sourdough bread recipe", "cybersecurity internship india",
    "electric car charging stations pune", "gst return filing last date",
    "best mutual fund for 5 years", "how to reduce cholesterol", "openai board members",
    "weather in nanded tomorrow", "history of nizamabad", "python list comprehension",
    "aws s3 lifecycle rules",
]
funnel, basic, fast, warm, examples = [], [], [], [], []


def call(q, depth, mr, diag, nocache):
    params = []
    if nocache:
        params.append("nocache=1")
    if diag:
        params.append("diagnostics=1")
    url = base + "/search" + ("?" + "&".join(params) if params else "")
    body = json.dumps({"query": q, "search_depth": depth, "max_results": mr}).encode()
    req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json", "Authorization": "Bearer " + key})
    t = time.perf_counter()
    with urllib.request.urlopen(req, timeout=40) as r:
        d = json.loads(r.read())
    return d, (time.perf_counter() - t) * 1000


def pctl(values, p):
    values = sorted(values)
    return values[min(len(values) - 1, int(len(values) * p))]


for i, q in enumerate(qs):
    unique = q + f" q{i}"
    d, ms = call(unique, "basic", 10, True, True)
    basic.append(ms)
    funnel.append((q, d.get("diagnostics") or {}, len(d.get("results", []))))
    if i < 5:
        examples.append((q, [(r.get("title", "")[:60], r.get("url", "")[:70]) for r in d.get("results", [])[:3]]))
    _, msf = call(unique, "fast", 5, False, True)
    fast.append(msf)
    _, msw = call(unique, "basic", 10, False, False)
    warm.append(msw)

print("| query | cand | ok | snippet | timeout | failed | short | below_floor | final |")
print("|---|---|---|---|---|---|---|---|---|")
for q, dg, n in funnel:
    print("| %s | %s | %s | %s | %s | %s | %s | %s | %d |" % (
        q[:38], dg.get("candidates", 0), dg.get("fetched_ok", 0), dg.get("snippet_only", 0),
        dg.get("timeout", 0), dg.get("failed", 0), dg.get("too_short", 0), dg.get("below_floor", 0), n))

avg_results = statistics.fmean([n for _, _, n in funnel])
print()
print("basic cold p50 %.0fms p95 %.0fms avg_results %.1f" % (statistics.median(basic), pctl(basic, .95), avg_results))
print("fast cold p50 %.0fms p95 %.0fms" % (statistics.median(fast), pctl(fast, .95)))
print("basic warm p50 %.0fms p95 %.0fms" % (statistics.median(warm), pctl(warm, .95)))
print("\nTOP3 EXAMPLES")
for q, tops in examples:
    print("*", q)
    for t, u in tops:
        print("   -", t, "|", u)
