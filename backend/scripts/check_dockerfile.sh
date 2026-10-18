#!/usr/bin/env bash
# check_dockerfile.sh —— backend/Dockerfile 验收脚本（入库，CI 也用）
#
# 断言 docs/platform-02 §5（多阶段构建、单二进制、非 root、内置 healthcheck）
# 与 2026-10-18 业主裁决 GC-4 的冻结取值。
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DOCKERFILE="$ROOT/Dockerfile"
fail=0

check() { # check <描述> <命令...>
  local desc="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    echo "PASS  $desc"
  else
    echo "FAIL  $desc"
    fail=$((fail + 1))
  fi
}

check_not() { # check_not <描述> <模式>
  local desc="$1"
  local pattern="$2"
  if grep -qE -- "$pattern" "$DOCKERFILE" 2>/dev/null; then
    echo "FAIL  $desc"
    fail=$((fail + 1))
  else
    echo "PASS  $desc"
  fi
}

if [ ! -f "$DOCKERFILE" ]; then
  echo "FAIL  backend/Dockerfile 不存在：$DOCKERFILE"
  echo
  echo "check_dockerfile: 1 项断言失败"
  exit 1
fi

# 构建阶段与产物：单二进制 newtg
check "builder 使用 golang:1.26-alpine"       grep -qE '^FROM golang:1\.26-alpine' "$DOCKERFILE"
check "runtime 使用 alpine:3.21"             grep -qE '^FROM alpine:3\.21' "$DOCKERFILE"
check "CGO_ENABLED=0"                        grep -q 'CGO_ENABLED=0' "$DOCKERFILE"
check "先 COPY go.mod go.sum 做依赖层缓存"    grep -qE '^COPY go\.mod go\.sum' "$DOCKERFILE"
check "go mod download"                      grep -q 'go mod download' "$DOCKERFILE"
check "定义 ARG VERSION"                     grep -qE '^ARG VERSION' "$DOCKERFILE"
check "版本注入 internal/version.Version"     grep -q 'internal/version\.Version=' "$DOCKERFILE"
check "go build 产物为 ./cmd/newtg"          grep -qF -- '-o /out/newtg ./cmd/newtg' "$DOCKERFILE"
check "仅构建 ./cmd/newtg，无 cmd/gateway|cmd/worker" \
  bash -c '! grep -qE -- "cmd/(gateway|worker)" "$1"' _ "$DOCKERFILE"

# 运行阶段：内容与安全
check "apk add --no-cache ca-certificates tzdata" grep -qF -- 'apk add --no-cache ca-certificates tzdata' "$DOCKERFILE"
check "非 root 用户 newtg uid 65532"          grep -qE 'adduser .*(-u 65532|--uid 65532)' "$DOCKERFILE"
check "含 USER 指令切到非 root"               grep -qE '^USER newtg' "$DOCKERFILE"
check "COPY 二进制到 /newtg"                  grep -qF -- 'COPY --from=builder /out/newtg /newtg' "$DOCKERFILE"
check "COPY 迁移到 /migrations"               grep -qF -- 'COPY migrations /migrations' "$DOCKERFILE"
check "COPY 日志库迁移到 /migrations-logs"     grep -qF -- 'COPY migrations-logs /migrations-logs' "$DOCKERFILE"
check "EXPOSE 3000"                          grep -qE '^EXPOSE 3000$' "$DOCKERFILE"
check "WORKDIR /data"                        grep -qE '^WORKDIR /data$' "$DOCKERFILE"
check "HEALTHCHECK 走内置 /newtg healthcheck" \
  grep -qF -- 'CMD ["/newtg","healthcheck","--url","http://127.0.0.1:3000/readyz"]' "$DOCKERFILE"
check "HEALTHCHECK 时序 10s/3s/5/30s"         grep -qF -- '--interval=10s --timeout=3s --retries=5 --start-period=30s' "$DOCKERFILE"
check "ENTRYPOINT 为 /newtg"                  grep -qF -- 'ENTRYPOINT ["/newtg"]' "$DOCKERFILE"

# 冻结裁决：不得出现 postgres 与服务端其它角色构建
check_not "不含 postgres"                     'postgres'
check_not "不含 token-gateway / new-api 标识" 'token-gateway|new-api'

echo
if [ "$fail" -gt 0 ]; then
  echo "check_dockerfile: $fail 项断言失败"
  exit 1
fi
echo "check_dockerfile: 全部断言通过"
