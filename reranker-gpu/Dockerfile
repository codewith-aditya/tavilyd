# PyTorch 2.1.2 + CUDA 12.1 is the last wheel line that ships sm_61 (Pascal)
# kernels. Do not bump to torch 2.5+ without checking
# torch.cuda.get_arch_list() on the target GPU: app.py fails loudly if sm_61 is
# missing.
FROM pytorch/pytorch:2.1.2-cuda12.1-cudnn8-runtime

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    HF_HOME=/data \
    HF_HUB_ETAG_TIMEOUT=10 \
    HF_HUB_DOWNLOAD_TIMEOUT=120 \
    HF_HUB_DISABLE_TELEMETRY=1 \
    TOKENIZERS_PARALLELISM=false

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py bench_dtypes.py test_rerank.py ./

# Nonroot runtime user, and a /data directory it owns so the named HF cache
# volume inherits the right ownership on first use.
RUN useradd --uid 10001 --create-home --shell /usr/sbin/nologin app \
    && mkdir -p /data /app \
    && chown -R app:app /data /app
USER 10001:10001

EXPOSE 80

HEALTHCHECK --interval=20s --timeout=5s --start-period=300s --retries=10 \
  CMD python -c "import sys,urllib.request; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:80/health', timeout=4).status == 200 else 1)"

CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "80", "--workers", "1", "--log-level", "info"]
