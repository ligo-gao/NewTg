#!/usr/bin/env bash
# check_migrations.sh —— 迁移文件静态断言（无需 DB）
#
# 断言来源：docs/platform-04-数据模型迁移-设计.md §9.6、§9.7 与 2026-10-18 裁决 GC-5：
#   (a) 每个 up 有成对 down
#   (b) 无孤儿 down
#   (c) 命名合规：{14 位 UTC 时间戳}_{snake_case}.{up|down}.sql
#   (d) 含 CONCURRENTLY 的 up 文件必须单语句（M0 应为零命中；命中即违规）
#   (e) 禁止默认分区：既查历史写法 DEFAULT PARTITION，也查真实语法
#       PARTITION OF <parent> ... DEFAULT（裁决 V11：落入未建月份必须失败）
#   (f) request_registry 的主键必须恰为 request_id（V9 幂等真源）；主库迁移流
#       必须定义该表，未识别为日志库流的目录一律 fail-closed（见该节注释）
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
# SQL 清洗依赖 perl（块注释 / 字面量一次处理）；缺失时不能降级成只 grep 行注释，
# 那会让 /* ... */ 里的 DDL 重新变成绕过面。
command -v perl >/dev/null 2>&1 || {
  echo "FAIL  check_migrations.sh 需要 perl 做 SQL 注释/字面量清洗（required for this check）" >&2
  exit 1
}
DIR="${DIR%/}"
echo "check_migrations: 检查目录 $DIR"

shopt -s nullglob
ups=("$DIR"/*.up.sql)
downs=("$DIR"/*.down.sql)
all=("$DIR"/*.sql)

# 结构匹配前的 SQL 清洗（perl 一次完成，避免多趟 sed 在注释/字面量交界处误伤）：
#   1. 去掉 /* */ 块注释与 -- 行注释——注释里的 DDL 不能算数；旧实现只处理行注释，
#      `/* CREATE TABLE request_registry (... PRIMARY KEY (request_id)) */` 能满足 (f)；
#   2. 把单引号字符串字面量替换为空串——字面量里的 `;`/`DEFAULT` 不是 SQL 结构；
#      否则 `PARTITION OF ... (note text DEFAULT 'a;b') DEFAULT;` 会因按 ; 断句而漏检。
# 局限（如实声明）：DO/EXECUTE 以字符串拼出的动态 SQL 不参与结构匹配；本仓库的
# 分区 DDL 是静态语句，评审时动态建分区也不得使用 DEFAULT。
sql_body() {
  perl -0777 -pe "s{/\*.*?\*/}{}gs; s/'(?:[^']|'')*'/''/gs; s/--[^\\n]*//g" "$1"
}

# CREATE TABLE [IF NOT EXISTS] —— 主库迁移可能用幂等写法，标记匹配必须同时覆盖两种。
create_table_re='CREATE TABLE[[:space:]]+(IF[[:space:]]+NOT[[:space:]]+EXISTS[[:space:]]+)?'

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
    n="$(sql_body "$f" | grep -o ';' | wc -l | tr -d '[:space:]')"
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

# (e) 禁止默认分区（任何 .sql，含 down）
#     PostgreSQL 的默认分区真实语法是 `CREATE TABLE ... PARTITION OF <parent> DEFAULT`
#     ——DEFAULT 位于语句尾部（可能换行，中间还可能有 FOR VALUES 之外的列定义）。
#     只 grep 历史写法 'DEFAULT PARTITION' 会漏掉它，因此先去注释、压平换行并按 ;
#     断句，再整句匹配；DEFAULT 后要求是语句结尾，避免把列 DEFAULT 误判为默认分区。
default_partition=0
for f in "${all[@]}"; do
  if sql_body "$f" | tr '\n' ' ' | sed -E 's/;[[:space:]]*/;\n/g' \
     | grep -Eqi 'DEFAULT[[:space:]]+PARTITION|PARTITION[[:space:]]+OF[^;]*[[:space:]]DEFAULT[[:space:]]*(;|$)'; then
    violation "禁止默认分区（DEFAULT PARTITION / PARTITION OF ... DEFAULT）：$(basename "$f")"
    default_partition=$((default_partition + 1))
  fi
done
[ "$default_partition" -eq 0 ] && pass "(e) 无默认分区（含 PARTITION OF ... DEFAULT 语法）"

# (f) request_registry 主键恰为 request_id
#     主库迁移流必须定义该表。旧实现以「没有 users 标记」推断日志库并直接跳过，
#     这是 fail-open：主库目录里 users/request_registry 的建表若被改写（例如加了
#     IF NOT EXISTS 或整体被注释），(f) 会静默 PASS。现改为正向识别日志库流：
#     目录名 migrations-logs，或（目录被复制/改名时）同时建出日志库锚表
#     usage_logs 与 audit_logs；其余任何目录缺少 request_registry 都判违规。
is_logs_stream() {
  local f
  case "$(basename "$DIR")" in
    migrations-logs) return 0 ;;
  esac
  for f in "${ups[@]}"; do
    if sql_body "$f" | grep -Eq "${create_table_re}usage_logs([[:space:]]|\\()" \
       && sql_body "$f" | grep -Eq "${create_table_re}audit_logs([[:space:]]|\\()"; then
      return 0
    fi
  done
  return 1
}

has_registry=0
has_table_ddl=0
for f in "${ups[@]}"; do
  if sql_body "$f" | grep -Eq 'CREATE TABLE[[:space:]]'; then
    has_table_ddl=1
  fi
  # 注释里的建表已在 sql_body 中剥离，不会让 (f) 误判为已定义。
  if sql_body "$f" | grep -Eq "${create_table_re}request_registry([[:space:]]|\\()"; then
    has_registry=1
    block="$(sql_body "$f" | awk '
      /CREATE TABLE[[:space:]]+(IF[[:space:]]+NOT[[:space:]]+EXISTS[[:space:]]+)?request_registry([[:space:]]|\()/ { inblk = 1 }
      inblk { print }
      inblk && /^[[:space:]]*\);/ { exit }
    ')"
    if printf '%s\n' "$block" | grep -Eqi 'PRIMARY[[:space:]]+KEY[[:space:]]*\([[:space:]]*request_id[[:space:]]*\)'; then
      pass "(f) request_registry 主键为 request_id（$(basename "$f")）"
    else
      violation "request_registry 主键必须恰为 (request_id)：$(basename "$f")"
    fi
  fi
done
if [ "$has_registry" -eq 0 ]; then
  if is_logs_stream; then
    pass "(f) 日志库迁移流（无 request_registry），跳过该断言"
  elif [ "$has_table_ddl" -eq 1 ]; then
    violation "(f) 目录含 CREATE TABLE 但未定义 request_registry（fail-closed：未识别为日志库流一律按主库校验）"
  else
    violation "(f) 目录未识别为日志库迁移流，且缺少 request_registry 建表（fail-closed）"
  fi
fi

echo
if [ "$fail" -gt 0 ]; then
  echo "check_migrations: $fail 项断言失败" >&2
  exit 1
fi
echo "check_migrations: 全部断言通过（${#all[@]} 个迁移文件）"
