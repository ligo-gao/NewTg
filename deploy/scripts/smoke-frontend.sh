#!/usr/bin/env bash
# 前端镜像冒烟：构建 → 起容器 → 断言控制台/官网的路由分流。
# 用法：bash deploy/scripts/smoke-frontend.sh [tag]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TAG="${1:-smoke}"
IMAGE="newtg/frontend:${TAG}"
NAME="newtg-fe-smoke"
PORT=18080

cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "smoke: building $IMAGE"
docker build -q -f "$ROOT/frontend/Dockerfile" -t "$IMAGE" "$ROOT" >/dev/null

cleanup
docker run -d --rm --name "$NAME" -p "${PORT}:8080" "$IMAGE" >/dev/null

for _ in $(seq 1 30); do
  if curl -fsS -o /dev/null "http://127.0.0.1:${PORT}/"; then break; fi
  sleep 1
done

# 取一个真实存在的官网静态资源，验证 /_next/ 前缀没被 SPA 兜底截走。
NEXT_ASSET="$(cd "$ROOT/website/out" && find _next -type f -name '*.js' | head -1)"
if [ -z "$NEXT_ASSET" ]; then
  echo "smoke: FAIL 找不到 website/out/_next 下的静态资源" >&2
  exit 1
fi

fail=0

# expect <path> <marker>：状态码必须是 200，且 body 含 marker。
expect() {
  local path="$1" marker="$2" body status
  body="$(mktemp)"
  status="$(curl -s -o "$body" -w '%{http_code}' "http://127.0.0.1:${PORT}${path}" || true)"
  if [ "$status" != "200" ] || ! grep -q "$marker" "$body"; then
    echo "smoke: FAIL ${path} → ${status}, marker '${marker}' $(grep -q "$marker" "$body" && echo present || echo missing)" >&2
    head -c 200 "$body" >&2
    echo >&2
    fail=1
  else
    echo "smoke: ok   ${path}"
  fi
  rm -f "$body"
}

expect "/" "__next"                 # 官网首页
expect "/pricing" "__next"          # 官网价格页
expect "/en/" "__next"              # 官网语言首页
expect "/zh-TW/pricing/" "__next"   # 官网语言价格页
expect "/${NEXT_ASSET}" "__next"    # 官网静态资源
expect "/sign-in" 'id="root"'       # 控制台路由
expect "/admin/channels" 'id="root"'
expect "/pricing/enterprise" 'id="root"' # 官网没有的路径必须回落控制台

if [ "$fail" -ne 0 ]; then
  echo "smoke: FAILED" >&2
  exit 1
fi
echo "smoke: all routes ok"
