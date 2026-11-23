#!/usr/bin/env python3
"""Benchmark the tavilyd /search pipeline.

For every query three requests are issued sequentially:
  * cold "basic"    (unique query text, so nothing is cached)
  * cold "advanced" (same unique text, different search_depth -> different cache key)
  * repeat "basic"  (must be served from the cache)

A run nonce is appended to each query so repeated benchmark runs never hit a
warm cache from a previous run (the in-process LRU cannot be flushed remotely).

Outputs <out-dir>/<label>.json (raw) and <out-dir>/<label>.md (summary).
"""

from __future__ import annotations

import argparse
import json
import os
import random
import statistics
import string
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

STAGES = ["cache", "search", "tier1", "tier2", "rerank", "total"]

QUERIES = [
    "golang context cancellation best practices",
    "python asyncio taskgroup vs gather",
    "kubernetes pod disruption budget example",
    "postgres index bloat diagnosis",
    "redis persistence aof vs rdb tradeoffs",
    "docker compose healthcheck depends_on",
    "linux cgroup v2 memory limit",
    "nginx reverse proxy websocket upgrade",
    "typescript decorators metadata reflection",
    "rust async runtime tokio work stealing",
    "sqlite wal mode concurrent readers",
    "prometheus histogram quantiles explained",
    "kafka exactly once semantics transactions",
    "http2 server push deprecated why",
    "websocket load balancing haproxy",
    "react server components streaming",
    "css container queries support",
    "webassembly component model status",
    "open telemetry collector pipeline",
    "terraform state locking dynamodb",
    "aws lambda cold start mitigation",
    "cloudflare workers durable objects",
    "grpc deadline propagation",
    "protobuf field presence optional",
    "sql injection prepared statements",
    "oauth2 pkce flow mobile apps",
    "jwt rotation refresh tokens",
    "bash process substitution examples",
    "git rebase interactive squash",
    "ffmpeg hardware acceleration nvenc",
]


def load_dotenv(path: str = ".env") -> dict[str, str]:
    env: dict[str, str] = {}
    if not os.path.exists(path):
        return env
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, _, value = line.partition("=")
            env[key.strip()] = value.strip().strip('"').strip("'")
    return env


def post(base: str, key: str, path: str, payload: dict, timeout: float = 40.0):
    body = json.dumps(payload).encode()
    headers = {"Content-Type": "application/json"}
    if key:
        headers["Authorization"] = "Bearer " + key
    req = urllib.request.Request(base.rstrip("/") + path, data=body, headers=headers, method="POST")
    started = time.perf_counter()
    status = 0
    raw = b"{}"
    resp_headers: dict[str, str] = {}
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            status = resp.status
            raw = resp.read()
            resp_headers = {k.lower(): v for k, v in resp.headers.items()}
    except urllib.error.HTTPError as exc:
        status = exc.code
        raw = exc.read()
        resp_headers = {k.lower(): v for k, v in exc.headers.items()}
    client_ms = (time.perf_counter() - started) * 1000.0
    try:
        decoded = json.loads(raw.decode() or "{}")
    except json.JSONDecodeError:
        decoded = {}
    timing: dict[str, float] = {}
    for part in resp_headers.get("x-timing", "").split(";"):
        if "=" not in part:
            continue
        name, _, value = part.partition("=")
        try:
            timing[name.strip()] = float(value)
        except ValueError:
            continue
    return {
        "status": status,
        "timing": timing,
        "client_ms": round(client_ms, 2),
        "response_time": decoded.get("response_time"),
        "results": len(decoded.get("results") or []),
        "request_id": decoded.get("request_id"),
    }


def percentile(values: list[float], p: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    k = (len(ordered) - 1) * p / 100.0
    lo = int(k)
    hi = min(lo + 1, len(ordered) - 1)
    return ordered[lo] + (ordered[hi] - ordered[lo]) * (k - lo)


class GPUSampler(threading.Thread):
    """Best-effort nvidia-smi sampler; silently disabled when unavailable."""

    def __init__(self, interval: float = 1.0):
        super().__init__(daemon=True)
        self.interval = interval
        self.samples: list[tuple[float, float]] = []
        self._stop = threading.Event()
        self.available = False
        try:
            subprocess.run(["nvidia-smi", "-L"], capture_output=True, timeout=5, check=True)
            self.available = True
        except Exception:
            pass

    def run(self) -> None:
        while not self._stop.is_set():
            try:
                out = subprocess.run(
                    ["nvidia-smi", "--query-gpu=memory.used,utilization.gpu",
                     "--format=csv,noheader,nounits"],
                    capture_output=True, text=True, timeout=5, check=True,
                ).stdout.strip().splitlines()[0]
                mem, util = [float(x.strip()) for x in out.split(",")]
                self.samples.append((mem, util))
            except Exception:
                pass
            self._stop.wait(self.interval)

    def stop(self) -> dict:
        self._stop.set()
        if not self.samples:
            return {}
        mem = [s[0] for s in self.samples]
        util = [s[1] for s in self.samples]
        return {
            "samples": len(self.samples),
            "vram_used_mb_peak": max(mem),
            "vram_used_mb_mean": round(statistics.fmean(mem), 1),
            "gpu_util_pct_peak": max(util),
            "gpu_util_pct_mean": round(statistics.fmean(util), 1),
        }


def summarize(records: list[dict], label: str, gpu: dict) -> str:
    lines = [f"# tavilyd benchmark — {label}", ""]
    lines.append(f"Requests: {len(records)}  |  generated: {time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())}")
    lines.append("")
    header = "| mode | n | " + " | ".join(f"{s} p50/p95 (ms)" for s in STAGES) + " | client p50/p95 (ms) |"
    lines.append(header)
    lines.append("|" + "---|" * (len(STAGES) + 3))
    for mode in ("basic", "advanced", "cache"):
        rows = [r for r in records if r["mode"] == mode]
        if not rows:
            continue
        cells = []
        for stage in STAGES:
            vals = [r["timing"].get(stage, 0.0) for r in rows]
            cells.append(f"{percentile(vals, 50):.0f}/{percentile(vals, 95):.0f}")
        client = [r["client_ms"] for r in rows]
        lines.append(
            f"| {mode} | {len(rows)} | " + " | ".join(cells)
            + f" | {percentile(client, 50):.0f}/{percentile(client, 95):.0f} |"
        )
    lines.append("")
    errors = [r for r in records if r["status"] != 200]
    lines.append(f"Non-200 responses: {len(errors)}")
    for r in errors[:10]:
        lines.append(f"- {r['mode']} `{r['query']}` -> {r['status']}")
    if gpu:
        lines.append("")
        lines.append(
            "GPU: VRAM peak {vram_used_mb_peak:.0f} MiB (mean {vram_used_mb_mean:.0f}), "
            "util peak {gpu_util_pct_peak:.0f}% (mean {gpu_util_pct_mean:.0f}%), "
            "{samples} samples".format(**gpu)
        )
    lines.append("")
    return "\n".join(lines)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", default=os.environ.get("BENCH_BASE_URL", "http://127.0.0.1:8080"))
    ap.add_argument("--api-key", default=os.environ.get("BENCH_API_KEY", ""))
    ap.add_argument("--label", default="baseline")
    ap.add_argument("--limit", type=int, default=len(QUERIES))
    ap.add_argument("--max-results", type=int, default=5)
    ap.add_argument("--out-dir", default="bench")
    ap.add_argument("--gpu-sample", action="store_true")
    ap.add_argument("--modes", default="basic,advanced,cache", help="comma-separated: basic,advanced,cache")
    args = ap.parse_args()

    key = args.api_key or os.environ.get("API_KEY", "") or load_dotenv().get("API_KEY", "")
    if not key:
        print("warning: no API key (auth may be disabled)", file=sys.stderr)

    nonce = "".join(random.choice(string.ascii_lowercase + string.digits) for _ in range(6))
    queries = QUERIES[: args.limit]
    print(f"benchmarking {len(queries)} queries against {args.base_url} (nonce {nonce})", file=sys.stderr)

    sampler = GPUSampler() if args.gpu_sample else None
    if sampler and sampler.available:
        sampler.start()
    elif sampler:
        print("gpu sampling requested but nvidia-smi is unavailable", file=sys.stderr)

    wanted_modes = {m.strip() for m in args.modes.split(",") if m.strip()}
    records: list[dict] = []
    for idx, query in enumerate(queries, start=1):
        unique = f"{query} {nonce}{idx:02d}"
        basic = {"query": unique, "search_depth": "basic", "max_results": args.max_results}
        advanced = {"query": unique, "search_depth": "advanced", "max_results": args.max_results}

        cold_basic = {"status": 0, "client_ms": 0}
        cold_adv = {"status": 0, "client_ms": 0}
        cached = {"status": 0, "client_ms": 0}
        if "basic" in wanted_modes:
            cold_basic = post(args.base_url, key, "/search", basic)
            records.append({"mode": "basic", "query": unique, **cold_basic})
        if "advanced" in wanted_modes:
            cold_adv = post(args.base_url, key, "/search", advanced)
            records.append({"mode": "advanced", "query": unique, **cold_adv})
        if "cache" in wanted_modes:
            cached = post(args.base_url, key, "/search", basic)
            records.append({"mode": "cache", "query": unique, **cached})

        if idx % 5 == 0 or idx == len(queries):
            print(f"[{idx:02d}/{len(queries)}] done", file=sys.stderr)

    gpu = sampler.stop() if sampler else {}
    os.makedirs(args.out_dir, exist_ok=True)
    payload = {
        "label": args.label,
        "base_url": args.base_url,
        "nonce": nonce,
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "gpu": gpu,
        "records": records,
    }
    json_path = os.path.join(args.out_dir, f"{args.label}.json")
    md_path = os.path.join(args.out_dir, f"{args.label}.md")
    with open(json_path, "w", encoding="utf-8") as fh:
        json.dump(payload, fh, indent=2)
    with open(md_path, "w", encoding="utf-8") as fh:
        fh.write(summarize(records, args.label, gpu))

    print(f"wrote {json_path} and {md_path}", file=sys.stderr)
    print(summarize(records, args.label, gpu))
    return 0 if all(r["status"] == 200 for r in records) else 1


if __name__ == "__main__":
    sys.exit(main())
