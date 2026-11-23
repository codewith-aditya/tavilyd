# tavilyd — current state

Self-hosted Tavily-compatible search/extract/map/crawl/research API in Go, with a
GPU reranker, a proxied fallback path, a Qwen research endpoint, and an
Exa-compatible `/exa/search` + `/exa/contents` facade. This file is the
single-page operational snapshot; README.md is the product documentation.

## Architecture and services

```
client (Tavily SDK / LangChain / LlamaIndex / curl)
  │  POST /search /extract /map /crawl /research, SSE
  ▼
api (Go, distroless, :8080 published)
  ├── searxng        direct discovery, engines: bing, duckduckgo, brave, mojeek, startpage, yahoo
  ├── searxng-proxy  second SearXNG instance used only when direct discovery fails/empties
  ├── redis          cache (LRU in api + Redis L2), research jobs, usage counters, feedback
  ├── browser        chromedp/headless-shell :9222, via proxyrelay when proxying
  ├── proxyrelay     internal CONNECT relay for Chrome, policy engine, SSRF-checked
  └── reranker       tavilyd-reranker (PyTorch + bge-reranker-v2-m3 FP16, CUDA via CDI)
```

| service | image / build | port | notes |
|---|---|---|---|
| api | Dockerfile (Go static → distroless nonroot) | 8080 | only published port; read-only rootfs, cap_drop ALL |
| searxng | searxng/searxng:2026.11.21-c6ee3e9fd | internal 8080 | JSON API, limiter off; google disabled (6 s p95) |
| searxng-proxy | same image, PROXY_SEARXNG=on | internal 8080 | fallback discovery through the proxy |
| redis | redis:7.2.17-alpine | internal 6379 | maxmemory 4gb, allkeys-lru, AOF |
| browser | chromedp/headless-shell:159.0.8154.13 | internal 9222 | shm 4g; images/fonts/media/trackers blocked at CDP |
| proxyrelay | Dockerfile.relay (Go static) | internal 8888 | no auth, compose network only |
| reranker | reranker-gpu/ (torch 2.1.2+cu121) | internal 80 | TEI-compatible /rerank, FP16, sm_61 |

Exa facade: `/exa/search` maps Exa fields onto the same pipeline (`type`: lite→fast,
auto→basic, deep→advanced; `numResults` capped at 20) and returns Exa's envelope;
`/exa/contents` maps `ids` onto the extractor. Both accept `x-api-key`, return
`costDollars.total=0`, and use an 8 s deadline (Tavily keeps 6 s) so per-result
image extraction can finish.

Data flow for `/search`: auth → rate limit → cache (LRU/Redis, single-flight) →
SearXNG discovery (direct, fallback proxy instance) → hedged tier-1 extraction
(HTTP+uTLS; failed pages fall back to their snippet) → chunk + rerank (TEI, or
BM25 for `fast`) → floor + backfill (pool 2x max_results) → response.

## Current production configuration

| setting | value |
|---|---|
| `SNIPPET_FALLBACK` | `true` |
| `MIN_RESULT_SCORE` | `0.10` |
| `MIN_RELATIVE_SCORE` | `0.05` |
| `SNIPPET_MIN_RELATIVE_SCORE` | `0.02` |
| candidate pool | `2 × max_results` (backfill uses the unused half) |
| `FETCH_BASIC_TIMEOUT` / `_FETCH_CONCURRENCY` | `400ms` / `16` |
| `SEARXNG_TIMEOUT` | `2200ms` |
| proxy | `PROXY_MODE=rotating`, `PROXY_POLICY=direct_first`, `PROXY_TLS_INSECURE=true` |
| SearXNG primary | `SEARXNG_URL=http://searxng-proxy:8080` (proxied discovery) |
| SearXNG fallback | `SEARXNG_PROXY_URL=http://searxng:8080` (direct instance) |
| SearXNG proxying | `PROXY_SEARXNG=on` for the proxy instance |
| engines | bing, duckduckgo, yahoo (2 per query, round-robin, 2 min cooldown); google disabled, brave removed (IP blocked) |
| research | `LLM_BASE_URL=<private inference endpoint>`, model `Qwen3.8-27B-FP8`, create status 200 |

## Environment variables

Parsed and validated at startup into one typed struct; invalid values abort
startup. Defaults from `internal/config/config.go` plus compose-only vars.

| variable | default |
|---|---|
| `ALLOW_PRIVATE_IPS` | `false` |
| `API_KEY` | `nil` |
| `BROWSER_BREAKER_INTERVAL` | `30*s` |
| `BROWSER_BREAKER_MAX_REQUESTS` | `2` |
| `BROWSER_BREAKER_TIMEOUT` | `15*s` |
| `BROWSER_IDLE_TIMEOUT` | `3*s` |
| `BROWSER_MAX_TABS` | `8` |
| `BROWSER_URL` | `"ws://browser:9222"` |
| `CACHE_TTL` | `15*m` |
| `CACHE_VERSION` | `"6"` |
| `CHARS_PER_TOKEN` | `4.0` |
| `CHUNK_OVERLAP` | `50` |
| `CHUNK_TOKENS` | `350` |
| `CLEAN_CTA_PHRASES_FILE` | `""` |
| `CLEAN_ENABLED` | `true` |
| `CLEAN_LINK_DENSITY` | `0.5` |
| `CLEAN_MAX_TOKEN_CHARS` | `2000` |
| `CLEAN_MIN_ALPHA` | `0.4` |
| `EXTRACT_CACHE_TTL` | `24*h` |
| `FETCH_ADVANCED_TIMEOUT` | `4*s` |
| `FETCH_BASIC_TIMEOUT` | `2*s` |
| `FETCH_CONCURRENCY` | `12` |
| `FETCH_MAX_BYTES` | `5<<20` |
| `FETCH_TOTAL_TIMEOUT` | `5*s` |
| `FETCH_USER_AGENT` | `Chrome UA constant` |
| `HTTP_ADDR` | `":8080"` |
| `HTTP_IDLE_TIMEOUT` | `120*s` |
| `HTTP_MAX_BODY_BYTES` | `1<<20` |
| `HTTP_READ_HEADER_TIMEOUT` | `5*s` |
| `HTTP_READ_TIMEOUT` | `10*s` |
| `HTTP_SHUTDOWN_TIMEOUT` | `20*s` |
| `HTTP_WRITE_TIMEOUT` | `30*s` |
| `LLM_API_KEY` | `""` |
| `LLM_BASE_URL` | `""` |
| `LLM_CA_FILE` | `""` |
| `LLM_CONTEXT_TOKENS` | `32768` |
| `LLM_CONNECT_TIMEOUT` | `5s` |
| `LLM_HEALTH_TIMEOUT` | `5s` |
| `LLM_ANSWER_TIMEOUT` | `1.5s` |
| `SEARCH_ENGINES` | `bing,duckduckgo,yahoo` |
| `SEARCH_ENGINES_PER_QUERY` | `2` |
| `SEARCH_ENGINE_COOLDOWN` | `2m` |
| `LLM_MAX_CONCURRENCY` | `2` |
| `LLM_MAX_OUTPUT_TOKENS` | `2048` |
| `LLM_MODEL` | `"Qwen3.8-27B-FP8"` |
| `LLM_THINKING` | `"auto"` |
| `LLM_TIMEOUT` | `60*s` |
| `LOG_LEVEL` | `slog.LevelInfo` |
| `LRU_ENTRIES` | `2000` |
| `MAX_CHUNKS_PER_PAGE` | `24` |
| `MAX_CHUNKS_TOTAL` | `200` |
| `MAX_REDIRECTS` | `5` |
| `METRICS_ADDR` | `"127.0.0.1:9090"` |
| `MIN_RELATIVE_SCORE` | `0.05` |
| `MIN_RESULT_SCORE` | `0.10` |
| `NEGATIVE_CACHE_TTL` | `5*m` |
| `OTEL_ENABLED` | `false` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `""` |
| `OTEL_EXPORTER_OTLP_INSECURE` | `true` |
| `PPROF_ADDR` | `"127.0.0.1:6060"` |
| `PROXY_CONNECT_TIMEOUT` | `2*s` |
| `PROXY_ENDPOINT` | `""` |
| `PROXY_FETCH_TIMEOUT_EXTRA` | `s` |
| `PROXY_HOST_MEMORY_TTL` | `h` |
| `PROXY_HOST_RULES` | `""` |
| `PROXY_LIST_FILE` | `""` |
| `PROXY_MAX_CONNS_PER_PROXY` | `64` |
| `PROXY_MAX_RETRIES` | `1` |
| `PROXY_MODE` | `"off"` |
| `PROXY_POLICY` | `"direct_first"` |
| `PROXY_SEARXNG` | `false` |
| `PROXY_STICKY_TEMPLATE` | `""` |
| `PROXY_TLS_INSECURE` | `false` |
| `CREDIT_LIMIT` | `0` (unlimited; 402 when exceeded) |
| `RATE_LIMIT_BURST` | `20` |
| `RATE_LIMIT_RPS` | `10` |
| `RAW_CONTENT_MAX_CHARS` | `60000` |
| `REDIS_TIMEOUT` | `250*ms` |
| `REDIS_URL` | `"redis://redis:6379/0"` |
| `REQUEST_DEADLINE` | `6*s` |
| `RESEARCH_CREATE_STATUS` | `200` |
| `RESEARCH_DEADLINE_MINI` | `90*s` |
| `RESEARCH_DEADLINE_PRO` | `300*s` |
| `RESEARCH_ENABLED` | `set when LLM_BASE_URL is set` |
| `RESEARCH_MAX_JOBS_PER_KEY` | `2` |
| `RESEARCH_MAX_LLM_CALLS_MINI` | `12` |
| `RESEARCH_MAX_LLM_CALLS_PRO` | `40` |
| `RESEARCH_MAX_PAGES_MINI` | `8` |
| `RESEARCH_MAX_PAGES_PRO` | `20` |
| `RESEARCH_MAX_TOKENS_MINI` | `40000` |
| `RESEARCH_MAX_TOKENS_PRO` | `120000` |
| `RESEARCH_QUEUE` | `16` |
| `RESEARCH_RESULT_TTL` | `24*h` |
| `RESEARCH_WORKERS` | `2` |
| `SEARXNG_BREAKER_INTERVAL` | `30*s` |
| `SEARXNG_BREAKER_MAX_REQUESTS` | `3` |
| `SEARXNG_BREAKER_TIMEOUT` | `10*s` |
| `SEARXNG_CANDIDATE_PAD` | `8` |
| `SEARXNG_MAX_CONNS` | `64` |
| `SEARXNG_PROXY_URL` | `""` |
| `SEARXNG_TIMEOUT` | `2500*ms` |
| `SEARXNG_URL` | `"http://searxng:8080"` |
| `SERVICE_NAME` | `"tavilyd"` |
| `SNIPPET_FALLBACK` | `true` |
| `SNIPPET_MAX_CHARS` | `1500` |
| `SNIPPET_MAX_CHARS_ADVANCED` | `3000` |
| `SNIPPET_MIN_RELATIVE_SCORE` | `0.02` |
| `SSRF_ALLOWLIST` | `nil` |
| `TEI_BREAKER_INTERVAL` | `30*s` |
| `TEI_BREAKER_MAX_REQUESTS` | `5` |
| `TEI_BREAKER_TIMEOUT` | `10*s` |
| `TEI_MAX_BATCH` | `32` |
| `TEI_MODEL` | `"BAAI/bge-reranker-v2-m3"` |
| `TEI_PARALLELISM` | `4` |
| `TEI_TIMEOUT` | `1500*ms` |
| `TEI_URL` | `"http://reranker:80"` |
| `TOKEN_SAFETY_MARGIN` | `1.15` |
| `HF_TOKEN` | `(empty)` |
| `JUDGE_TMPL` | `scripts/judge_relevance.tmpl` |
| `MCP_API_KEY` | `uses API_KEY when set` |
| `MCP_API_URL` | `http://127.0.0.1:8080` |
| `PROXY_SEARXNG_SCHEME` | `http` |
| `PUBLISH_PORT` | `8080` |
| `RERANK_BATCH_SIZE` | `16 (compose)` |
| `RERANK_DTYPE` | `fp16 (compose)` |
| `RERANK_MAX_LENGTH` | `512 (compose)` |
| `SEARXNG_SECRET` | `required by compose (openssl rand -hex 32)` |
| `TAVILYD_VERSION` | `1.0.0` |
| `TAVILY_API_KEY` | `(unset; eval comparison only)` |

## Usage accounting (per key)

- `GET /usage` returns all-time `requests, searches, extracts, maps, crawls,
  research, credits, tokens_in, tokens_out, last_used` plus the key's
  `rate_limit` (`rps/burst/rpm`).
- Counters are stored in Redis under a SHA-256 hash of the key (plaintext keys
  are never used in Redis keys).
- Credits are charged on every request, not only when `include_usage` is set.
- Tokens are estimated at 4 chars/token; they are accounting metadata, not
  tokenizer-exact.
- Rate limit: per-key token bucket, default 10 rps / burst 20 (600 RPM); 429 +
  `Retry-After` on exhaustion.

## Health model (after the probe fix)

- Core dependencies: `redis`, `searxng`, `reranker`, `browser`. `overall=down`
  if any is down; `degraded` if a core probe succeeds but is slow (redis >250 ms,
  network deps >2 s); otherwise `ok`.
- Optional dependencies: `llm`, `proxy`, individual engines. They are listed
  under `optional` and their names under `optional_degraded`; they never flip an
  otherwise healthy service. A suspended engine appears as `engine:<name>`.
- LLM probe: background loop every 30 s, `LLM_HEALTH_TIMEOUT` (5 s), connect
  timeout 5 s, one jittered retry. The snapshot is cached and `/health` never
  probes the LLM. Only 3 consecutive failures set the optional LLM to degraded.
- `include_answer` uses `LLM_ANSWER_TIMEOUT` (1.5 s); on timeout the response is
  still 200 with `answer: null` and `X-Answer-Status: unavailable`.

## Benchmarks (measured, not estimates)

**Cold `basic` with max_results=10, 30 distinct queries (`X-Timing` stages, ms):**

| run | total p50/p95 | search | tier1 sum | tier2 | rerank |
|---|---|---|---|---|---|
| before tuning (proxied discovery) | 2358 / 4325 | 586 / 1303 | 3831 / 15742 | 0 | 447 / 816 |
| direct engines + 600 ms page budget | 1642 / 2270 | 673 / 770 | 1613 / 4488 | 0 | 362 / 476 |
| **relevance-first (current)** | **2503 / 6003** | 673 | 1613 | 0 | 362 |

**Depth targets (20 queries):** `ultra-fast` p50 119 / p95 654 (target met);
`fast` p50 1783 / p95 2348 (target 1.2 s missed); warm cache hit 0.6–1 ms.
Current config trades latency for relevance: basic 2.5 s vs the 1.8 s target.

**60-query quality eval (Qwen judge 0/1/2, `scripts/judge_relevance.tmpl`):**

| config | precision@3 | precision@5 | junk top-1 | avg results | p50 | p95 |
|---|---|---|---|---|---|---|
| old (no snippet fallback, floor 0.10/0.05) | 0.82 | 0.74 | 13% | 4.9 | 2927 | 6007 |
| floor=0 | 0.82 | 0.70 | 12% | 8.3 | 2383 | 6005 |
| **new (snippet TEI + fallback + floors + 2x pool)** | **0.92** | **0.89** | **7%** | 7.1 | 2540 | 3854 |

The first "new" run had `SNIPPET_FALLBACK` silently unwired (field existed but the
loader never read the env var, so it behaved like old with 2x pool: P@5 0.77).
After fixing the loader the same 60 queries scored P@5 0.89 / P@3 0.92, junk 7%,
avg 7.1 results. **Extended eval — 100 queries (60 + 40 new) × 3 cold runs per config:**

| config | P@3 mean (spread) | P@5 mean (spread) | junk top-5 | avg results | p50 |
|---|---|---|---|---|---|
| old | 0.884 (0.041) | 0.758 (0.115) | 24.2% | 5.0 | 3170 |
| floor=0 | **0.988 (0.004)** | **0.969 (0.007)** | **3.1%** | 8.5 | **2019** |
| new (floors 0.10/0.05) | 0.943 (0.041) | 0.902 (0.072) | 9.8% | 6.9 | 2692 |

This reverses the earlier 60-query finding: once snippet fallback is wired and
snippets are cross-encoded, **floor=0 is the quality winner** (P@5 0.969, junk
3.1%, p50 2019 ms) and the floor is what creates backfill junk. Production was
returned to the user-requested floors (0.10/0.05/0.02) after the eval; switching
to floor=0 is a one-env change pending a product decision. Worst-20 lists are in
`bench/quality/extended-*.json` (`worst20`).

**Proxy handshake (20 curl samples):** DNS 2 ms, TCP connect 10 ms, **TLS to
proxy 1.0–5.9 s (median ~1.5 s)**, TTFB ~2.1 s; connection reuse drops a repeat
to 0.6 s. This, not a degraded endpoint, was the proxied-discovery floor.

**Engines (300 diverse queries, direct):** bing 19.4 results/query, p50 628;
duckduckgo 19.1, 620; brave 19.2, 623 (0% empty). Full tables in
`bench/engines.md`.

## Known traps

- **Engine suspension is burst-driven.** After 300 queries at ~1.2 s pacing,
  brave and google-cse reported "Suspended: too many requests" and duckduckgo
  returned CAPTCHA; a query can end up served by bing alone. SearXNG has no
  documented per-engine rate-limit setting in this pinned version (verify
  `search.suspended_times` in the image's default settings.yml before relying on
  it — earlier note says it does not exist). Mitigation today: more engines +
  searxng-proxy fallback + `tavilyd_engine_suspended` metric.
- **Proxy handshakes cost 1–5 s** per new CONNECT+TLS; reuse/keep-alive is
  mandatory for proxy-first discovery. The gateway uses a self-signed TLS cert
  (`PROXY_TLS_INSECURE=true` for the proxy transport only).
- **Pascal GPU (Quadro P4000, cc 6.1)** cannot run TEI's CUDA images. The
  `reranker-gpu` sidecar pins torch 2.1.2+cu121 (last sm_61 line) and FPs16 is
  only ~5% faster than FP32 there.
- **Disk space**: the build host is 48 GB and filled once (Go build cache 5.6 GB
  + docker). Fix: `go clean -cache`, `docker builder prune`; keep ~8 GB free.
- **Research create status**: the official SDK only accepts HTTP 200; the spec
  says 201. Default 200 via `RESEARCH_CREATE_STATUS`.
- **Cache warm-up is single-flight**: a concurrent duplicate of a cold query
  waits for the first computation, so "warm" means a later request.
- **Engine gauges** are per last query; a recovered engine is reset to 0 only on
  the next query that reports it.
- **Exa facade is not Exa**: no neural index, no `summary` synthesis, no
  pagination, `numResults` max 20, `costDollars.total` always 0. Do not publish
  it as the "Exa API" without that caveat.
- **Brave is IP-blocked**: direct "too many requests", proxied "timeout".
  SearXNG suspends it (now 120 s); it was removed from `SEARCH_ENGINES`.
- **Proxy-primary discovery can time out individual engines**: with
  `SEARXNG_URL=searxng-proxy`, a burst gave only duckduckgo (bing/brave/google/
  yahoo: timeout) and 3.4 s wall time for 5 results. The direct instance remains
  the fallback; per-engine timeouts are a P2 item.
- **Silent env wiring bugs**: `SNIPPET_FALLBACK` existed as a struct field but the
  loader never read it, so an eval run silently used the wrong config. Any new
  env var needs a loader test, not just a struct field.

## Pending (ordered)

P1. **Done.** The four named cases are clean under the current config (budget,
isro, monsoon: 5/5 relevant; openai: 4/5). Top-5 junk rate is 11.3% (11 of 60
queries have at least one zero), concentrated in generic company/profile pages,
not stale news. Decision: no date-aware scoring and no stricter absolute floor —
the remaining zero scores 0.476, above any safe floor that would not cut
relevant low-score results.

P2. **Done.** 2 engines per query round-robin (`SEARCH_ENGINES`,
`SEARCH_ENGINES_PER_QUERY`), 2 min cooldown on our side, `search.suspended_times`
verified in the pinned image (AccessDenied/TooManyRequests 180 s, Captcha
3600 s) and shortened to 120/600 s in `searxng/settings.yml`. Brave removed:
direct returns "too many requests", proxied "timeout". Invalid mojeek/startpage
entries removed.

P3. **Code done, targets missed.** Snippet TEI scoring now overlaps page fetches;
the extra pool is fetched lazily (max_results+2 up front, rest on backfill);
fast/ultra-fast use the direct SearXNG instance first. Measured after the
changes: basic p50 3168 ms / p95 5003, fast p50 2112 / p95 4651 (targets 1.8 s /
1.2 s). Search stage alone is 1.0–2.0 s; discovery is the floor.

P4. **Done.** 100 queries × 3 cold runs for old/floor0/new; mean and spread
reported above; worst-20 lists in `bench/quality/extended-*.json`. Floor=0 wins
P@5 (0.969 vs 0.902) and junk (3.1% vs 9.8%) once snippet fallback is wired.
