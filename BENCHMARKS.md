# Benchmarks

All numbers below are measured on the deployment host, not estimates. The host
has 20 cores, 61 GB RAM, a Quadro P4000 8 GB (cc 6.1) and an SSD; the API runs
as a distroless container with Redis 7 and SearXNG on the same bridge network.
Latency is wall-clock from the client, cold (`?nocache=1`) unless stated.

## How to reproduce

```bash
# 20-query cold/warm latency and result counts
python3 scripts/funnel_bench.py

# 60/100-query judged quality eval (needs LLM_BASE_URL)
KEY=$(grep '^API_KEY=*** .env | cut -d= -f2)
python3 scripts/quality_eval_queries.py "$KEY" new
python3 scripts/quality_eval_extended.py "$KEY" new

# staged benchmark harness
make bench
```

## Search latency — current deployment

10 queries, `max_results=10` for basic and `5` for fast/ultra-fast:

| depth | p50 | p95 | avg results |
|---|---|---|---|
| `basic` | **2837 ms** | 4529 ms | 7.6 |
| `fast` | **2062 ms** | 3522 ms | 4.4 |
| `ultra-fast` | **1238 ms** | 1332 ms | 5.0 |
| warm cache | **0.8 ms** | — | — |

Targets were `basic ≤ 1.8 s`, `fast ≤ 1.2 s`, `ultra-fast ≤ 0.5 s`. They are
**missed** on the current deployment; discovery (1–2 s through the proxy
instance) is the floor. Earlier, direct-only discovery reached `basic` p50
1642 ms but with fewer engines and more suspensions.

### Stage breakdown (representative requests)

| query | depth | cache | search | tier 1 | tier 2 | rerank | total |
|---|---|---|---|---|---|---|---|
| kubernetes readiness probe | basic | 0 | 2005 | 3013 | 0 | 440 | 3282 |
| kubernetes readiness probe | fast | 0 | 1054 | 1674 | 0 | 0 | 1514 |

`X-Timing` reports these stages on every response. Tier-1 and search overlap
partially; totals are wall-clock, not a sum of stages.

## Exa facade latency

Three distinct cold queries, `type=auto`, `numResults=3`, `contents.highlights`:

| query | HTTP | time | results |
|---|---|---|---|
| cold … kubernetes | 200 | 4914 ms | 1 |
| cold … golang | 200 | 3433 ms | 1 |
| cold … paneer recipe | 200 | 4831 ms | 3 |

No 504/TIMEOUT. Stage sample: `search=2006ms tier1=1022ms rerank=294ms
total=4725ms`. The facade has an 8 s budget because per-result image extraction
is enabled; Tavily's `/search` keeps the 6 s budget. A warm/cached Exa response
was observed at ~0.2 s. `costDollars.total` is always 0.

## Quality — 60 queries, one run per config

Qwen judge, rubric 0/1/2, top-5 results, prompt in
`scripts/judge_relevance.tmpl`.

| config | P@3 | P@5 | junk top-1 | avg results | p50 |
|---|---|---|---|---|---|
| old (no snippet fallback, floors) | 0.82 | 0.74 | 13% | 4.9 | 2927 |
| floor=0 (old code) | 0.82 | 0.70 | 12% | 8.3 | 2383 |
| new (snippet TEI + fallback + floors) | **0.92** | **0.89** | **7%** | 7.1 | 2540 |

The first "new" run was invalid: `SNIPPET_FALLBACK` was a dead config field, so
the run silently used fallback-off. After wiring it the same queries scored
P@5 0.89. This is why new env vars require loader tests.

## Quality — 100 queries × 3 cold runs (extended)

60 original + 40 new queries across tech, news, finance, how-to, people/company,
India local and Hindi. Mean and spread across three runs:

| config | P@3 mean (spread) | P@5 mean (spread) | junk top-5 | avg results | p50 |
|---|---|---|---|---|---|
| old | 0.884 (0.041) | 0.758 (0.115) | 24.2% | 5.0 | 3170 |
| **floor=0** | **0.988 (0.004)** | **0.969 (0.007)** | **3.1%** | 8.5 | **2019** |
| new (floors 0.10/0.05) | 0.943 (0.041) | 0.902 (0.072) | 9.8% | 6.9 | 2692 |

Finding: once snippets are cross-encoded and the fallback is wired, the floor is
the weaker option — it removes relevant low-score pages and forces snippet
backfill. Floor=0 wins P@5, junk and latency. Production currently runs the
requested floors (0.10/0.05/0.02); switching to floor=0 is an env-only change.

Raw, sanitized per-query data (URLs and scores, no page bodies) is in
`bench/quality/`:

```
old.json, floor0.json, new.json                      # 60 queries
extended-old.json, extended-floor0.json, extended-new.json   # 100 x 3
```

## Reranker (GPU)

48 chunks × ~350 tokens, batch 16, `bge-reranker-v2-m3`:

| dtype | median | best | chunks/s |
|---|---|---|---|
| fp32 | 1391 ms | 1384 ms | 34.5 |
| fp16 | 1319 ms | 1312 ms | 36.4 |

FP16 is only ~5% faster on Pascal (no fast FP16 path). VRAM peak for the service
was **3830 MiB**; steady state ~2.8 GB. A previous direct-discovery run measured
`basic` 1132 ms / `advanced` 2050 ms / cache 1 ms with rerank 338/930 ms — useful
as a reranker cost reference, not as current end-to-end latency.

## Engines

300 diverse queries, direct instances, before the current exclusions:

| engine | results/query | p50 | empty rate |
|---|---|---|---|
| bing | 19.4 | 628 ms | 0% |
| duckduckgo | 19.1 | 620 ms | 0% |
| brave | 19.2 | 623 ms | 0% |

Full per-engine tables: `bench/engines.md`. In the current deployment brave is
IP-blocked (`too many requests` direct, `timeout` via proxy) and is excluded;
rotation uses bing, duckduckgo and yahoo, with google-cse occasionally
contributing. Suspensions are burst-driven; `search.suspended_times` is set to
120 s (access denied / too many requests) and 600 s (CAPTCHA).

## Proxy

Measured with the rotating gateway:

| metric | value |
|---|---|
| DNS | ~2 ms |
| TCP connect | ~10 ms |
| TLS to proxy | 1.0–5.9 s (median ~1.5 s) |
| TTFB | ~2.1 s |
| reused connection | ~0.6 s |
| egress | 200 via `--proxy-insecure` |

The handshake, not the endpoint, is the proxied-discovery floor. Use
`--proxy-insecure` when testing a self-signed proxy; `curl -k` does not apply to
the proxy certificate.

## Cache and storage

| path | measured |
|---|---|
| warm search (LRU) | 0.8–1 ms |
| Redis cache hit | ~1–3 ms |
| extract cache hit | 1.8 ms for 22,620 chars |
| clean stage | p50 < 10 ms, mean ~12.7 ms |

## Resource footprint

| resource | usage |
|---|---|
| API RSS | ~150–250 MB under normal load |
| reranker VRAM | 2.8–3.8 GB |
| Redis | capped at 4 GB (`allkeys-lru`) |
| browser | 4 GB shm, memory-limited |
| disk | model cache + Redis AOF; keep ≥8 GB free for builds |

## Caveats

- The LLM judge is approximate; the prompt is committed so runs are comparable.
- Junk rate is reported for top-5; tail junk beyond position 5 is not scored.
- Proxy and engine availability vary by minute; re-run before quoting numbers.
- Latency numbers are from a single host; they include client and network time.
