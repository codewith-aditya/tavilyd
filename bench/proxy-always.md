# tavilyd benchmark — proxy-always

Requests: 15  |  generated: 2026-11-22T08:19:21Z

| mode | n | cache p50/p95 (ms) | search p50/p95 (ms) | tier1 p50/p95 (ms) | tier2 p50/p95 (ms) | rerank p50/p95 (ms) | total p50/p95 (ms) | client p50/p95 (ms) |
|---|---|---|---|---|---|---|---|---|
| basic | 5 | 0/0 | 1343/1888 | 2808/12461 | 0/0 | 347/472 | 3333/4090 | 3334/4092 |
| advanced | 5 | 0/0 | 1394/1840 | 2227/4050 | 5294/8065 | 883/1163 | 3923/5948 | 3924/5949 |
| cache | 5 | 0/0 | 0/0 | 0/0 | 0/0 | 0/0 | 0/0 | 1/1 |

Non-200 responses: 1
- advanced `postgres index bloat diagnosis eo96s304` -> 504
