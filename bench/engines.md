# SearXNG engine benchmark (Phase 6)

## Why proxied discovery was fast once and 2.5 s later

20 sequential HTTPS requests through the proxy, curl timings (seconds):

| metric | median | range | note |
|---|---|---|---|
| DNS (proxy host) | 0.002 | 0.001–0.23 | local DNS cache |
| TCP connect to proxy | 0.010 | 0.008–0.24 | fast |
| **TLS handshake to proxy** | **~1.5** | **1.0–5.9** | the whole problem |
| TTFB (CONNECT + target TLS + response) | ~2.1 | 1.4–6.3 | handshake + target |

A single curl process fetching the same URL 5 times: 1.77 s, then 0.60 s,
0.62 s, 0.57 s, 0.59 s — i.e. **connection reuse removes the 1.5 s handshake**.
SearXNG/curl_cffi opened a fresh CONNECT+TLS per engine request, so every
engine paid 1–6 s; with 1.2 s engine timeouts all three engines timed out.
Earlier 550 ms numbers were taken while the proxy handshakes were warm/fast.

## Proxied engine sample (10 queries per engine, PROXY_SEARXNG=on)

| engine | n | ok | unresponsive entries | p50 ms | p95 ms |
|---|---|---|---|---|---|
| google | 10 | 10 | 10 | 613 | 6004 |
| bing | 10 | 10 | 10 | 594 | 925 |
| duckduckgo | 10 | 10 | 10 | 557 | 571 |
| brave | 10 | 10 | 10 | 549 | 646 |

(unresponsive counts include engines that were not queried; the queried engine
also timed out on the slow handshakes.)

## Direct sustained test (300 diverse queries, ~10 min, direct SearXNG)

| category | n | results/query | empty | blocked/timeout | p50 ms | p95 ms |
|---|---|---|---|---|---|---|
| tech | 210 | 20.0 | 0% | 33% | 625 | 721 |
| news | 30 | 19.1 | 0% | 37% | 620 | 759 |
| finance | 25 | 18.4 | 0% | 36% | 639 | 1531 |
| health_howto | 10 | 10.0 | 0% | 70% | 135 | 161 |
| people_company | 10 | 19.4 | 0% | 40% | 620 | 1305 |
| local_india | 10 | 19.7 | 0% | 30% | 605 | 913 |
| hindi | 5 | 10.0 | 0% | 80% | 135 | 136 |

Per engine (direct):

| engine | n | avg results | empty/blocked | p50 ms | p95 ms |
|---|---|---|---|---|---|
| bing | 100 | 19.4 | 0% | 628 | 775 |
| duckduckgo | 100 | 19.1 | 0% | 620 | 735 |
| brave | 100 | 19.2 | 0% | 623 | 758 |

Trend: first 100 requests p50 624 ms / 0% non-200; last 100 p50 613 ms / 0%,
so direct engines did not degrade over the run.

## Decision

- Engines go **direct** by default (google disabled: 6 s p95 hangs).
- `searxng-proxy` is a second instance used only as a fallback when the direct
  instance errors or returns zero results; verified by stopping `searxng`
  (search still returned 200 through the fallback in 5.6 s).
- Proxy handshakes are the cost; a warm-connection forwarder is the next lever,
  not enabled in this phase.
