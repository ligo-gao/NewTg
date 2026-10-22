#!/usr/bin/env python3
"""NewTG 最小可用压测脚本（stdlib-only）。

范围与纪律（见 README.md 与 docs/platform-06-压测方案-设计.md）：
  - 只打 POST {base_url}/v1/chat/completions（OpenAI 协议）；
  - 支持非流式与 stream=true（SSE），记录首字时间；
  - 只从环境变量 NEWTG_API_TOKEN 读取令牌，不接受命令行 token；
  - 不做自动重试：连接失败/超时都记为失败，避免重复计费；
  - 输出 JSON 摘要与（可选）成功请求的 X-Request-ID 列表，供
    ledger_check.sh 做账本一致性断言。

这不是 k6 的替代品。platform-06 的完整场景矩阵（S1–S13）、longconn
（SSE/WS 长连接）与阈值判定仍需按设计执行；本脚本覆盖 S1/S2 的最小闭环
与压后账本核对。

用法：
  export NEWTG_API_TOKEN='<loadtest token>'
  export NEWTG_MODEL='<cheapest text model>'
  python3 deploy/ops/loadtest/loadtest.py --requests 200 --concurrency 10 \
      --output /tmp/newtg-run.json --request-id-file /tmp/newtg-ids.txt
"""

from __future__ import annotations

import argparse
import http.client
import json
import math
import os
import ssl
import sys
import threading
import time
import urllib.parse
import uuid
from collections import Counter
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def positive_int(value: str) -> int:
    try:
        n = int(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("must be an integer") from exc
    if n <= 0:
        raise argparse.ArgumentTypeError("must be > 0")
    return n


def non_negative_float(value: str) -> float:
    try:
        n = float(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("must be a number") from exc
    if n < 0:
        raise argparse.ArgumentTypeError("must be >= 0")
    return n


@dataclass
class Result:
    ok: bool
    status: int | None
    latency_ms: float
    ttft_ms: float | None
    request_id: str | None
    prompt_tokens: int
    completion_tokens: int
    error: str


class Scheduler:
    """分配请求序号；--rate > 0 时按恒定到达率错开发车。"""

    def __init__(self, total: int, rate: float) -> None:
        self._total = total
        self._rate = rate
        self._next = 0
        self._next_start = time.monotonic()
        self._lock = threading.Lock()

    def next(self) -> tuple[int, float]:
        with self._lock:
            if self._next >= self._total:
                return -1, 0.0
            index = self._next
            self._next += 1
            if self._rate <= 0:
                return index, 0.0
            now = time.monotonic()
            if self._next_start < now:
                self._next_start = now
            delay = self._next_start - now
            self._next_start += 1.0 / self._rate
            return index, max(0.0, delay)


class Client:
    def __init__(self, args: argparse.Namespace) -> None:
        parsed = urllib.parse.urlsplit(args.base_url)
        if parsed.scheme not in ("http", "https"):
            raise SystemExit("loadtest: --base-url must start with http:// or https://")
        if not parsed.hostname:
            raise SystemExit("loadtest: --base-url has no host")
        self.scheme = parsed.scheme
        self.host = parsed.hostname
        self.port = parsed.port or (443 if self.scheme == "https" else 80)
        self.path = parsed.path.rstrip("/") + args.path
        self.args = args
        self.body = json.dumps(
            {
                "model": args.model,
                "messages": [{"role": "user", "content": args.prompt}],
                "max_tokens": args.max_tokens,
                "stream": args.stream,
                **(
                    {"stream_options": {"include_usage": True}}
                    if args.stream and args.stream_usage
                    else {}
                ),
            },
            separators=(",", ":"),
        ).encode("utf-8")
        self.headers = {
            "Authorization": f"Bearer {args.token}",
            "Content-Type": "application/json",
            "Accept": "text/event-stream" if args.stream else "application/json",
            "User-Agent": "newtg-loadtest/1.0",
        }
        self._ssl = None
        if self.scheme == "https":
            self._ssl = ssl.create_default_context()
            if args.insecure:
                self._ssl.check_hostname = False
                self._ssl.verify_mode = ssl.CERT_NONE
        self._conn: http.client.HTTPConnection | None = None

    def _connect(self) -> http.client.HTTPConnection:
        if self.scheme == "https":
            return http.client.HTTPSConnection(
                self.host, self.port, timeout=self.args.timeout, context=self._ssl
            )
        return http.client.HTTPConnection(self.host, self.port, timeout=self.args.timeout)

    def _read_non_stream(self, resp: http.client.HTTPResponse) -> tuple[int, int]:
        raw = resp.read()
        prompt = completion = 0
        try:
            body = json.loads(raw)
            usage = body.get("usage") or {}
            prompt = int(usage.get("prompt_tokens") or 0)
            completion = int(usage.get("completion_tokens") or 0)
        except (ValueError, TypeError):
            pass
        return prompt, completion

    def _read_stream(
        self, resp: http.client.HTTPResponse, start: float
    ) -> tuple[float | None, int, int]:
        """返回 (首字耗时 ms, prompt tokens, completion tokens)。"""
        first: float | None = None
        prompt = completion = 0
        while True:
            line = resp.readline()
            if not line:
                break
            if not line.strip():
                continue
            if line.startswith(b"data:"):
                payload = line[5:].strip()
                if payload == b"[DONE]":
                    break
                if first is None:
                    first = (time.perf_counter() - start) * 1000.0
                try:
                    chunk = json.loads(payload)
                except ValueError:
                    continue
                usage = chunk.get("usage") or {}
                if usage:
                    prompt = int(usage.get("prompt_tokens") or prompt)
                    completion = int(usage.get("completion_tokens") or completion)
        return first, prompt, completion

    def request(self) -> Result:
        start = time.perf_counter()
        status: int | None = None
        ttft: float | None = None
        prompt = completion = 0
        request_id: str | None = None
        try:
            if self._conn is None:
                self._conn = self._connect()
            self._conn.request("POST", self.path, body=self.body, headers=self.headers)
            resp = self._conn.getresponse()
            status = resp.status
            request_id = resp.getheader("X-Request-ID") or resp.getheader("X-Request-Id")
            content_type = (resp.getheader("Content-Type") or "").lower()
            if self.args.stream and "text/event-stream" in content_type:
                ttft, prompt, completion = self._read_stream(resp, start)
            else:
                prompt, completion = self._read_non_stream(resp)
            if resp.will_close:
                self._conn.close()
                self._conn = None
            latency = (time.perf_counter() - start) * 1000.0
            ok = 200 <= status < 300
            return Result(
                ok=ok,
                status=status,
                latency_ms=latency,
                ttft_ms=ttft,
                request_id=request_id,
                prompt_tokens=prompt,
                completion_tokens=completion,
                error="" if ok else f"http_{status}",
            )
        except Exception as exc:  # 连接/超时/协议错误：不重试，避免重复计费。
            if self._conn is not None:
                try:
                    self._conn.close()
                except Exception:
                    pass
                self._conn = None
            latency = (time.perf_counter() - start) * 1000.0
            return Result(
                ok=False,
                status=status,
                latency_ms=latency,
                ttft_ms=ttft,
                request_id=request_id,
                prompt_tokens=prompt,
                completion_tokens=completion,
                error=type(exc).__name__,
            )


def percentile(sorted_values: list[float], p: float) -> float | None:
    if not sorted_values:
        return None
    rank = max(1, math.ceil(p / 100.0 * len(sorted_values)))
    return sorted_values[min(rank - 1, len(sorted_values) - 1)]


def redact_url(url: str) -> str:
    """去掉 URL 中的 userinfo，避免把凭据写进压测报告。"""
    parsed = urllib.parse.urlsplit(url)
    netloc = parsed.hostname or ""
    if parsed.port:
        netloc = f"{netloc}:{parsed.port}"
    return urllib.parse.urlunsplit((parsed.scheme, netloc, parsed.path, "", ""))


def summarize(
    args: argparse.Namespace, results: list[Result], started: str, finished: str, elapsed: float
) -> dict[str, Any]:
    attempted = len(results)
    ok_results = [r for r in results if r.ok]
    latencies = sorted(r.latency_ms for r in results)
    ttfts = sorted(r.ttft_ms for r in results if r.ttft_ms is not None)
    status_counts = Counter(str(r.status) if r.status is not None else "error" for r in results)
    error_counts = Counter(r.error for r in results if r.error)

    return {
        "run_id": f"{started.replace(':', '').replace('-', '')}-{uuid.uuid4().hex[:8]}",
        "started_at": started,
        "finished_at": finished,
        "config": {
            "base_url": redact_url(args.base_url),
            "path": args.path,
            "model": args.model,
            "stream": args.stream,
            "stream_usage": args.stream_usage,
            "concurrency": args.concurrency,
            "rate_per_second": args.rate,
            "requests_requested": args.requests,
            "max_tokens": args.max_tokens,
            "timeout_seconds": args.timeout,
        },
        "requests_attempted": attempted,
        "ok": len(ok_results),
        "failed": attempted - len(ok_results),
        "success_rate": (len(ok_results) / attempted) if attempted else 0.0,
        "error_rate": ((attempted - len(ok_results)) / attempted) if attempted else 1.0,
        "rps": (attempted / elapsed) if elapsed > 0 else 0.0,
        "elapsed_seconds": round(elapsed, 3),
        "latency_ms": {
            "p50": percentile(latencies, 50),
            "p95": percentile(latencies, 95),
            "p99": percentile(latencies, 99),
            "max": max(latencies) if latencies else None,
            "mean": (sum(latencies) / len(latencies)) if latencies else None,
        },
        "ttft_ms": (
            {
                "p50": percentile(ttfts, 50),
                "p95": percentile(ttfts, 95),
                "p99": percentile(ttfts, 99),
                "max": max(ttfts) if ttfts else None,
            }
            if args.stream
            else None
        ),
        "tokens": {
            "prompt": sum(r.prompt_tokens for r in results),
            "completion": sum(r.completion_tokens for r in results),
        },
        "status_counts": dict(sorted(status_counts.items())),
        "error_counts": dict(sorted(error_counts.items())),
        "request_id_count": sum(1 for r in results if r.request_id),
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="NewTG 最小可用压测（stdlib-only，POST /v1/chat/completions）",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "令牌只从 NEWTG_API_TOKEN 读取。模型可用 --model 或 NEWTG_MODEL。\n"
            "成本护栏与判据见 deploy/ops/loadtest/README.md。"
        ),
    )
    parser.add_argument(
        "--base-url",
        default=os.environ.get("NEWTG_BASE_URL", "http://127.0.0.1:8080"),
        help="入口基址（默认 $NEWTG_BASE_URL 或 http://127.0.0.1:8080）",
    )
    parser.add_argument(
        "--path", default="/v1/chat/completions", help="请求路径（默认 /v1/chat/completions）"
    )
    parser.add_argument(
        "--model", default=os.environ.get("NEWTG_MODEL", ""), help="模型名（或 $NEWTG_MODEL）"
    )
    parser.add_argument(
        "--prompt",
        default=os.environ.get("NEWTG_PROMPT", "Reply with the single word: OK."),
        help="用户消息（不要放敏感内容）",
    )
    parser.add_argument("--requests", type=positive_int, default=100, help="总请求数（默认 100）")
    parser.add_argument("--concurrency", type=positive_int, default=4, help="并发 worker 数（默认 4）")
    parser.add_argument(
        "--rate", type=non_negative_float, default=0.0, help="恒定到达率（请求/秒；0=不限速）"
    )
    parser.add_argument("--stream", action="store_true", help="stream=true（SSE）")
    parser.add_argument(
        "--stream-usage",
        action="store_true",
        help="流式时附带 stream_options.include_usage（部分上游不支持）",
    )
    parser.add_argument("--max-tokens", type=positive_int, default=16, help="max_tokens（默认 16，设计上限 64）")
    parser.add_argument("--timeout", type=non_negative_float, default=120.0, help="单请求超时秒数（默认 120）")
    parser.add_argument(
        "--max-error-rate",
        type=non_negative_float,
        default=0.05,
        help="错误率超过该值则退出码 1（默认 0.05，对齐 platform-06 J7）",
    )
    parser.add_argument(
        "--max-p95-ms",
        type=non_negative_float,
        default=0.0,
        help="客户端 p95 延迟上限（ms；0=不设门禁，仅记录）",
    )
    parser.add_argument("--output", help="JSON 摘要写入文件（默认打印到 stdout）")
    parser.add_argument(
        "--request-id-file",
        help="把成功响应的 X-Request-ID 逐行写入该文件（供 ledger_check.sh）",
    )
    parser.add_argument("--insecure", action="store_true", help="HTTPS 跳过证书校验（仅受控内网）")
    parser.add_argument("--dry-run", action="store_true", help="只打印计划与请求体，不发请求")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    args.token = os.environ.get("NEWTG_API_TOKEN", "")

    if args.dry_run:
        if not args.model:
            print("loadtest: --model or NEWTG_MODEL is required", file=sys.stderr)
            return 2
        print(
            json.dumps(
                {
                    "base_url": args.base_url,
                    "path": args.path,
                    "model": args.model,
                    "requests": args.requests,
                    "concurrency": args.concurrency,
                    "rate": args.rate,
                    "stream": args.stream,
                    "max_tokens": args.max_tokens,
                    "token_present": bool(args.token),
                },
                indent=2,
            )
        )
        print("loadtest: dry-run OK")
        return 0

    if not args.token:
        print("loadtest: NEWTG_API_TOKEN is required (token is never accepted on argv)", file=sys.stderr)
        return 2
    if not args.model:
        print("loadtest: --model or NEWTG_MODEL is required", file=sys.stderr)
        return 2
    if args.max_tokens > 64:
        print(
            "loadtest: WARNING max_tokens > 64 exceeds platform-06 cost guardrail",
            file=sys.stderr,
        )

    # 先构造一次以尽早校验 URL；每个 worker 再持自己的连接，避免多线程
    # 共用同一个 HTTPConnection 造成请求/响应串线。
    Client(args)
    scheduler = Scheduler(args.requests, args.rate)
    results: list[Result] = []
    results_lock = threading.Lock()

    started = utc_now()
    started_monotonic = time.monotonic()

    def worker() -> None:
        client = Client(args)
        local: list[Result] = []
        while True:
            index, delay = scheduler.next()
            if index < 0:
                break
            if delay > 0:
                time.sleep(delay)
            local.append(client.request())
        with results_lock:
            results.extend(local)

    threads = [threading.Thread(target=worker, name=f"newtg-load-{i}") for i in range(args.concurrency)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()

    elapsed = time.monotonic() - started_monotonic
    finished = utc_now()
    summary = summarize(args, results, started, finished, elapsed)

    if args.request_id_file:
        ids = sorted({r.request_id for r in results if r.ok and r.request_id})
        with open(args.request_id_file, "w", encoding="utf-8") as f:
            for request_id in ids:
                f.write(request_id + "\n")
        summary["request_id_file"] = args.request_id_file
        summary["request_id_count"] = len(ids)

    payload = json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=False)
    if args.output:
        with open(args.output, "w", encoding="utf-8") as f:
            f.write(payload + "\n")
        print(
            f"loadtest: run_id={summary['run_id']} ok={summary['ok']} "
            f"failed={summary['failed']} rps={summary['rps']:.2f} "
            f"p95={summary['latency_ms']['p95']} ms -> {args.output}",
            file=sys.stderr,
        )
    else:
        print(payload)

    if summary["ok"] == 0:
        print("loadtest: FAIL no successful requests", file=sys.stderr)
        return 1
    if summary["error_rate"] > args.max_error_rate:
        print(
            f"loadtest: FAIL error_rate={summary['error_rate']:.4f} > {args.max_error_rate}",
            file=sys.stderr,
        )
        return 1
    p95 = summary["latency_ms"]["p95"]
    if args.max_p95_ms > 0 and p95 is not None and p95 > args.max_p95_ms:
        print(f"loadtest: FAIL p95={p95:.1f}ms > {args.max_p95_ms}ms", file=sys.stderr)
        return 1

    print("loadtest: OK", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
