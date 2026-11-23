#!/usr/bin/env python3
"""FP32 vs FP16 benchmark for bge-reranker-v2-m3 on this GPU.

Run inside the reranker container so it sees the same torch build and GPU:

    docker compose run --rm --entrypoint python reranker bench_dtypes.py

48 chunks of roughly 350 tokens, batch size from RERANK_BATCH_SIZE (default 32),
three repetitions per dtype; reports median and best wall time.
"""

from __future__ import annotations

import os
import statistics
import time

import torch
from sentence_transformers import CrossEncoder

MODEL_ID = os.getenv("MODEL_ID", "BAAI/bge-reranker-v2-m3")
BATCH = int(os.getenv("RERANK_BATCH_SIZE", "32"))
MAX_LENGTH = int(os.getenv("RERANK_MAX_LENGTH", "512"))
CHUNKS = int(os.getenv("DTYPE_BENCH_CHUNKS", "48"))
REPS = int(os.getenv("DTYPE_BENCH_REPS", "3"))

if not torch.cuda.is_available():
    raise SystemExit("CUDA is not available in this container")

arch = torch.cuda.get_arch_list()
print(f"torch {torch.__version__} | {torch.cuda.get_device_name(0)} | arch list {arch}")
if "sm_61" not in arch:
    raise SystemExit("torch build lacks sm_61; cannot benchmark the P4000")

# ~350 tokens of English text per chunk.
SENTENCE = (
    "Distributed systems are collections of independent computers that appear to users as a "
    "single coherent system, introducing partial failure, unreliable networks and clock skew. "
)
TEXTS = [(SENTENCE * 3)[:1400] for _ in range(CHUNKS)]
QUERY = "how do distributed systems handle partial failure"


def _predict_logits(model: CrossEncoder, pairs: list[tuple[str, str]], device: str) -> list[float]:
    queries = [p[0] for p in pairs]
    texts = [p[1] for p in pairs]
    out: list[float] = []
    with torch.inference_mode():
        for start in range(0, len(pairs), BATCH):
            enc = model.tokenizer(
                queries[start:start + BATCH], texts[start:start + BATCH],
                padding=True, truncation=True, max_length=MAX_LENGTH, return_tensors="pt",
            )
            enc = {k: v.to(device) for k, v in enc.items()}
            logits = model.model(**enc).logits.view(-1)
            out.extend(logits.float().cpu().tolist())
    return out


def measure(dtype: torch.dtype) -> tuple[float, float]:
    model = CrossEncoder(
        MODEL_ID, max_length=MAX_LENGTH, device="cuda", automodel_args={"torch_dtype": dtype}
    )
    model.model.eval()
    model.model.to("cuda")
    pairs = [(QUERY, t) for t in TEXTS]
    _predict_logits(model, pairs[:8], "cuda")  # warmup
    torch.cuda.synchronize()

    times: list[float] = []
    for _ in range(REPS):
        started = time.perf_counter()
        _predict_logits(model, pairs, "cuda")
        torch.cuda.synchronize()
        times.append(time.perf_counter() - started)
    del model
    torch.cuda.empty_cache()
    return statistics.median(times), min(times)


results = {}
for name, dtype in (("fp32", torch.float32), ("fp16", torch.float16)):
    try:
        median, best = measure(dtype)
        results[name] = (median, best)
        print(f"{name}: median {median*1000:.0f} ms, best {best*1000:.0f} ms "
              f"for {CHUNKS} chunks ({CHUNKS/median:.1f} chunks/s)")
    except Exception as exc:  # fp16 may be unsupported on some kernels
        print(f"{name}: FAILED: {exc}")

if len(results) == 2:
    winner = min(results, key=lambda k: results[k][0])
    print(f"winner: {winner} ({results[winner][0]*1000:.0f} ms median)")
