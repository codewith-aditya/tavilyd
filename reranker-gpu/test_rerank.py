#!/usr/bin/env python3
"""Contract test for the reranker sidecar.

Sends a known query with one clearly relevant and one clearly irrelevant
passage and asserts the relevant one ranks first. Works against any
TEI-compatible endpoint, including TEI itself.

    python3 test_rerank.py --url http://127.0.0.1:8080
"""

from __future__ import annotations

import argparse
import json
import sys
import urllib.request

QUERY = "how does Go handle cancellation of a request"
RELEVANT = (
    "Go passes a context.Context through call chains; cancelling the parent context "
    "propagates a Done channel to every derived context, letting HTTP handlers stop work."
)
IRRELEVANT = "Sourdough bread needs a mature starter, warm water and a long slow fermentation."


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", default="http://127.0.0.1:8080")
    args = ap.parse_args()
    body = json.dumps({"query": QUERY, "texts": [IRRELEVANT, RELEVANT], "truncate": True}).encode()
    req = urllib.request.Request(
        args.url.rstrip("/") + "/rerank", data=body,
        headers={"Content-Type": "application/json"}, method="POST",
    )
    with urllib.request.urlopen(req, timeout=60) as resp:
        scores = json.loads(resp.read())

    if len(scores) != 2:
        print(f"FAIL: expected 2 scores, got {scores}", file=sys.stderr)
        return 1
    by_index = {item["index"]: item["score"] for item in scores}
    relevant, irrelevant = by_index.get(1), by_index.get(0)
    if relevant is None or irrelevant is None:
        print(f"FAIL: missing indices in {scores}", file=sys.stderr)
        return 1
    print(f"relevant={relevant:.4f} irrelevant={irrelevant:.4f}")
    if not relevant > irrelevant:
        print("FAIL: relevant passage did not outscore the irrelevant one", file=sys.stderr)
        return 1
    if not (0.0 <= relevant <= 1.0 and 0.0 <= irrelevant <= 1.0):
        print("FAIL: scores are not normalized to 0..1 (raw_scores=false)", file=sys.stderr)
        return 1
    print("PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
