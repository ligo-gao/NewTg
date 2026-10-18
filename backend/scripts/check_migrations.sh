#!/usr/bin/env bash
# check_migrations.sh —— 迁移文件静态断言（无需 DB）
#
# 断言来源：docs/platform-04-数据模型迁移-设计.md §9.6、§9.7 与 2026-10-18 裁决 GC-5：
#   (a) 每个 up 有成对 down
#   (b) 无孤儿 down
#   (c) 命名合规：{14 位 UTC 时间戳}_{snake_case}.{up|down}.sql
#   (d) 含 CONCURRENTLY 的 up 文件必须单语句（M0 应为零命中；命中即违规）
#   (e) 无 DEFAULT PARTITION（裁决 V11：落入未建月份必须失败，不得静默落默认分区）
#   (f) request_registry 的主键必须恰为 request_id（V9 幂等真源；仅主库迁移断言）
#
# 用法：
#   bash backend/scripts/check_migrations.sh [migrations_dir]
#   默认 ../migrations（相对本脚本）；主库与日志库两条迁移流各跑一次：
#     check_migrations.sh backend/migrations
#     check_migrations.sh backend/migrations-logs
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIR="${1:-$SCRIPT_DIR/../migrations}"
fail=0

pass() { echo "PASS  $1"; }
violation() { echo "FAIL  $1" >&2; fail=$((fail + 1)); }

if [ ! -d "$DIR" ]; then
  echo "FAIL  迁移目录不存在：$DIR" >&2
  echo
  echo "check_migrations: 断言失败" >&2
  exit 1
fi
DIR="${DIR%/}"
echo "check_migrations: 检查目录 $DIR"

shopt -s nullglob
ups=("$DIR"/*.up.sql)
downs=("$DIR"/*.down.sql)
all=("$DIR"/*.sql)

if [ "${#all[@]}" -eq 0 ]; then
  violation "目录中没有任何 *.sql 迁移文件"
fi

# (a) 每个 up 必须有成对 down
missing_down=0
for f in "${ups[@]}"; do
  d="${f%.up.sql}.down.sql"
  if [ ! -f "$d" ]; then
    violation "孤儿 up（缺少配对 down）：$(basename "$f") -> 期望 $(basename "$d")"
    missing_down=$((missing_down + 1))
  fi
done
[ "$missing_down" -eq 0 ] && pass "(a) 每个 up 均有成对 down"

# (b) 无孤儿 down
orphan_down=0
for f in "${downs[@]}"; do
  u="${f%.down.sql}.up.sql"
  if [ ! -f "$u" ]; then
    violation "孤儿 down（缺少配对 up）：$(basename "$f") -> 期望 $(basename "$u")"
    orphan_down=$((orphan_down + 1))
  fi
done
[ "$orphan_down" -eq 0 ] && pass "(b) 无孤儿 down"

# (c) 命名合规
bad_name=0
for f in "${all[@]}"; do
  b="$(basename "$f")"
  if ! [[ "$b" =~ ^[0-9]{14}_[a-z0-9_]+\.(up|down)\.sql$ ]]; then
    violation "命名不合规：$b（要求 {14位时间戳}_{snake_case}.{up|down}.sql）"
    bad_name=$((bad_name + 1))
  fi
done
[ "$bad_name" -eq 0 ] && pass "(c) 命名合规（14 位时间戳 + snake_case）"

# (d) CONCURRENTLY 必须单语句（统计语句终止符 ; 的数量，忽略 -- 注释）
concurrent_hits=0
concurrent_bad=0
for f in "${ups[@]}"; do
  if grep -qi 'CONCURRENTLY' "$f"; then
    concurrent_hits=$((concurrent_hits + 1))
    n="$(sed -E 's/--.*$//' "$f" | grep -o ';' | wc -l | tr -d '[:space:]')"
    if [ "$n" -gt 1 ]; then
      violation "CONCURRENTLY 必须单语句，实际 $n 条语句：$(basename "$f")"
      concurrent_bad=$((concurrent_bad + 1))
    fi
  fi
done
if [ "$concurrent_hits" -eq 0 ]; then
  pass "(d) 无 CONCURRENTLY 命中（M0 期望）"
elif [ "$concurrent_bad" -eq 0 ]; then
  pass "(d) $concurrent_hits 个 CONCURRENTLY 文件均为单语句"
fi

# (e) 无 DEFAULT PARTITION（任何 .sql，含 down）
default_partition=0
for f in "${all[@]}"; do
  if grep -Eqi 'DEFAULT[[:space:]]+PARTITION' "$f"; then
    violation "禁止 DEFAULT PARTITION：$(basename "$f")"
    default_partition=$((default_partition + 1))
  fi
done
[ "$default_partition" -eq 0 ] && pass "(e) 无 DEFAULT PARTITION"

# (f) request_registry 主键恰为 request_id
#     主库迁移流必须定义该表；日志库迁移流（无 request_registry）跳过。
has_registry=0
for f in "${ups[@]}"; do
  if grep -Eq 'CREATE TABLE[[:space:]]+request_registry([[:space:]]|\()' "$f"; then
    has_registry=1
    block="$(awk '
      /CREATE TABLE[[:space:]]+request_registry([[:space:]]|\()/ { inblk = 1 }
      inblk { print }
      inblk && /^[[:space:]]*\);/ { exit }
    ' "$f")"
    if printf '%s\n' "$block" | grep -Eqi 'PRIMARY[[:space:]]+KEY[[:space:]]*\([[:space:]]*request_id[[:space:]]*\)'; then
      pass "(f) request_registry 主键为 request_id（$(basename "$f")）"
    else
      violation "request_registry 主键必须恰为 (request_id)：$(basename "$f")"
    fi
  fi
done
if [ "$has_registry" -eq 0 ]; then
  # 主库迁移流以 users 表为标志；没有 users 也没有 request_registry 视为日志库迁移流。
  main_marker=0
  for f in "${ups[@]}"; do
    grep -Eq 'CREATE TABLE[[:space:]]+users([[:space:]]|\()' "$f" && main_marker=1
  done
  if [ "$main_marker" -eq 1 ]; then
    violation "主库迁移流缺少 request_registry 建表语句"
  else
    pass "(f) 非主库迁移流，跳过 request_registry 断言"
  fi
fi

echo
if [ "$fail" -gt 0 ]; then
  echo "check_migrations: $fail 项断言失败" >&2
  exit 1
fi
echo "check_migrations: 全部断言通过（${#all[@]} 个迁移文件）"
