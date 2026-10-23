#!/usr/bin/env bash
# audit-copy-guards.sh —— 文案与品牌守卫（2026-10-23 审核改进计划 阶段 4 / WS6）
#
# 为什么需要这个脚本：以下问题都不会让构建失败，却会直接出现在用户界面上。
# 用机械 grep 门禁兜住，避免文案回归：
#   1. 用户可见词典里出现内部模块编号（`模块 N`）；
#   2. 品牌大小写漂移（精确匹配 `NewTG`；合规写法是 `NEWTG`）。
#      例外：`X-NewTG-Signature` 是 HMAC 线协议头名，属于受保护的代码契约，
#      包含它的整行允许通过，也绝不能重命名；
#   3. 首页文案里未经证实的规模/性能数字（`100+`、`10,000+`、`99.9%`、
#      `10ms`、`注册即送`）；
#   4. 词典里残留的旧版本表述（`本版本不可用`、`本版本尚未提供`）。
#
# 用法：bash scripts/audit-copy-guards.sh
# 退出码：0 通过；1 存在违规。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

LOCALES_DIR="frontend/src/locales"
COPY_DIR="website/src/config/copy"
INDEX_HTML="frontend/index.html"

# 依赖目录、构建产物与压缩包不属于源码文案，递归搜索时一律忽略。
EXCLUDES=(
  --exclude-dir=node_modules
  --exclude-dir=dist
  --exclude-dir=out
  --exclude-dir=.git
  --exclude='*.min.js'
  --exclude='*.bundle.js'
)

for dir in "$LOCALES_DIR" "$COPY_DIR"; do
  if [ ! -d "$dir" ]; then
    echo "FAIL 找不到目录：$dir" >&2
    exit 1
  fi
done
if [ ! -f "$INDEX_HTML" ]; then
  echo "FAIL 找不到文件：$INDEX_HTML" >&2
  exit 1
fi

failed=0

report() {
  local title="$1"
  local results="$2"
  if [ -n "$results" ]; then
    echo "FAIL $title" >&2
    printf '%s\n' "$results" >&2
    failed=1
  fi
}

# 在 `set -e` 下执行 grep 并区分两种非零退出：
#   1 → 没有匹配（通过）；
#   >1 → 读取/权限/语法错误（必须红灯，不能被当成“无匹配”放过）。
# 用法：run_grep "<title>" grep <args...>（命令参数原样传给它）。
run_grep() {
  local title="$1"
  shift
  local results status
  set +e
  results="$("$@")"
  status=$?
  set -e
  if [ "$status" -gt 1 ]; then
    echo "FAIL $title" >&2
    echo "grep 退出码 $status：读取或权限错误（不是“无匹配”），守卫未生效" >&2
    failed=1
    return
  fi
  if [ "$status" -eq 0 ]; then
    report "$title" "$results"
  fi
}

# 1. 内部模块编号（所有已翻译写法，大小写不敏感）
# 例：`模块 06`、`module 06`、`Modul 6`、`módulo 6`、`モジュール6`、`mô-đun 06`。
# 数字前只允许一个可选空格与可选前导 0，避免匹配 `modules` 等普通单词。
MODULE_PATTERN='(模块[ ]?0?[0-9]|module[ ]?0?[0-9]|modul[ ]?0?[0-9]|módulo[ ]?0?[0-9]|モジュール[ ]?0?[0-9]|mô-đun[ ]?0?[0-9])'
run_grep \
  "1) 用户可见词典出现内部模块编号；应改为面向用户的功能描述（模块/module/Modul/módulo/モジュール/mô-đun N）" \
  grep -RIniE "${EXCLUDES[@]}" --include='*.json' -- "$MODULE_PATTERN" "$LOCALES_DIR"

# 2. 品牌大小写：精确匹配 NewTG（NEWTG 合规），放行 X-NewTG-Signature 所在行。
# 先把主 grep 的结果与退出码取出来，才能在过滤受保护行之前识别读取错误
# （管道会把第一个 grep 的退出码吞掉）。
set +e
brand_all="$(grep -RInE "${EXCLUDES[@]}" --include='*.json' --include='*.ts' --include='*.html' \
  -- 'NewTG' "$LOCALES_DIR" "$COPY_DIR" "$INDEX_HTML")"
brand_status=$?
set -e
if [ "$brand_status" -gt 1 ]; then
  echo "FAIL 2) 品牌大小写应为 NEWTG（已放行 X-NewTG-Signature 线协议头名）" >&2
  echo "grep 退出码 $brand_status：读取或权限错误（不是“无匹配”），守卫未生效" >&2
  failed=1
elif [ -n "$brand_all" ]; then
  brand_hits="$(printf '%s\n' "$brand_all" | grep -v -- 'X-NewTG-Signature' || true)"
  report "2) 品牌大小写应为 NEWTG（已放行 X-NewTG-Signature 线协议头名）" "$brand_hits"
fi

# 3. 未经证实的营销数字
run_grep \
  "3) 首页文案含未经证实的规模/性能数字" \
  grep -RInE "${EXCLUDES[@]}" --include='*.ts' \
  -- '100\+|10,000\+|99\.9%|10ms|注册即送' "$COPY_DIR"

# 4. 旧版本表述
run_grep \
  "4) 词典残留“本版本不可用 / 本版本尚未提供”" \
  grep -RInE "${EXCLUDES[@]}" --include='*.json' \
  -- '本版本不可用|本版本尚未提供' "$LOCALES_DIR"

if [ "$failed" -ne 0 ]; then
  echo "copy guards failed" >&2
  exit 1
fi

echo "copy guards passed"
