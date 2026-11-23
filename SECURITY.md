# Security Policy

## Supported versions

| version | supported |
|---|---|
| `v1.x` (`release/v1`) | ✅ security fixes |
| `master` | ❌ experimental Phase 7 work; no security SLA |

Only the latest `v1.x` release receives security fixes. If you run a fork, you
own patching and deployment.

## Reporting a vulnerability

**Do not open a public GitHub issue for a security problem.**

Preferred: open a private GitHub Security Advisory:
https://github.com/codewith-aditya/tavilyd/security/advisories/new

Use the private advisory form; never paste production API keys, proxy
credentials or full page content into a public issue.

Please include:

- affected version/commit (`v1.0.0`, `release/v1`, or a commit SHA)
- endpoint and a minimal request (redact `Authorization` / `x-api-key`)
- expected vs actual behaviour, impact, and a reproduction
- relevant logs with secrets redacted, request id and `X-Timing`
- whether the issue is already public or exploited

## Response targets

| stage | target |
|---|---|
| acknowledgement | 72 hours |
| initial triage / severity | 7 days |
| fix or mitigation for critical/high | 30 days, best effort |
| coordinated disclosure | after a fix is released, by mutual agreement |

These are targets, not a commercial SLA. This is an independent project.

## In scope

- Authentication/authorization bypass, API-key handling, admin endpoints
- SSRF, DNS rebinding, redirect validation, private/metadata IP access
- Injection, path traversal, unsafe deserialization
- Secret leakage in logs, errors, metrics or container images
- Denial of service caused by a specific code defect (not raw volume)
- Container escape or privilege escalation in the shipped Compose stack
- Credit/quota bypass with concrete impact

## Out of scope

- Findings against third-party services (SearXNG, Tavily, Exa, Redis, Postgres,
  NVIDIA, base images) without a demonstrated impact on this project
- Automated scanner output without a reproducible exploit
- Volumetric DDoS, physical access, social engineering, phishing
- Missing best-practice headers with no demonstrated impact
- Testing the hosted production deployment without written permission; use your
  own deployment or a local Compose stack

## Safe harbour

Good-faith research against your own deployment is welcome. Do not access,
modify, exfiltrate or retain other users' data; do not run destructive tests
against shared infrastructure; stop and report if you encounter real user data.
We will not pursue legal action for good-faith research that follows this
policy, and we will credit reporters who want attribution.

## Security design notes

- SSRF guard resolves DNS once, pins the validated IP, rejects private,
  loopback, link-local, CGNAT and cloud-metadata ranges, and re-validates every
  redirect.
- API keys are compared in constant time; optional DB keys are stored as
  HMAC-SHA256 with a server pepper and only a visible prefix (Phase 7).
- Per-key token-bucket rate limits, request body caps and a hard request
  deadline.
- The API runs as nonroot on distroless, read-only root filesystem, all
  capabilities dropped, `no-new-privileges`.
- Proxy credentials are redacted from logs; page bodies and query text are not
  stored in the ledger by default.
- Redis is not the durable system of record when `STORE=postgres`; Postgres
  backups are encrypted/handled by the operator.

## Hardening checklist for operators

1. Set `API_KEY`/`ADMIN_KEY`/`STORE_PEPPER` from a secret manager; never commit
   `.env`.
2. Publish only the API port; keep SearXNG, Redis, Chromium, Postgres and
   metrics internal or on Tailscale.
3. Run behind TLS (Caddy/Traefik/Cloudflare) and restrict `/admin/*` and
   `/metrics`.
4. Keep images pinned and rebuild on security releases; run `govulncheck` and
   image scanning in CI.
5. Back up Postgres and test restores; bound Redis memory.
6. Rotate keys after any suspected exposure; revoke via the admin API/CLI.

## License

Security reports and contributions are handled under the project's MIT license.
