#!/usr/bin/env bash
# 校验 OpenAPI 源目录树、入口 $ref 汇总与 $ref 边界（platform-03 §9.1 / C-6·D8：仓根 api/）。
# $ref 边界：只允许引用 api/openapi/ 内已存在的文件；禁止 http(s)、绝对路径与 ../ 越界。
# 安全动机：bundle 会无条件内联 $ref 目标——放开 http(s)/../../ 等于把任意本地文件或远端内容
# 注入单体产物与下游 codegen（供应链/路径穿越）。Spectral 在求值 given 前已解析 $ref，
# 规则层观察不到原始引用值，因此边界校验必须在本脚本强制执行。
# 权限门禁（2026-10-18 终审 area4 Minor 15）：
#   · 每个 x-required-permission 的取值必须属于 backend/internal/rbac/catalog.go 的 20 键目录；
#   · tags 含 admin 的 operation 必须声明 x-required-permission（当前 15/15，防未来漂移）。
#   第三条「spec 路径 ⊆ 已注册路由」需要解析 Go 路由表，正则黑名单既脆弱又容易误报，
#   故不在本脚本实现（报告登记为后续项）。
# 失败即非零退出；供 CI 与本地 `make api-check` 调用。
# 自检：`bash scripts/check_openapi_layout.sh --self-test`（注入越界 $ref / 非法权限，
# 必须被拒绝）。
set -euo pipefail

cd "$(dirname "$0")/.."

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# 多行 $ref 提取与物理路径解析依赖 perl（Cwd::realpath 是核心模块）。缺失即失败，
# 不允许降级成「只扫同行引用」——那会重新打开多行值与符号链接的绕过面。
command -v perl >/dev/null 2>&1 \
  || fail "本脚本需要 perl（多行 \$ref 提取 + Cwd::realpath，required for this check）"

# 提取文件中所有 $ref 值（块式/列表式/流式；去掉引号、尾逗号与尾花括号）。
# 键允许单/双引号（`"$ref":`、`'$ref' :`）且冒号前可有空格——这些都是合法 YAML，
# 漏掉等于放行未校验的引用（extract 不到 = scan_refs 看不到 = 永远 PASS）。
# 值的两种合法形态都要覆盖：与键同行，以及下一行的缩进标量（`$ref:` 换行后缩进）。
extract_refs() {
  # 1) 键与值同行。
  #    YAML 中冒号后以 `#` 开头的 token 是行尾注释而不是值（`$ref: # 注释` 的值在
  #    下一行）。同行 grep 会把它捕获成裸 `#`；若留到 scan_refs，就会因 `#*` 被当成
  #    文档内片段而跳过，真正的下一行值于是完全不经路径校验（PyYAML/redocly 却会
  #    读取它）。因此丢弃以 `#` 开头的捕获值，交给下面的状态机取下一行。
  #    （带引号的 `'#/X'` 也是文档内片段，scan_refs 本就跳过，丢弃不改变语义。）
  grep -oE "[\"']?\\\$ref[\"']?[[:space:]]*:[[:space:]]*[^[:space:]]+" "$1" 2>/dev/null \
    | sed -E "s/^[\"']?\\\$ref[\"']?[[:space:]]*:[[:space:]]*//; s/^[\"']//; s/[\"']?[,}]?\$//" \
    | grep -v '^#' || true
  # 2) 值在下一行：
  #      $ref:              $ref: # 行尾注释（YAML 合法）
  #        ../outside.yaml#/X
  #    逐行状态机，只取键行后的第一个非空非注释行；不处理折叠标量（$ref 路径是单行）。
  #    键行必须允许行尾注释：`\s*(#.*)?$` 才是 YAML 的完整语义。只认「冒号后直接
  #    换行」会让带注释的多行引用整条绕过边界校验（同行 grep 只看到注释）。
  perl -ne '
    if ($want) {
      next if /^\s*(#.*)?$/;
      my $v = $_;
      $v =~ s/^\s+//;
      $v =~ s/\s+$//;
      $v =~ s/^[\x27"]//;
      $v =~ s/[\x27"]?[,}]?$//;
      print "$v\n";
      $want = 0;
      next;
    }
    $want = 1 if !/^\s*#/ && /["\x27]?\$ref["\x27]?\s*:\s*(#.*)?$/;
  ' "$1" 2>/dev/null || true
}

# 纯词法路径规范化（realpath -m 的词法部分）：目标允许不存在，也不依赖 GNU coreutils
# （macOS/BSD 的 realpath 没有 -m）。相对路径以当前 $PWD 为基准；`..` 越过根时停在根。
# 注意：它不解析符号链接，因此 scan_refs 还会物理解析父目录并拒绝链接本体。
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

# 物理路径解析（跟随符号链接）。用 perl 核心模块 Cwd::realpath：跨平台一致，
# 不用 `readlink -f`（旧版 macOS/BSD 不支持）。只对已存在的目录调用。
realpath_portable() {
  perl -MCwd=realpath -e 'my $p = realpath($ARGV[0]); defined $p or exit 1; print $p' -- "$1"
}

# 扫描 root 下全部 YAML/JSON 源：$ref 只能指向 root 内已存在的普通文件（#/... 纯片段引用跳过）。
# 返回 0=全部合法；1=存在违规（逐条打印原因）。
scan_refs() {
  local root="$1" root_abs f dir_abs ref target resolved rc=0 original physical_dir physical
  # root 必须真实存在，用 cd+pwd -P 取物理绝对路径（跨平台，不依赖 realpath -m）。
  root_abs=$(cd "$root" && pwd -P) || return 1
  while IFS= read -r -d '' f; do
    # 源文件本体是符号链接：`find -type f` 不会返回链接（它不是普通文件），于是
    # 链接指向的树外 payload 既不参与任何 $ref 扫描，入口文档本身又会被 redocly
    # 跟随——整份源文件就此绕过本脚本。策略与 $ref 目标一致：一律拒绝，不解引用。
    if [ -L "$f" ]; then
      echo "FAIL: $f: OpenAPI 源文件是符号链接（禁止跟随）：$f" >&2
      rc=1
      continue
    fi
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
      # 第一道：词法解析后做前缀包含判断（目标即使不存在也能判定是否逃出 root）。
      resolved=$(normalize_path "$dir_abs/$target")
      case "$resolved" in
        "$root_abs"/*) ;;
        *)
          echo "FAIL: $f: \$ref 逃出 $root（解析为 $resolved）：$ref" >&2
          rc=1
          continue
          ;;
      esac
      # 第二道：对「原始 join 路径」的父目录做物理解析，再校验前缀。
      # 必须在词法归一化之外保留原始路径：`link/../X` 会先跟随 link 再回退，
      # 词法归一化把 link 抹成 `..`，恰好掩盖「链接组件 + ..」的逃逸。
      original="$dir_abs/$target"
      if [ ! -d "$(dirname "$original")" ]; then
        echo "FAIL: $f: \$ref 目标不存在：$ref → $resolved" >&2
        rc=1
        continue
      fi
      physical_dir=$(realpath_portable "$(dirname "$original")") || physical_dir=""
      if [ -z "$physical_dir" ]; then
        echo "FAIL: $f: \$ref 目标目录不可解析：$ref → $original" >&2
        rc=1
        continue
      fi
      physical="$physical_dir/$(basename "$original")"
      case "$physical" in
        "$root_abs"/*) ;;
        *)
          echo "FAIL: $f: \$ref 经符号链接逃出 $root（物理路径 $physical）：$ref" >&2
          rc=1
          continue
          ;;
      esac
      # 第三道：目标必须是普通文件本体。符号链接一律拒绝——bundle 会跟随链接读取
      # 树外内容，而目录级检查拦不住「文件本身是链接」这一形态。
      if [ -L "$physical" ]; then
        echo "FAIL: $f: \$ref 目标是符号链接（禁止跟随）：$ref → $physical" >&2
        rc=1
        continue
      fi
      if [ ! -f "$physical" ]; then
        echo "FAIL: $f: \$ref 目标不存在：$ref → $physical" >&2
        rc=1
      fi
    done < <(extract_refs "$f")
  done < <(find "$root" \( -type f -o -type l \) \
    \( -name '*.yaml' -o -name '*.yml' -o -name '*.json' \) -print0)
  return "$rc"
}

# ---------------------------------------------------------------------------
# 权限门禁静态断言（area4 Minor 15）
# ---------------------------------------------------------------------------

# 从 rbac 单一真源提取权限键。只取 `PermXxx = "..."` 形式的字符串常量；
# 目录是安全边界，解析失败必须 fail-closed（调用方断言数量 == 20）。
catalog_permissions() { # catalog_permissions <catalog.go>
  sed -nE 's/^[[:space:]]*Perm[A-Za-z0-9_]+[[:space:]]*=[[:space:]]*"([^"]+)"[[:space:]]*$/\1/p' "$1"
}

# 断言 1：spec 中每个 x-required-permission 取值都必须在 20 键目录内。
# 为什么比对 Go 常量而不是 handoff 文档：catalog.go 是运行时鉴权的唯一真源，
# 文档表格只是它的镜像；契约 enum（PermissionKey）与真源漂移时，只有这里能发现。
check_permission_catalog() { # check_permission_catalog <openapi_root> <catalog.go>
  local root="$1" catalog="$2" rc=0 perms n key
  perms="$(catalog_permissions "$catalog")"
  n="$(printf '%s\n' "$perms" | grep -c . || true)"
  if [ "$n" -ne 20 ]; then
    echo "FAIL: RBAC 权限目录解析得到 ${n} 个键（期望 20）：$catalog" >&2
    return 1
  fi
  while IFS= read -r key; do
    [ -n "$key" ] || continue
    if ! printf '%s\n' "$perms" | grep -qxF -- "$key"; then
      echo "FAIL: x-required-permission 取值不在 RBAC 20 键目录内：$key" >&2
      rc=1
    fi
  done < <(grep -hE '^[[:space:]]+x-required-permission:' "$root"/paths/*.yaml \
    | sed -E 's/^[[:space:]]*x-required-permission:[[:space:]]*//; s/[[:space:]]+#.*$//')
  return "$rc"
}

# 输出未声明 x-required-permission 的 admin operation（file:line），无违规时为空。
# 用 4 空格缩进的 HTTP 方法行切分 operation；tags/x-required-permission 取 6 空格
# 缩进字段。只针对仓内自有 YAML 的固定排版，解析不出 admin operation 时由调用方
# 以「计数为 0」fail-closed，避免排版变化把断言悄悄变成永远 PASS。
admin_ops_missing_permission() { # admin_ops_missing_permission <openapi_root>
  local root="$1" f
  for f in "$root"/paths/*.yaml; do
    [ -f "$f" ] || continue
    awk -v file="$f" '
      function finish() {
        if (in_op && is_admin && !has_perm) {
          printf "%s:%d: admin operation 缺少 x-required-permission\n", file, start
        }
      }
      /^    (get|put|post|patch|delete|head|options|trace):[[:space:]]*$/ {
        finish(); in_op=1; is_admin=0; has_perm=0; start=NR; next
      }
      in_op && /^      tags:[[:space:]]*\[admin\][[:space:]]*$/ { is_admin=1 }
      in_op && /^      x-required-permission:/ { has_perm=1 }
      END { finish() }
    ' "$f"
  done
}

# admin operation 总数（每 operation 只有一行 tags）。
admin_operation_count() { # admin_operation_count <openapi_root>
  local root="$1" f total=0 n
  for f in "$root"/paths/*.yaml; do
    [ -f "$f" ] || continue
    n="$(awk '/^      tags:[[:space:]]*\[admin\][[:space:]]*$/ { c++ } END { print c+0 }' "$f")"
    total=$((total + n))
  done
  printf '%s\n' "$total"
}

# 断言 2：每个 admin operation 必须声明 x-required-permission。
check_admin_permission_required() { # check_admin_permission_required <openapi_root>
  local root="$1" out count
  count="$(admin_operation_count "$root")"
  if [ "$count" -eq 0 ]; then
    echo "FAIL: 未解析到任何 admin operation（YAML 排版变化？断言 fail-closed）" >&2
    return 1
  fi
  out="$(admin_ops_missing_permission "$root")"
  if [ -n "$out" ]; then
    printf '%s\n' "$out" >&2
    return 1
  fi
  echo "PASS: ${count} 个 admin operation 均声明 x-required-permission"
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
  # 单引号键同样是合法 YAML；只匹配双引号会让 `'$ref' :` 静默绕过整道边界检查。
  run_case escape-single 逃出 "'\$ref' : '../outside.yaml#/schemas/Escape'"
  # 普通写法的一级越界（../ 从 api/openapi 到 api/，已在 root 外）。
  run_case escape-plain 逃出 "\$ref: '../outside.yaml#/schemas/Escape'"
  # http(s) 引用会把远端内容注入 bundle 与下游 codegen（供应链面）。
  run_case network 禁止网络 "\$ref: 'https://evil.example/schemas.yaml#/X'"
  # 绝对路径可读取宿主任意文件（文件泄露面）。
  run_case absolute 禁止绝对路径 "\$ref: '/etc/passwd#/X'"
  # 路径在树内但目标不存在：bundle 会在解析阶段才失败，必须提前拦住。
  run_case missing 目标不存在 "\$ref: './paths/does-not-exist.yaml#/X'"
  # 值在键的下一行（YAML 合法）：按行提取会看不到，必须由多行状态机覆盖。
  run_case multiline 逃出 $'$ref:\n    ../outside.yaml#/X'
  # 键行带行尾注释同样是合法 YAML（PyYAML 解析出的值在下一行）。只认「冒号后直接
  # 换行」会让整条引用逃过校验；同行 grep 捕获的裸 `#` 也不能被当作片段放行。
  run_case multiline-comment 逃出 $'$ref: # 注释\n    ../outside.yaml#/X'

  # 文件符号链接：词法/目录物理检查都能过，但链接本体指向树外，必须拒绝。
  printf 'x-outside: 1\n' >"$tmp/outside-file.yaml"
  ln -s "$tmp/outside-file.yaml" "$tmp/api/openapi/selftest-link.yaml"
  printf 'x-selftest:\n  $ref: '\''./selftest-link.yaml#/X'\''\n' \
    >"$tmp/api/openapi/selftest-symlink-file.yaml"
  if out=$(scan_refs "$tmp/api/openapi" 2>&1); then
    echo "FAIL: 自检 [symlink-file] 未被拒绝" >&2
    rc=1
  elif ! printf '%s' "$out" | grep -qF -- '符号链接'; then
    echo "FAIL: 自检 [symlink-file] 拒绝原因不符：$out" >&2
    rc=1
  else
    echo "PASS: 自检 [symlink-file] 已被拒绝（符号链接）"
  fi
  rm -f "$tmp/api/openapi/selftest-link.yaml" "$tmp/api/openapi/selftest-symlink-file.yaml"

  # 目录符号链接：链接本体不是 -L 目标，只有物理解析父目录才能发现已出树。
  mkdir -p "$tmp/outside-dir"
  printf 'x-outside: 1\n' >"$tmp/outside-dir/target.yaml"
  ln -s "$tmp/outside-dir" "$tmp/api/openapi/selftest-linkdir"
  printf 'x-selftest:\n  $ref: '\''./selftest-linkdir/target.yaml#/X'\''\n' \
    >"$tmp/api/openapi/selftest-symlink-dir.yaml"
  if out=$(scan_refs "$tmp/api/openapi" 2>&1); then
    echo "FAIL: 自检 [symlink-dir] 未被拒绝" >&2
    rc=1
  elif ! printf '%s' "$out" | grep -qF -- '逃出'; then
    echo "FAIL: 自检 [symlink-dir] 拒绝原因不符：$out" >&2
    rc=1
  else
    echo "PASS: 自检 [symlink-dir] 已被拒绝（逃出）"
  fi
  rm -f "$tmp/api/openapi/selftest-linkdir" "$tmp/api/openapi/selftest-symlink-dir.yaml"

  # 链接组件 + `..`：词法归一化会把 `link/..` 抹成父目录，必须用原始路径的
  # 父目录做物理解析，才能发现实际已跟随 link 出树。
  mkdir -p "$tmp/outside-dir"
  printf 'x-outside: 1\n' >"$tmp/victim.yaml"
  ln -s "$tmp/outside-dir" "$tmp/api/openapi/selftest-linkdot"
  printf 'x-selftest:\n  $ref: '\''./selftest-linkdot/../victim.yaml#/X'\''\n' \
    >"$tmp/api/openapi/selftest-symlink-dotdot.yaml"
  if out=$(scan_refs "$tmp/api/openapi" 2>&1); then
    echo "FAIL: 自检 [symlink-dotdot] 未被拒绝" >&2
    rc=1
  elif ! printf '%s' "$out" | grep -qF -- '逃出'; then
    echo "FAIL: 自检 [symlink-dotdot] 拒绝原因不符：$out" >&2
    rc=1
  else
    echo "PASS: 自检 [symlink-dotdot] 已被拒绝（逃出）"
  fi
  rm -f "$tmp/api/openapi/selftest-linkdot" "$tmp/api/openapi/selftest-symlink-dotdot.yaml"

  # 源文件符号链接：旧 find 用 -type f，链接本体不会被枚举，链接指向的树外文档
  # 因而从未进入 $ref 扫描（redocly 却会跟随）。与 $ref 目标同策略：直接拒绝。
  printf 'x-outside: 1\n' >"$tmp/outside-source.yaml"
  ln -s "$tmp/outside-source.yaml" "$tmp/api/openapi/selftest-source-link.yaml"
  if out=$(scan_refs "$tmp/api/openapi" 2>&1); then
    echo "FAIL: 自检 [symlink-source] 未被拒绝" >&2
    rc=1
  elif ! printf '%s' "$out" | grep -qF -- '符号链接'; then
    echo "FAIL: 自检 [symlink-source] 拒绝原因不符：$out" >&2
    rc=1
  else
    echo "PASS: 自检 [symlink-source] 已被拒绝（符号链接）"
  fi
  rm -f "$tmp/api/openapi/selftest-source-link.yaml"

  # 权限断言自检：只验证真实源树通过，无法区分「守卫有效」与「规则退化成永远
  # PASS」。逐条注入后必须按预期原因失败，且随后从真实源恢复副本。
  # 用例 A：删除一条 x-required-permission → 管理面断言必须失败。
  cp api/openapi/paths/admin.yaml "$tmp/api/openapi/paths/admin.yaml"
  perl -0pi -e 's/^[ \t]+x-required-permission:[^\n]*\n//m' "$tmp/api/openapi/paths/admin.yaml"
  if out=$(check_admin_permission_required "$tmp/api/openapi" 2>&1); then
    echo "FAIL: 自检 [admin-permission-missing] 未被拒绝" >&2
    rc=1
  elif ! printf '%s' "$out" | grep -qF -- '缺少 x-required-permission'; then
    echo "FAIL: 自检 [admin-permission-missing] 拒绝原因不符：$out" >&2
    rc=1
  else
    echo "PASS: 自检 [admin-permission-missing] 已被拒绝"
  fi

  # 用例 B：把权限值改成目录外的拼写 → 目录断言必须失败。
  cp api/openapi/paths/admin.yaml "$tmp/api/openapi/paths/admin.yaml"
  sed -i 's/users:read/users:root/' "$tmp/api/openapi/paths/admin.yaml"
  if out=$(check_permission_catalog "$tmp/api/openapi" backend/internal/rbac/catalog.go 2>&1); then
    echo "FAIL: 自检 [permission-not-in-catalog] 未被拒绝" >&2
    rc=1
  elif ! printf '%s' "$out" | grep -qF -- '不在 RBAC 20 键目录内'; then
    echo "FAIL: 自检 [permission-not-in-catalog] 拒绝原因不符：$out" >&2
    rc=1
  else
    echo "PASS: 自检 [permission-not-in-catalog] 已被拒绝"
  fi
  # 恢复真实副本，避免影响后续用例与最终状态。
  cp api/openapi/paths/admin.yaml "$tmp/api/openapi/paths/admin.yaml"

  [ "$rc" -eq 0 ] && echo "PASS: 自检——越界（单/双引号、../、多行值、多行值+键行注释）、网络、绝对路径、缺失目标、符号链接（源文件、\$ref 目标、目录、链接+..）、权限（缺失声明、目录外取值）注入均已被拒绝"
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
#    键匹配只认 `$ref` 子串而不写死 `$ref:`：单/双引号键（`"$ref":`、`'$ref' :`）
#    同样是合法 YAML，写死冒号会把合规写法误判成内联 path（fail-closed 误报）。
awk '
  /^paths:[[:space:]]*$/ { inpaths = 1; next }
  inpaths && /^[^[:space:]#]/ { inpaths = 0 }
  inpaths && /^  \// { paths++ }
  inpaths && /\$ref/ { refs++ }
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

# 7) 权限门禁：x-required-permission 取值 ⊆ rbac 20 键目录；admin operation 必须声明。
#    目录文件缺失即失败——无目录可比对时放行会让断言退化成永远 PASS。
RBAC_CATALOG=backend/internal/rbac/catalog.go
[ -f "$RBAC_CATALOG" ] || fail "缺少 RBAC 权限目录 $RBAC_CATALOG（权限断言 fail-closed）"
check_permission_catalog api/openapi "$RBAC_CATALOG" \
  || fail "x-required-permission 取值与 RBAC 20 键目录不一致"
check_admin_permission_required api/openapi \
  || fail "管理面 operation 缺少 x-required-permission 声明"

# 8) relay schema 源文件存在但不承载协议 schema（§9.3：真源在 contract/adapter）
grep -q 'x-codegen: false' api/openapi/paths/relay.yaml || fail "relay paths 必须打 x-codegen: false"

echo "PASS: OpenAPI layout 校验通过（仓根 api/openapi/，入口 paths 全为 \$ref，\$ref 未越界，权限声明与 RBAC 目录一致）"
