#!/usr/bin/env bash
# 校验 OpenAPI 源目录树、入口 $ref 汇总与 $ref 边界（platform-03 §9.1 / C-6·D8：仓根 api/）。
# $ref 边界：只允许引用 api/openapi/ 内已存在的文件；禁止 http(s)、绝对路径与 ../ 越界。
# 安全动机：bundle 会无条件内联 $ref 目标——放开 http(s)/../../ 等于把任意本地文件或远端内容
# 注入单体产物与下游 codegen（供应链/路径穿越）。Spectral 在求值 given 前已解析 $ref，
# 规则层观察不到原始引用值，因此边界校验必须在本脚本强制执行。
# 失败即非零退出；供 CI 与本地 `make api-check` 调用。
# 自检：`bash scripts/check_openapi_layout.sh --self-test`（注入越界 $ref，必须被拒绝）。
set -euo pipefail

cd "$(dirname "$0")/.."

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# 提取文件中所有 $ref 值（块式/列表式/流式；去掉引号、尾逗号与尾花括号）。
extract_refs() {
  grep -oE '\$ref:[[:space:]]*[^[:space:]]+' "$1" 2>/dev/null \
    | sed -E "s/^\\\$ref:[[:space:]]*//; s/^['\"]//; s/['\"]?[,}]?$//" || true
}

# 扫描 root 下全部 YAML：$ref 只能指向 root 内已存在的文件（#/... 纯片段引用跳过）。
# 返回 0=全部合法；1=存在违规（逐条打印原因）。
scan_refs() {
  local root="$1" root_abs f dir ref target resolved rc=0
  root_abs=$(realpath -m "$root")
  while IFS= read -r -d '' f; do
    dir=$(dirname "$f")
    while IFS= read -r ref; do
      [ -n "$ref" ] || continue
      # #/... 是当前文档内的纯片段引用：没有文件系统目标，无法越界，故不参与路径解析。
      case "$ref" in
        '#'*) continue ;;
      esac
      target="${ref%%#*}"
      [ -n "$target" ] || continue
      case "$target" in
        http://* | https://*)
          echo "FAIL: $f: 禁止网络 \$ref：$ref" >&2
          rc=1
          continue
          ;;
        /*)
          echo "FAIL: $f: 禁止绝对路径 \$ref：$ref" >&2
          rc=1
          continue
          ;;
      esac
      resolved=$(realpath -m "$dir/$target")
      case "$resolved" in
        "$root_abs"/*) ;;
        *)
          echo "FAIL: $f: \$ref 逃出 $root（解析为 $resolved）：$ref" >&2
          rc=1
          continue
          ;;
      esac
      if [ ! -f "$resolved" ]; then
        echo "FAIL: $f: \$ref 目标不存在：$ref → $resolved" >&2
        rc=1
      fi
    done < <(extract_refs "$f")
  done < <(find "$root" -type f \( -name '*.yaml' -o -name '*.yml' \) -print0)
  return "$rc"
}

# 失败先行自检：临时副本中注入越界 $ref，必须被 scan_refs 拒绝。
# 为什么要注入用例：只验证“当前源树通过”无法区分“守卫有效”与“规则退化成永远 PASS”；
# 用临时副本注入可重复证明拒绝路径真的生效，且不污染真实源树。
# SELFTEST_TMP 用全局变量：EXIT trap 触发时函数局部变量已出作用域（set -u 下会报 unbound）。
SELFTEST_TMP=
self_test() {
  local tmp out
  tmp=$(mktemp -d)
  SELFTEST_TMP=$tmp
  trap 'rm -rf "${SELFTEST_TMP:-}"' EXIT
  mkdir -p "$tmp/api"
  cp -a api/openapi "$tmp/api/openapi"
  cat >>"$tmp/api/openapi/openapi.yaml" <<'EOF'

# self-test: 越界 $ref（必须被拒绝）
x-selftest-escape:
  $ref: '../../outside.yaml#/schemas/Escape'
EOF
  if out=$(scan_refs "$tmp/api/openapi" 2>&1); then
    echo "FAIL: 自检未拒绝越界 \$ref（注入未被检出）" >&2
    return 1
  fi
  case "$out" in
    *逃出*)
      echo "PASS: 自检——越界 \$ref 注入已被拒绝（$out）"
      ;;
    *)
      echo "FAIL: 自检拒绝原因不符合预期：$out" >&2
      return 1
      ;;
  esac
}

case "${1:-}" in
  --self-test)
    self_test
    exit $?
    ;;
  '') ;;
  *)
    echo "用法: $0 [--self-test]" >&2
    exit 2
    ;;
esac

ENTRY=api/openapi/openapi.yaml

[ -f "$ENTRY" ] || fail "缺少入口 $ENTRY"
[ ! -e backend/api/openapi.yaml ] || fail "旧路径 backend/api/openapi.yaml 仍存在（契约源必须在仓根 api/openapi/）"

# 1) 五个公共组件文件
for f in security problem pagination params responses; do
  [ -f "api/openapi/components/$f.yaml" ] || fail "缺少 api/openapi/components/$f.yaml"
done

# 2) 四个域 paths + schemas 文件（另含 ops 探测域）
for d in ops public user admin relay; do
  [ -f "api/openapi/paths/$d.yaml" ] || fail "缺少 api/openapi/paths/$d.yaml"
done
for d in public user admin relay; do
  [ -f "api/openapi/schemas/$d.yaml" ] || fail "缺少 api/openapi/schemas/$d.yaml"
done

# 3) 入口必须是 OpenAPI 3.1
grep -qE '^openapi: 3\.1\.[0-9]+$' "$ENTRY" || fail "入口 openapi 版本必须为 3.1.x"

# 4) 入口 paths 下每一项都必须是 $ref（不允许内联 path item）
awk '
  /^paths:[[:space:]]*$/ { inpaths = 1; next }
  inpaths && /^[^[:space:]#]/ { inpaths = 0 }
  inpaths && /^  \// { paths++ }
  inpaths && /\$ref:/ { refs++ }
  END { if (paths == 0 || paths != refs) exit 1 }
' "$ENTRY" || fail "入口 paths 未全部使用 \$ref 汇总（每条 path 一个 \$ref）"

# 5) 五个公共组件与四个域 paths/schemas 文件均须被入口引用
for c in security problem pagination params responses; do
  grep -q "components/$c.yaml" "$ENTRY" || fail "入口未引用 components/$c.yaml"
done
for d in ops public user admin relay; do
  grep -q "paths/$d.yaml" "$ENTRY" || fail "入口未引用 paths/$d.yaml"
done
for d in public user admin; do
  grep -q "schemas/$d.yaml" "$ENTRY" || fail "入口未引用 schemas/$d.yaml"
done

# 6) $ref 边界：不得跨出 api/openapi/，目标文件必须存在
scan_refs api/openapi || fail "\$ref 边界校验失败（只允许引用 api/openapi/ 内已存在的文件）"

# 7) relay schema 源文件存在但不承载协议 schema（§9.3：真源在 contract/adapter）
grep -q 'x-codegen: false' api/openapi/paths/relay.yaml || fail "relay paths 必须打 x-codegen: false"

echo "PASS: OpenAPI layout 校验通过（仓根 api/openapi/，入口 paths 全为 \$ref，\$ref 未越界）"
