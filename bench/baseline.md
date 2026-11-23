# tavilyd benchmark — baseline

Requests: 90  |  generated: 2026-11-22T06:29:38Z

| mode | n | cache p50/p95 (ms) | search p50/p95 (ms) | tier1 p50/p95 (ms) | tier2 p50/p95 (ms) | rerank p50/p95 (ms) | total p50/p95 (ms) | client p50/p95 (ms) |
|---|---|---|---|---|---|---|---|---|
| basic | 30 | 0/0 | 662/759 | 2282/5465 | 0/0 | 0/826 | 1415/2583 | 1416/2593 |
| advanced | 30 | 0/0 | 624/727 | 1470/5119 | 0/4062 | 0/1 | 1205/4499 | 1207/4501 |
| cache | 30 | 0/0 | 0/0 | 0/0 | 0/0 | 0/0 | 0/0 | 1/1 |

Non-200 responses: 0

GPU: VRAM peak 1406 MiB (mean 1404), util peak 39% (mean 17%), 103 samples
