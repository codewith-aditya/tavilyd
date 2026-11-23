# Documentation

Start here. This page is the index for the project docs and a compact API
reference; deeper detail lives in the linked files.

| document | what it covers |
|---|---|
| [README.md](README.md) | what the project is, quick start, GPU setup, configuration, Tavily compatibility, troubleshooting |
| [ARCHITECTURE.md](ARCHITECTURE.md) | services, request lifecycle, concurrency, caching, security, design trade-offs |
| [BENCHMARKS.md](BENCHMARKS.md) | measured latency, 60/100-query quality evals, engine/proxy/reranker numbers, reproduction |
| [CONTRIBUTING.md](CONTRIBUTING.md) | development setup, tests, lint, PR checklist, adding config/engines |
| [MCP server](docs/mcp.md) | stdio + HTTP MCP tools, 5 tools, Claude Code setup, working GIF, security |
| [MCP architecture](docs/mcp-architecture.md) | adapter topology, JSON-RPC lifecycle, tool map, failure modes |
| [Search architecture](docs/search-architecture.md) | discovery, extraction, reranking, floors and budgets |
| [GPU architecture](docs/tavilyd-gpu-architecture.md) | GPU reranker sidecar, CDI, FP16, VRAM, fallback |
| [CPU architecture](docs/tavilyd-cpu-architecture.md) | CPU-only override, TEI batch limits, sizing |
| [docs/STATE.md](docs/STATE.md) | live deployment snapshot: every env var with defaults, current production config, known traps, ordered backlog |
| [LICENSE](LICENSE) | MIT |

## Base URL

Local Compose:

```
http://localhost:8080
```

The only published port is the API (`PUBLISH_PORT`, default `8080`). SearXNG,
Redis, Chromium, the relay and the reranker are internal-only.

## Authentication

```http
Authorization: Bearer <API_KEY>
```

`x-api-key: <API_KEY>` is accepted too (Exa clients use it). Legacy `api_key` in
the JSON body is also accepted. If `API_KEY` is unset, authentication is
disabled (a startup warning is logged). Keys are compared in constant time.

## API keys, RPM and credits

### v1: environment-backed keys

The v1 release uses one or more static keys from `.env`:

```bash
# generate a high-entropy key
openssl rand -hex 24

# .env
API_KEY=<key1>
# multiple keys: API_KEY=<key1>,<key2>,<key3>
```

Restart the API after changing `API_KEY`. To rotate without downtime:

1. append the new key: `API_KEY=<old>,<new>`
2. restart, move clients to the new key
3. remove the old key and restart again

Keys are sent as `Authorization: Bearer <key>` or `x-api-key: <key>`. They are
never written to logs. This release does **not** expose a key-creation endpoint;
key management is operator-side.

### How keys work

Every request passes through the auth middleware; the key is compared in
constant time and then used as the identity for its own token bucket and usage
counters. Health endpoints (`/health`, `/livez`, `/readyz`) do not require a
key. A missing/invalid key returns `401`.

### RPM / rate limit

Default per key:

| setting | default | meaning |
|---|---|---|
| `RATE_LIMIT_RPS` | `10` | sustained requests per second |
| `RATE_LIMIT_BURST` | `20` | burst bucket size |
| effective RPM | **600** | 10 rps × 60, sustained |

The bucket refills continuously; a short burst up to 20 is allowed. Exceeding
it returns `429 Too Many Requests` with `Retry-After` (seconds). Limits are
per key, not global: two keys get two independent buckets. Raise
`RATE_LIMIT_RPS`/`RATE_LIMIT_BURST` in `.env` and restart to change them.

### Credits

Credits are charged on **every** request (cache hits included) regardless of
`include_usage`; `include_usage: true` only adds the `usage` object to the
response.

| operation | credits |
|---|---|
| search `basic` / `fast` / `ultra-fast` | 1 |
| search `advanced` | 2 |
| extract `basic` | ceil(successful URLs / 5) |
| extract `advanced` | 2 × ceil(successful URLs / 5) |
| map / crawl | ceil(pages / 10) |
| research `mini` | 4 |
| research `pro` | 15 |

Failures are not charged for extract (only successful URLs count). Research
queues/jobs have their own per-key job cap (`RESEARCH_MAX_JOBS_PER_KEY`).

### Credit quota

`CREDIT_LIMIT` is an all-time per-key cap; `0` (default) means unlimited.

```bash
CREDIT_LIMIT=1000
```

A request that would cross the cap is rejected **before** search/extract work
with `432` (plan-limit semantics) and:

```
X-Credits-Limit: 1000
X-Credits-Used: 998
X-Credits-Remaining: 2
```

`429` is only for the per-second rate limit; `432` is for exhausted credits.
If Redis is unavailable the quota check fails open, so a cache outage cannot
take the API down.

### Per-key usage

```bash
curl -sS http://localhost:8080/usage -H "Authorization: Bearer $KEY" | jq
```

Returns `requests`, `searches`, `extracts`, `maps`, `crawls`, `research`,
`credits`, `tokens_in`, `tokens_out`, `last_used`, `rate_limit` and (when a
limit is configured) `credit_limit` / `credits_remaining`. Token counts are a
4-characters-per-token estimate for accounting, not tokenizer-exact.

## `/search`

```bash
KEY=$(grep '^API_KEY=' .env | cut -d= -f2)

curl -sS -X POST http://localhost:8080/search \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $KEY" \
  -d '{
    "query": "kubernetes readiness probe",
    "search_depth": "basic",
    "max_results": 5,
    "topic": "general",
    "include_answer": true,
    "include_raw_content": false
  }' | python3 -m json.tool
```

Key fields:

| field | values / default |
|---|---|
| `query` | required string |
| `search_depth` | `basic` (default), `advanced`, `fast`, `ultra-fast` |
| `max_results` | 0–20, default 10 |
| `topic` | `general`, `news`, `finance` |
| `time_range` | `day|week|month|year`, `d|w|m|y` |
| `include_domains` / `exclude_domains` | up to 300 / 150 |
| `include_answer` | bool or `basic`/`advanced`; 1.5 s cap, `null` on timeout |
| `include_raw_content` | bool or `markdown`/`text` |
| `include_images` | supported; per-result `<img>` URLs plus top-level image results |
| `chunks_per_source` | 1–3, default 3 |

Response shape:

```json
{
  "query": "kubernetes readiness probe",
  "answer": null,
  "images": [],
  "results": [
    {"title": "...", "url": "...", "content": "...", "score": 0.94, "raw_content": null}
  ],
  "response_time": 1.12,
  "request_id": "uuid"
}
```

Errors use the Tavily envelope: `{"detail": {"error": "..."}}` with `401`,
`422`, `429`, `502` or `504`. Unknown request fields are ignored.

## Exa-compatible facade

`/exa/search` and `/exa/contents` accept Exa's request shapes and return Exa's
response envelope on top of the local pipeline. Auth accepts `x-api-key` or
`Authorization: Bearer`. Use `base_url="http://localhost:8080/exa"` with the Exa
SDK (replace with your deployment URL), or call the paths directly. This is our index, not Exa's paid neural API:
`costDollars.total` is always 0, and `summary` is accepted but not synthesized.

```bash
curl -sS -X POST http://localhost:8080/exa/search \
  -H 'Content-Type: application/json' \
  -H "x-api-key: <API_KEY>" \
  -d '{"query":"kubernetes readiness probe","type":"auto","numResults":3,"contents":{"highlights":true}}'
```

```bash
curl -sS -X POST http://localhost:8080/exa/contents \
  -H 'Content-Type: application/json' \
  -H "x-api-key: <API_KEY>" \
  -d '{"ids":["https://en.wikipedia.org/wiki/Kubernetes"],"text":true,"highlights":true}'
```

- `numResults` is clamped to the local maximum of 20; no pagination.
- Search types: `lite` → fast, `deep`/`deep-lite` → advanced, otherwise basic.
- `/exa/search` gets an 8 s deadline (per-result images stay enabled); Tavily
  endpoints keep the 6 s budget.
- `type` maps to local depth: `lite` → fast, `deep`/`deep-lite` → advanced,
  otherwise basic.
- `costDollars.total` is always 0: this is our index, not Exa's paid API.
- `summary` is accepted but not synthesized; `text`/`highlights` are returned
  when requested. `contents` failures appear under `statuses`.

## `/extract`

```bash
curl -sS -X POST http://localhost:8080/extract \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $KEY" \
  -d '{"urls":["https://kubernetes.io/docs/concepts/workloads/pods/probes/"]}' \
  | python3 -m json.tool
```

`urls` accepts a string or list (max 20), `extract_depth` is `basic|advanced`,
`format` is `markdown|text`, and `query`/`chunks_per_source` rerank chunks.
Response: `results[]` with `url`, `raw_content`, `images`, `failed_results[]`,
`response_time`, `request_id`.

## `/research`

```bash
curl -sS -X POST http://localhost:8080/research \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $KEY" \
  -d '{"query":"state of open-source LLM inference","model":"mini"}' | python3 -m json.tool
```

Create returns `202` (job id) when queued; poll `GET /research/{id}` until 200.
Progress is available as SSE with `text/event-stream`. Requires `LLM_BASE_URL`
and `RESEARCH_ENABLED`; results are stored in Redis for 24 h.

## Usage and credits

`GET /usage` returns per-key counters/credits. Counters increment for every
authenticated API request (including `/usage` itself); credits are charged
regardless of `include_usage`:

| operation | credits |
|---|---|
| search basic/fast/ultra-fast | 1 |
| search advanced | 2 |
| extract | ceil(successful URLs / 5), ×2 advanced |
| map/crawl | ceil(pages / 10) |
| research mini | 4 |
| research pro | 15 |

`tokens_in`/`tokens_out` are a 4-characters-per-token **estimate** for
accounting, not tokenizer-exact. `rate_limit` reports the per-key token bucket
(`rps`, `burst`, `rpm`). `include_usage: true` still adds the Tavily-style
`usage` object to the response; accounting happens either way.

`CREDIT_LIMIT` (default 0 = unlimited) is an all-time per-key cap. A request
that would cross it is rejected **before** search/extract work with `402 Payment
Required` and `X-Credits-Limit`, `X-Credits-Used`, `X-Credits-Remaining`
headers. `429` remains the per-second rate limit. The check fails open if Redis
is unavailable so a cache outage cannot take the API down.

## Health model

```bash
curl -sS http://localhost:8080/health | python3 -m json.tool
```

- **core**: `redis`, `searxng`, `reranker`, `browser` — decide `ok` / `degraded`
  / `down`. A slow core probe (redis >250 ms, network deps >2 s) is degraded.
- **optional**: `llm`, `proxy`, individual `engine:<name>` — listed under
  `optional` and `optional_degraded`; they do not flip overall status when the
  core is healthy.
- The LLM probe runs in the background (30 s interval, 5 s timeout, retry once);
  three consecutive failures mark the optional LLM degraded. `/health` never
  probes the LLM inline.

## SDKs and framework clients

Any Tavily-compatible client works by changing its base URL.

**Python (`tavily-python`)**

```python
from tavily import TavilyClient

client = TavilyClient(api_key="<API_KEY>", base_url="http://localhost:8080")
res = client.search("kubernetes readiness probe", max_results=5)
```

**LangChain / LlamaIndex** — point the Tavily integration at the same base URL
(or the equivalent environment variable your integration exposes) and keep the
rest of the configuration unchanged. The API accepts Tavily's documented fields
and returns Tavily's response envelope.

**h2c** — the server supports HTTP/2 cleartext (prior knowledge). Plain HTTP/1.1
works for all clients.

## Deployment

```bash
cp .env.example .env
# set SEARXNG_SECRET and API_KEY
docker compose up -d --build
docker compose ps
```

GPU reranking needs the NVIDIA Container Toolkit and the CDI device request used
by the `reranker` service. For a CPU-only host, use
`docker-compose.cpu-reranker.yml` (slower, but keeps the API contract). Details
and troubleshooting are in the README.

## Compatibility notes

Tavily parity is documented in the README under **Compatibility notes**. In
short: all documented request fields are accepted, unknown fields are ignored,
`include_images` is best-effort (per-result `<img>` URLs plus top-level image
results); approximated features degrade rather than erroring. The Exa facade
returns `costDollars.total=0` and caps `numResults` at 20. Contract tests validate responses against Tavily's OpenAPI schema in
`internal/api/contract_test.go`.

## Contributing and license

See [CONTRIBUTING.md](CONTRIBUTING.md). MIT — see [LICENSE](LICENSE).
