"""TEI-compatible reranker for BAAI/bge-reranker-v2-m3 on CUDA.

Why this exists: TEI's prebuilt CUDA images require compute capability >= 7.0,
so they cannot run on Pascal (sm_61) hardware. This service speaks the same
POST /rerank contract as TEI, so the Go client and its BM25 fallback are
unchanged; only the container behind TEI_URL differs.

Design notes:
  * one process, one GPU worker, one lock: a request queue rather than
    concurrent GPU kernels thrashing VRAM;
  * pairs are sorted by length before batching to minimise padding;
  * dtype is configurable (RERANK_DTYPE=auto|fp16|fp32); see bench_dtypes.py
    and the README for the measured winner on Pascal;
  * /health returns 200 only after the model is loaded and one warmup
    inference has completed.
"""

from __future__ import annotations

import asyncio
import logging
import math
import os
import threading
import time

import torch
from fastapi import FastAPI
from fastapi.responses import JSONResponse
from pydantic import BaseModel
from sentence_transformers import CrossEncoder

logging.basicConfig(level=os.getenv("LOG_LEVEL", "INFO"), format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("reranker")

MODEL_ID = os.getenv("MODEL_ID", "BAAI/bge-reranker-v2-m3")
DTYPE = os.getenv("RERANK_DTYPE", "auto").lower()
BATCH_SIZE = int(os.getenv("RERANK_BATCH_SIZE", "32"))
MAX_LENGTH = int(os.getenv("RERANK_MAX_LENGTH", "512"))
MAX_TEXTS = int(os.getenv("RERANK_MAX_TEXTS", "256"))
DEVICE = "cuda" if torch.cuda.is_available() else "cpu"

if DTYPE not in ("auto", "fp16", "fp32"):
    raise RuntimeError(f"RERANK_DTYPE must be auto|fp16|fp32, got {DTYPE!r}")

if DEVICE == "cuda":
    arch_list = torch.cuda.get_arch_list()
    if "sm_61" not in arch_list:
        raise RuntimeError(
            "this PyTorch build has no sm_61 kernels, cannot run on the Quadro P4000; "
            f"torch.cuda.get_arch_list()={arch_list}"
        )
    cap = torch.cuda.get_device_capability(0)
    log.info("CUDA device: %s (cc %s.%s), torch %s, arch %s",
             torch.cuda.get_device_name(0), cap[0], cap[1], torch.__version__, arch_list)

# Pascal has no fast FP16 tensor path; "auto" resolves to the measured winner,
# which bench_dtypes.py is used to establish. fp16 stays available for Ada/Hopper.
if DEVICE != "cuda":
    TORCH_DTYPE = torch.float32
elif DTYPE == "fp32":
    TORCH_DTYPE = torch.float32
elif DTYPE == "fp16":
    TORCH_DTYPE = torch.float16
else:
    # Measured on the Quadro P4000: fp16 is ~5% faster than fp32 (1319 ms vs
    # 1391 ms for 48 x ~350-token chunks). On Ampere+ the gap is much larger.
    TORCH_DTYPE = torch.float16

log.info("loading %s on %s as %s", MODEL_ID, DEVICE, TORCH_DTYPE)
model = CrossEncoder(
    MODEL_ID,
    max_length=MAX_LENGTH,
    device=DEVICE,
    automodel_args={"torch_dtype": TORCH_DTYPE} if DEVICE == "cuda" else None,
)
model.model.eval()
model.model.to(DEVICE)
if DEVICE == "cuda":
    torch.backends.cudnn.benchmark = True
    torch.set_float32_matmul_precision("high")
    log.info("model loaded on cuda, allocated %.2f GB", torch.cuda.memory_allocated() / 1e9)

_gpu_lock = threading.Lock()
_ready = False
_load_seconds = 0.0


def _score_pairs(pairs: list[tuple[str, str]]) -> list[float]:
    """Return raw logits for (query, text) pairs.

    The forward pass is done manually instead of CrossEncoder.predict because
    sentence-transformers 2.7 applies a sigmoid inside predict and does not
    expose an activation override; TEI's contract needs raw logits for
    raw_scores=true and sigmoid only for the default path.
    """
    queries = [p[0] for p in pairs]
    texts = [p[1] for p in pairs]
    out: list[float] = []
    with _gpu_lock, torch.inference_mode():
        for start in range(0, len(pairs), BATCH_SIZE):
            batch_q = queries[start:start + BATCH_SIZE]
            batch_t = texts[start:start + BATCH_SIZE]
            enc = model.tokenizer(
                batch_q, batch_t,
                padding=True, truncation=True, max_length=MAX_LENGTH,
                return_tensors="pt",
            )
            enc = {k: v.to(DEVICE) for k, v in enc.items()}
            logits = model.model(**enc).logits.view(-1)
            out.extend(logits.float().cpu().tolist())
    return out


def _rerank(query: str, texts: list[str], raw_scores: bool) -> list[dict]:
    pairs = [(query, t) for t in texts]
    order = sorted(range(len(pairs)), key=lambda i: len(pairs[i][1]))
    scored = _score_pairs([pairs[i] for i in order])

    scores = [0.0] * len(pairs)
    for pos, original in enumerate(order):
        scores[original] = scored[pos]

    if not raw_scores:
        scores = [1.0 / (1.0 + math.exp(-s)) if -700 < s < 700 else (0.0 if s <= -700 else 1.0) for s in scores]

    ranked = sorted(range(len(scores)), key=lambda i: scores[i], reverse=True)
    return [{"index": idx, "score": scores[idx]} for idx in ranked]


class RerankRequest(BaseModel):
    query: str
    texts: list[str]
    raw_scores: bool = False
    truncate: bool = True
    return_text: bool = False


app = FastAPI(title="tavilyd-reranker", version="1.0.0")


@app.get("/health")
def health():
    if not _ready:
        return JSONResponse(status_code=503, content={"status": "loading"})
    return {
        "status": "ok",
        "model": MODEL_ID,
        "device": DEVICE,
        "dtype": str(TORCH_DTYPE).replace("torch.", ""),
        "batch_size": BATCH_SIZE,
        "max_length": MAX_LENGTH,
        "load_seconds": round(_load_seconds, 2),
        "gpu": torch.cuda.get_device_name(0) if DEVICE == "cuda" else None,
    }


@app.get("/info")
def info():
    return {
        "model_id": MODEL_ID,
        "model_type": {"reranker": {"id2label": {"0": "LABEL_0"}, "label2id": {"LABEL_0": 0}}},
        "max_input_length": MAX_LENGTH,
        "max_client_batch_size": BATCH_SIZE,
        "auto_truncate": True,
        "version": "1.0.0",
        "device": DEVICE,
        "dtype": str(TORCH_DTYPE).replace("torch.", ""),
    }


@app.post("/rerank")
async def rerank(req: RerankRequest):
    if not req.texts:
        return []
    if len(req.texts) > MAX_TEXTS:
        return JSONResponse(status_code=422, content={"error": f"at most {MAX_TEXTS} texts per request"})
    return await asyncio.to_thread(_rerank, req.query, req.texts, req.raw_scores)


@app.on_event("startup")
def warmup():
    global _ready, _load_seconds
    if _ready:
        return
    started = time.perf_counter()
    _score_pairs([("warmup query", "warmup passage about database indexing")])
    if DEVICE == "cuda":
        torch.cuda.synchronize()
    _load_seconds = time.perf_counter() - started
    _ready = True
    log.info("warmup complete in %.2fs", _load_seconds)
