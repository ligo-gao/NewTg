#!/usr/bin/env bash
# check_bundle_size.sh —— 首屏 JS gzip 体积门禁（platform-02 §7.8 / plan Task 15）
#
# 为什么只算「首屏 JS」：用户可交互前必须下载并解析的只有 index.html 里引用的
# 同步/延迟脚本；异步路由 chunk 是按需加载的，不应计入首屏预算。
#
# 为什么阈值是 150 KiB gzip：M0 实测首屏 ≈104 KiB（React + 入口 + 共享 chunk），
# 预留约 45% 余量后冻结；超限通常意味着新增依赖没有做 tree-shaking 或代码分割，
# 必须在合并前处理，而不是等到线上慢查询。阈值变更需同步更新本注释与计划。
#
# 用法：bash scripts/check_bundle_size.sh [dist_dir]
#   默认 dist_dir = frontend/dist；目录缺失且本机有 bun 时自动构建一次。
# 退出：0 通过；1 超限或产物损坏；2 产物缺失且无法自动构建。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="${1:-$ROOT/frontend/dist}"
LIMIT=$((150 * 1024)) # 150 KiB（gzip 后）

if [ ! -f "$DIST/index.html" ]; then
  if command -v bun >/dev/null 2>&1; then
    echo "==> $DIST/index.html 不存在，先执行 frontend 生产构建"
    (cd "$ROOT/frontend" && bun run build)
  else
    echo "FAIL 缺少 $DIST/index.html；请先 cd frontend && bun run build" >&2
    exit 2
  fi
fi

# 提取 index.html 中所有 <script src="...">（含 defer/async 属性）。
# grep 无匹配时退出 1；在 `set -e` 下必须 `|| true`，否则「没有脚本」的报错分支不可达。
scripts="$(grep -oE '<script[^>]*[[:space:]]src="[^"]+"' "$DIST/index.html" \
  | sed -E 's/.*[[:space:]]src="([^"]+)".*/\1/' || true)"
if [ -z "$scripts" ]; then
  echo "FAIL index.html 中未找到 <script src>，无法计算首屏体积" >&2
  exit 1
fi

total=0
while IFS= read -r src; do
  [ -n "$src" ] || continue
  case "$src" in
    http://*|https://*|//*)
      echo "skip 外链（不计入本地产物）：$src"
      continue
      ;;
  esac
  file="$DIST/${src#/}"
  if [ ! -f "$file" ]; then
    echo "FAIL index.html 引用的 JS 不存在：$file" >&2
    exit 1
  fi
  # -n：不写文件名与时间戳，保证同一产物每次得到相同字节数。
  bytes="$(gzip -9 -n -c "$file" | wc -c | tr -d '[:space:]')"
  total=$((total + bytes))
  printf '%8d B gzip  %s\n' "$bytes" "$src"
done <<< "$scripts"

if [ "$total" -gt "$LIMIT" ]; then
  echo "FAIL 首屏 gzip JS ${total}B > 上限 ${LIMIT}B（150 KiB）" >&2
  exit 1
fi
echo "PASS 首屏 gzip JS ${total}B ≤ ${LIMIT}B（150 KiB）"
