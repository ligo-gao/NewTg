#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Gateway chat-completions concurrency and latency test (stdlib only).

Sends `--concurrency` requests at the same time (a barrier synchronises the
start), then reports status counts, latency percentiles, throughput and token
throughput. The API key is read from `GATEWAY_API_KEY` (or `--key`); it is
never printed or written to disk.

Examples:
    export GATEWAY_API_KEY=nkt-...
    python3 scripts/loadtest_concurrency.py --concurrency 500

    # Streaming: measures TTFT as well as total latency.
    python3 scripts/loadtest_concurrency.py --concurrency 50 --stream

    # Another deployment / model.
    python3 scripts/loadtest_concurrency.py \
        --url https://10.211.55.5:8443/v1/chat/completions \
        --model deepseek-flash --max-tokens 8
"""

from __future__ import annotations

import argparse
import json
import os
import re
import ssl
import statistics
import sys
import threading
import time
import urllib.error
import urllib.request
from collections import Counter

DEFAULT_URL = "https://10.211.55.5:8443/v1/chat/completions"
DEFAULT_MODEL = "deepseek-flash"
DEFAULT_PROMPT = "ping"


def now() -> float:
    return time.perf_counter()


def percentile(sorted_values: list[float], p: float) -> float:
    """Linear-interpolated percentile on an ascending list."""
    if not sorted_values:
        return 0.0
    if len(sorted_values) == 1:
        return sorted_values[0]
    rank = (len(sorted_values) - 1) * (p / 100.0)
    low = int(rank)
    high = min(low + 1, len(sorted_values) - 1)
    fraction = rank - low
    return sorted_values[low] + (sorted_values[high] - sorted_values[low]) * fraction


def ssl_context(verify: bool) -> ssl.SSLContext:
    if verify:
        return ssl.create_default_context()
    context = ssl.create_default_context()
    context.check_hostname = False
    context.verify_mode = ssl.CERT_NONE
    return context


def read_stream(response) -> tuple[float | None, int, dict | None, int]:
    """Consume an SSE response. Returns (ttft, data_frames, usage, text_length)."""
    started = now()
    ttft = None
    frames = 0
    usage = None
    text_length = 0
    while True:
        line = response.readline()
        if not line:
            break
        text = line.decode("utf-8", "replace").strip()
        if not text.startswith("data:"):
            continue
        payload = text[5:].strip()
        if payload == "[DONE]":
            break
        try:
            frame = json.loads(payload)
        except json.JSONDecodeError:
            continue
        frames += 1
        if ttft is None:
            choices = frame.get("choices") or []
            if choices:
                delta = (choices[0] or {}).get("delta") or {}
                if delta.get("content") or delta.get("reasoning_content"):
                    ttft = now() - started
        if isinstance(frame.get("usage"), dict):
            usage = frame["usage"]
        for choice in frame.get("choices") or []:
            delta = (choice or {}).get("delta") or {}
            text_length += len(delta.get("content") or "")
    return ttft, frames, usage, text_length


def one_request(
    url: str,
    key: str,
    model: str,
    prompt: str,
    max_tokens: int,
    timeout: float,
    stream: bool,
    context: ssl.SSLContext,
) -> dict:
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "max_tokens": max_tokens,
        "stream": stream,
    }
    if stream:
        payload["stream_options"] = {"include_usage": True}
    data = json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(
        url,
        data=data,
        method="POST",
        headers={
            "Authorization": f"Bearer {key}",
            "Content-Type": "application/json",
            "Accept": "text/event-stream" if stream else "application/json",
        },
    )
    started = now()
    try:
        with urllib.request.urlopen(request, timeout=timeout, context=context) as response:
            status = response.status
            if stream:
                ttft, frames, usage, text_length = read_stream(response)
                elapsed = now() - started
                return {
                    "ok": 200 <= status < 300,
                    "status": status,
                    "latency": elapsed,
                    "ttft": ttft,
                    "frames": frames,
                    "text_length": text_length,
                    "usage": usage,
                    "error": None,
                }
            body = response.read()
            elapsed = now() - started
            usage = None
            if body:
                try:
                    parsed = json.loads(body.decode("utf-8"))
                    if isinstance(parsed.get("usage"), dict):
                        usage = parsed["usage"]
                except (json.JSONDecodeError, UnicodeDecodeError):
                    pass
            return {
                "ok": 200 <= status < 300,
                "status": status,
                "latency": elapsed,
                "ttft": None,
                "frames": 0,
                "text_length": 0,
                "usage": usage,
                "error": None,
            }
    except urllib.error.HTTPError as error:
        elapsed = now() - started
        detail = ""
        try:
            detail = error.read().decode("utf-8", "replace")[:300]
        except Exception:
            pass
        return {
            "ok": False,
            "status": error.code,
            "latency": elapsed,
            "ttft": None,
            "frames": 0,
            "text_length": 0,
            "usage": None,
            "error": detail or str(error.reason),
        }
    except Exception as error:  # URLError, timeout, TLS, connection reset
        elapsed = now() - started
        return {
            "ok": False,
            "status": None,
            "latency": elapsed,
            "ttft": None,
            "frames": 0,
            "text_length": 0,
            "usage": None,
            "error": f"{type(error).__name__}: {error}",
        }


def fmt_seconds(value: float | None) -> str:
    if value is None:
        return "—"
    if value >= 10:
        return f"{value:.1f}s"
    if value >= 1:
        return f"{value:.2f}s"
    return f"{value * 1000:.0f}ms"


def usage_total(usage: dict | None, *keys: str) -> int:
    if not isinstance(usage, dict):
        return 0
    total = 0
    for key in keys:
        value = usage.get(key)
        if isinstance(value, (int, float)):
            total += int(value)
    return total


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--url", default=DEFAULT_URL, help=f"chat completions endpoint (default: {DEFAULT_URL})")
    parser.add_argument("--key", default=os.environ.get("GATEWAY_API_KEY", ""), help="API key; prefer GATEWAY_API_KEY")
    parser.add_argument("--model", default=DEFAULT_MODEL)
    parser.add_argument("--concurrency", type=int, default=500)
    parser.add_argument("--requests", type=int, default=None, help="default: same as --concurrency")
    parser.add_argument("--max-tokens", type=int, default=16)
    parser.add_argument("--prompt", default=DEFAULT_PROMPT)
    parser.add_argument("--timeout", type=float, default=120.0)
    parser.add_argument("--stream", action="store_true", help="stream responses and measure TTFT")
    parser.add_argument("--verify", action="store_true", help="verify TLS (off by default for the internal CA)")
    parser.add_argument("--json-out", default="", help="optional path for the raw per-request results")
    parser.add_argument("--skip-warmup", action="store_true")
    args = parser.parse_args()

    if not args.key:
        print("error: API key missing; set GATEWAY_API_KEY or pass --key", file=sys.stderr)
        return 2
    if args.concurrency < 1:
        print("error: --concurrency must be >= 1", file=sys.stderr)
        return 2

    requests_count = args.requests if args.requests is not None else args.concurrency
    if requests_count < 1:
        print("error: --requests must be >= 1", file=sys.stderr)
        return 2
    context = ssl_context(args.verify)

    print(f"gateway : {args.url}")
    print(f"model   : {args.model}")
    print(
        f"plan    : concurrency={args.concurrency} requests={requests_count} "
        f"max_tokens={args.max_tokens} stream={'yes' if args.stream else 'no'}"
    )

    if not args.skip_warmup:
        warm = one_request(
            args.url, args.key, args.model, args.prompt,
            min(args.max_tokens, 8), args.timeout, args.stream, context,
        )
        status = warm["status"] if warm["status"] is not None else warm["error"]
        print(f"warm-up : {status} in {fmt_seconds(warm['latency'])}")
        if not warm["ok"]:
            print(f"warm-up failed: {warm['error']}", file=sys.stderr)
            print("aborting before the burst; pass --skip-warmup to force", file=sys.stderr)
            return 3

    # Synchronise the start: every worker waits at the barrier, then fires.
    barrier = threading.Barrier(min(args.concurrency, requests_count)) if requests_count >= args.concurrency else None
    results: list[dict] = []
    results_lock = threading.Lock()

    def worker() -> None:
        if barrier is not None:
            try:
                barrier.wait(timeout=args.timeout)
            except threading.BrokenBarrierError:
                pass
        result = one_request(
            args.url, args.key, args.model, args.prompt,
            args.max_tokens, args.timeout, args.stream, context,
        )
        with results_lock:
            results.append(result)

    wall_started = now()
    threads = [threading.Thread(target=worker, daemon=True) for _ in range(requests_count)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    wall = now() - wall_started

    ok_results = [item for item in results if item["ok"]]
    failed_results = [item for item in results if not item["ok"]]
    # 只把成功的请求计入模型时延；429 等拒绝是“多快被挡下”，混进来会
    # 让 P95 看起来变好。被拒请求单独出一行。
    latencies = sorted(item["latency"] for item in ok_results)
    reject_latencies = sorted(item["latency"] for item in failed_results)
    ttfts = sorted(item["ttft"] for item in ok_results if item["ttft"] is not None)
    statuses = Counter(item["status"] if item["status"] is not None else "transport" for item in results)

    def error_reason(item: dict) -> str:
        if item["status"] is None:
            return (item["error"] or "transport error")[:120]
        text = item["error"] or ""
        message = text
        try:
            parsed = json.loads(text)
            candidate = (parsed.get("error") or {}).get("message")
            if isinstance(candidate, str):
                message = candidate
        except (json.JSONDecodeError, AttributeError, TypeError):
            pass
        message = re.sub(r"\s*\(requestId: [^)]+\)", "", message).strip()
        return f"HTTP {item['status']}: {message[:100] or 'no body'}"

    errors = Counter(error_reason(item) for item in failed_results)
    prompt_tokens = sum(usage_total(item["usage"], "prompt_tokens") for item in results)
    completion_tokens = sum(usage_total(item["usage"], "completion_tokens") for item in results)
    total_tokens = prompt_tokens + completion_tokens

    print()
    print("results")
    print(f"  success    {len(ok_results)} / {len(results)} ({len(ok_results) / len(results) * 100:.1f}%)")
    print(f"  statuses   {', '.join(f'{key}:{value}' for key, value in sorted(statuses.items(), key=lambda x: str(x[0])))}")
    if latencies:
        print(
            "  latency    (success) "
            f"mean {fmt_seconds(statistics.fmean(latencies))} · "
            f"p50 {fmt_seconds(percentile(latencies, 50))} · "
            f"p90 {fmt_seconds(percentile(latencies, 90))} · "
            f"p95 {fmt_seconds(percentile(latencies, 95))} · "
            f"p99 {fmt_seconds(percentile(latencies, 99))} · "
            f"max {fmt_seconds(latencies[-1])}"
        )
    if ttfts:
        print(
            "  ttft       "
            f"p50 {fmt_seconds(percentile(ttfts, 50))} · "
            f"p95 {fmt_seconds(percentile(ttfts, 95))} · "
            f"max {fmt_seconds(ttfts[-1])}"
        )
    if reject_latencies:
        print(
            "  rejected   "
            f"mean {fmt_seconds(statistics.fmean(reject_latencies))} · "
            f"p95 {fmt_seconds(percentile(reject_latencies, 95))}"
        )
    print(
        f"  throughput offered {len(results) / wall:.1f} req/s · "
        f"successful {len(ok_results) / wall:.1f} req/s (wall {fmt_seconds(wall)})"
    )
    print(f"  tokens     prompt {prompt_tokens:,} / completion {completion_tokens:,} · {total_tokens / wall:,.0f} tok/s")
    if errors:
        print("  errors")
        for message, count in errors.most_common(5):
            print(f"    {count}× {message}")

    if args.json_out:
        with open(args.json_out, "w", encoding="utf-8") as handle:
            json.dump(
                {
                    "url": args.url,
                    "model": args.model,
                    "concurrency": args.concurrency,
                    "requests": requests_count,
                    "wall_seconds": wall,
                    "results": results,
                },
                handle,
                ensure_ascii=False,
                indent=2,
            )
        print(f"  raw        {args.json_out}")

    return 0 if len(ok_results) == len(results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
