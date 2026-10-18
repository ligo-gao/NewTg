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
# 键允许带双引号（`"$ref":`）且冒号前可有空格——YAML 合法写法，漏掉等于放行
# 未校验的引用（extract 不到 = scan_refs 看不到 = 永远 PASS）。
extract_refs() {
  grep -oE '"?\$ref"?[[:space:]]*:[[:space:]]*[^[:space:]]+' "$1" 2>/dev/null \
    | sed -E "s/^\"?\\\$ref\"?[[:space:]]*:[[:space:]]*//; s/^['\"]//; s/['\"]?[,}]?$//" || true
}

# 纯词法路径规范化，等价 `realpath -m`：目标允许不存在，也不依赖 GNU coreutils
# （macOS/BSD 的 realpath 没有 -m；CI 与本机必须同一行为）。相对路径以当前 $PWD 为基准。
# `..` 越过根时停在根，与 realpath -m 一致。
normalize_path() {
  local path="$1" out="" part
  local -a parts
  case "$path" in
    /*) ;;
    *) path="$PWD/$path" ;;
  esac
  local IFS=/
  read -r -a parts <<<"$path"
  for part in "${parts[@]}"; do
    case "$part" in
      ''|.) ;;
      ..) out="${out%/*}" ;;
      *) out="$out/$part" ;;
    esac
  done
  printf '%s\n' "${out:-/}"
}

# 扫描 root 下全部 YAML：$ref 只能指向 root 内已存在的文件（#/... 纯片段引用跳过）。
# 返回 0=全部合法；1=存在违规（逐条打印原因）。
scan_refs() {
  local root="$1" root_abs f dir_abs ref target resolved rc=0
  # root 必须真实存在，用 cd+pwd -P 取物理绝对路径（跨平台，不依赖 realpath -m）。
  root_abs=$(cd "$root" && pwd -P) || return 1
  while IFS= read -r -d '' f; do
    dir_abs=$(cd "$(dirname "$f")" && pwd -P) || continue
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
      # 词法解析后做前缀包含判断：目标即使不存在，也能判定是否逃出 root。
      resolved=$(normalize_path "$dir_abs/$target")
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

# 失败先行自检：临时副本中逐条注入越界 $ref，必须被 scan_refs 以预期原因拒绝。
# 为什么要注入用例：只验证“当前源树通过”无法区分“守卫有效”与“规则退化成永远 PASS”；
# 用临时副本注入可重复证明每条拒绝路径真的生效，且不污染真实源树。
# 每个用例单独落一个 YAML 文件：一次只注入一条引用，拒绝原因才能精确归因。
# SELFTEST_TMP 用全局变量：EXIT trap 触发时函数局部变量已出作用域（set -u 下会报 unbound）。
SELFTEST_TMP=
self_test() {
  local tmp out rc=0
  tmp=$(mktemp -d)
  SELFTEST_TMP=$tmp
  trap 'rm -rf "${SELFTEST_TMP:-}"' EXIT
  mkdir -p "$tmp/api"
  cp -a api/openapi "$tmp/api/openapi"

  run_case() { # run_case <名称> <期望关键字> <注入的 YAML 行>
    local case_name="$1" expect="$2" yaml_line="$3"
    local case_file="$tmp/api/openapi/selftest-${case_name}.yaml"
    printf 'x-selftest:\n  %s\n' "$yaml_line" >"$case_file"
    if out=$(scan_refs "$tmp/api/openapi" 2>&1); then
      echo "FAIL: 自检 [$case_name] 未被拒绝：$yaml_line" >&2
      rc=1
    elif ! printf '%s' "$out" | grep -qF -- "$expect"; then
      echo "FAIL: 自检 [$case_name] 拒绝原因不符（期望含「$expect」）：$out" >&2
      rc=1
    else
      echo "PASS: 自检 [$case_name] 已被拒绝（$expect）"
    fi
    rm -f "$case_file"
  }

  # 引号键 + 冒号前空格：extract_refs 必须仍能提取，否则该用例会因「未检出」而 FAIL。
  run_case escape-quoted 逃出 "\"\$ref\" : '../outside.yaml#/schemas/Escape'"
  # 普通写法的一级越界（../ 从 api/openapi 到 api/，已在 root 外）。
  run_case escape-plain 逃出 "\$ref: '../outside.yaml#/schemas/Escape'"
  # http(s) 引用会把远端内容注入 bundle 与下游 codegen（供应链面）。
  run_case network 禁止网络 "\$ref: 'https://evil.example/schemas.yaml#/X'"
  # 绝对路径可读取宿主任意文件（文件泄露面）。
  run_case absolute 禁止绝对路径 "\$ref: '/etc/passwd#/X'"
  # 路径在树内但目标不存在：bundle 会在解析阶段才失败，必须提前拦住。
  run_case missing 目标不存在 "\$ref: './paths/does-not-exist.yaml#/X'"

  [ "$rc" -eq 0 ] && echo "PASS: 自检——越界/网络/绝对路径/缺失目标注入均已被拒绝"
  return "$rc"
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
