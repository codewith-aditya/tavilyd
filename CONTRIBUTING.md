# Contributing to tavilyd

Thanks for taking the time to contribute. This project is a production
Tavily-compatible search API; correctness, latency and graceful degradation
matter more than feature count. Small, measured, well-tested changes are the
easiest to merge.

## Code of conduct

Be respectful and technical. Critique code and evidence, not people. Harassment,
doxxing or publishing someone else's secrets is not acceptable.

## Prerequisites

- Go **1.23+** (the module declares `go 1.23.0`; the Docker builder uses a newer
  toolchain)
- Docker 27+ and Docker Compose v2
- Optional: NVIDIA GPU + NVIDIA Container Toolkit for the reranker; a CPU-only
  host can use `docker-compose.cpu-reranker.yml`
- `make`, `git`, `golangci-lint` (the Makefile can install/use the pinned
  version)

## Local setup

```bash
git clone <repo> tavilyd
cd tavilyd
cp .env.example .env
# generate the two mandatory secrets
sed -i "s|^SEARXNG_SECRET=.*|SEARXNG_SECRET=$(openssl rand -hex 32)|" .env
sed -i "s|^API_KEY=.*|API_KEY=$(openssl rand -hex 24)|" .env
docker compose up -d --build
docker compose ps
```

Only the API publishes a port. Live health:

```bash
curl -sS http://localhost:8080/health | python3 -m json.tool
```

Never commit `.env`, `.proxy.url`, proxy credentials, API keys or full page
bodies. Use `.env.example` for new configuration keys.

## Build, test and lint

```bash
make fmt          # gofmt + goimports
make vet          # go vet ./...
make lint         # golangci-lint run
make test         # go test -race ./...
make build        # static binary in bin/
make docker       # multi-stage image build
```

`go test -race ./...` must pass before a PR. If your change touches extraction,
caching or the search pipeline, also run the contract tests:

```bash
go test -race ./internal/api -run 'Contract|Fuzz|Schema'
```

For a live smoke test:

```bash
KEY=$(grep '^API_KEY=' .env | cut -d= -f2)
curl -sS -X POST http://localhost:8080/search \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $KEY" \
  -d '{"query":"kubernetes readiness probe","max_results":3}' | python3 -m json.tool
```

## What to work on

1. Bugs that break Tavily clients (schema, status code, error envelope).
2. Relevance and extraction quality regressions, with a judged before/after.
3. Latency improvements with stage timings from `X-Timing`.
4. Graceful degradation (Redis/reranker/browser down, engine suspensions).
5. Documentation and reproducible benchmarks.

Open an issue first for large changes, new dependencies, or config surface
changes. Do not add a dependency when the standard library or an existing
dependency is sufficient.

## Project layout

```
cmd/server/          composition root
internal/api/        handlers, middleware, schemas, health, router
internal/search/     SearXNG client, dedupe, domain filtering, engine selection
internal/extract/    uTLS fetcher, browser pool, readability, cleanup, chunking
internal/rerank/     chunker, TEI client, BM25 fallback, scoring
internal/cache/      LRU + Redis + single-flight
internal/security/   auth, rate limiting, SSRF guard
internal/llm/        OpenAI-compatible Qwen client (research/answers)
internal/research/    research jobs, citations, SSE
internal/proxy/      proxy pool, policy, health, relay
internal/observability/ logs, metrics, timings, tracing
reranker-gpu/        PyTorch bge-reranker-v2-m3 sidecar
searxng/             SearXNG settings and entrypoint
scripts/             bench, drills, eval harnesses
bench/               committed, sanitized benchmark/eval output
```

## Adding configuration

All tunables live in `internal/config/config.go` as one typed struct, parsed
from env and validated at startup. A new env var is not done until:

1. the field and loader entry exist;
2. a validation rule exists (range, URL, duration, enum);
3. `.env.example` and `docs/STATE.md` are updated;
4. a config test covers a valid and an invalid value.

**Lesson learned:** a field existed but its loader line was missing, so
`SNIPPET_FALLBACK` silently stayed `false` and an eval ran with the wrong
config. Struct fields alone are not wiring.

## Adding a search engine

1. Verify the engine name exists in the pinned SearXNG version:
   `docker compose exec searxng wget -qO- http://127.0.0.1:8080/config`.
2. Add it to `searxng/settings.yml` only if it is valid for that version; do not
   add guessed names.
3. Add it to `SEARCH_ENGINES` (comma-separated) and test with
   `?engines=<name>&format=json`.
4. Record result count, p50 and suspension behaviour; update
   `BENCHMARKS.md` if it changes the engine mix.

Engines that return `too many requests`/CAPTCHA are expected to be suspended;
the API rotates and cools them down. Do not "fix" a blocked engine by lowering
timeouts without measuring the effect on other engines.

## Benchmarks and evals

For latency or relevance changes include measured before/after numbers, not
estimates:

```bash
python3 scripts/funnel_bench.py                 # 20 queries, diagnostics
python3 scripts/quality_eval_queries.py "$KEY" new
python3 scripts/quality_eval_extended.py "$KEY" new   # 100 queries x 3
```

Quality data must be sanitized: keep query, URL, engine score and judge score;
strip page titles and bodies before committing anything under `bench/`.
Never store third-party page content or Tavily outputs in the repository.

## Pull request checklist

- [ ] `make fmt && make vet && make lint && make test` pass
- [ ] New behavior has tests, including a failure/degradation path
- [ ] No secrets, `.env` files, proxy URLs or page bodies in the diff
- [ ] Config changes update `.env.example` and `docs/STATE.md`
- [ ] Public API changes update `DOCS.md` / README compatibility notes
- [ ] Performance/relevance claims include reproducible measurements
- [ ] Commit messages are focused (Conventional Commits style preferred:
      `fix(search): ...`, `feat(api): ...`, `docs: ...`)

Keep PRs small. A PR that mixes refactoring, a behavior change and new
dependencies is hard to review and will usually be split.

## Reporting bugs

Include:

- request (`curl` with the key redacted), expected vs actual
- `request_id`, `X-Timing` header and relevant `/health` output
- logs with secrets redacted
- whether the cache was bypassed (`?nocache=1`)
- depth, `max_results`, topic and domains used

Security issues: do not open a public issue with exploit details. Contact the
maintainer privately and allow time for a fix.

## License

By contributing you agree that your contribution is licensed under the MIT
License (see [LICENSE](LICENSE)).
