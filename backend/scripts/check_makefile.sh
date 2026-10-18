#!/usr/bin/env bash
# check_makefile.sh —— M0 仓库根骨架验收脚本（入库，CI 也用）
#
# 逐行断言 Makefile 符合 docs/platform-02-仓库构建与CI-设计.md §4
# 及 2026-10-18 业主裁决：
#   (a) 角色选择用 RUN_MODE=gateway / RUN_MODE=worker，不再使用 NEWTG_ROLE；
#   (b) migrate-create 使用 14 位 UTC 时间戳格式 20060102150405；
#   (c) build 单产物 bin/newtg（不再有 cmd/gateway、cmd/worker 双产物）；
#   (d) MIGRATE_VERSION / KIN_OPENAPI_VERSION 经 CI 变量注入的注释占位存在。
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAKEFILE="$ROOT/Makefile"
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

# (a) RUN_MODE 角色选择 + 禁用 NEWTG_ROLE
check "Makefile 含 RUN_MODE=gateway" grep -q 'RUN_MODE=gateway' "$MAKEFILE"
check "Makefile 含 RUN_MODE=worker"  grep -q 'RUN_MODE=worker'  "$MAKEFILE"
if grep -q 'NEWTG_ROLE' "$MAKEFILE"; then
  echo "FAIL  Makefile 不得含 NEWTG_ROLE（角色选择已改 RUN_MODE）"
  fail=$((fail + 1))
else
  echo "PASS  Makefile 不含 NEWTG_ROLE"
fi

# (b) migrate-create 的 14 位 UTC 时间戳格式
check "Makefile 含 -format 20060102150405" grep -qF -- '-format 20060102150405' "$MAKEFILE"

# (c) build 单产物 bin/newtg，不再有双角色 main
check "build 产物为 bin/newtg" grep -q 'bin/newtg' "$MAKEFILE"
if grep -qE 'cmd/(gateway|worker)' "$MAKEFILE"; then
  echo "FAIL  Makefile 不得再引用 cmd/gateway 或 cmd/worker（单产物 ./cmd/newtg）"
  fail=$((fail + 1))
else
  echo "PASS  Makefile 不含 cmd/gateway|cmd/worker"
fi

# (d) CI 变量注入的注释占位
check "Makefile 含 MIGRATE_VERSION"     grep -q 'MIGRATE_VERSION'     "$MAKEFILE"
check "Makefile 含 KIN_OPENAPI_VERSION" grep -q 'KIN_OPENAPI_VERSION' "$MAKEFILE"
check "CI 变量注入注释占位存在"          grep -q 'CI 变量'              "$MAKEFILE"

echo
if [ "$fail" -gt 0 ]; then
  echo "check_makefile: $fail 项断言失败"
  exit 1
fi
echo "check_makefile: 全部断言通过"
