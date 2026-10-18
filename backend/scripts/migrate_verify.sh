#!/usr/bin/env bash
# migrate_verify.sh —— golang-migrate 迁移全序列验证（真实 PG）
#
# 依据 docs/platform-02 §8.3 与 docs/platform-04 §9.2–§9.5：
#   1. 全新库 up（第二次 up 必须输出 no change）
#   2. version == 该迁移目录中最大的 14 位时间戳（golang-migrate 的版本即时间戳，不是 1）
#   3. down 1 && up 1 后版本回到原值
#   4. down -all 后 public 下除 schema_migrations 外无任何表
#   5. up 后 pg_dump --schema-only --no-owner --no-privileges（normalize 后）与首次 dump diff 为空
#   6. schema_migrations.dirty = false
#
# 用法：
#   bash backend/scripts/migrate_verify.sh <dsn> <logs_dsn>
#
# 破坏性：脚本会对目标库执行 down -all（并在库非空时拒绝运行）。
# 只允许指向一次性的验证库；CI 的 service container 正合此用途。
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MIGRATIONS_DIR="$SCRIPT_DIR/../migrations"
LOGS_MIGRATIONS_DIR="$SCRIPT_DIR/../migrations-logs"

MIGRATE="${MIGRATE_BIN:-migrate}"
PSQL="${PSQL:-psql}"
PG_DUMP="${PG_DUMP:-pg_dump}"

log()  { echo "==> $*"; }
die()  { echo "FAIL  $*" >&2; exit 1; }

if [ "$#" -ne 2 ]; then
  echo "用法: $0 <dsn> <logs_dsn>" >&2
  exit 2
fi
DSN="$1"
LOGS_DSN="$2"

command -v "$MIGRATE" >/dev/null 2>&1 || die "找不到 migrate 可执行文件：$MIGRATE（可用 MIGRATE_BIN 指定）"
command -v "$PSQL"    >/dev/null 2>&1 || die "找不到 psql：$PSQL（可用 PSQL 指定）"
command -v "$PG_DUMP" >/dev/null 2>&1 || die "找不到 pg_dump：$PG_DUMP（可用 PG_DUMP 指定）"
[ -d "$MIGRATIONS_DIR" ]      || die "迁移目录不存在：$MIGRATIONS_DIR"
[ -d "$LOGS_MIGRATIONS_DIR" ] || die "日志库迁移目录不存在：$LOGS_MIGRATIONS_DIR"

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/newtg-migrate-verify.XXXXXX")"
trap 'rm -rf "$TMP_DIR"' EXIT

migrate_db() { # migrate_db <dir> <dsn> <args...>
  local dir="$1" dsn="$2"
  shift 2
  "$MIGRATE" -path "$dir" -database "$dsn" "$@"
}

expected_version() { # expected_version <dir> —— 最大的 14 位时间戳前缀
  local dir="$1" v
  v="$(find "$dir" -maxdepth 1 -type f -name '*.up.sql' -print 2>/dev/null \
        | sed -nE 's#.*/([0-9]{14})_[a-z0-9_]+\.up\.sql$#\1#p' \
        | sort | tail -n 1)"
  [ -n "$v" ] || die "目录中没有合法命名的 *.up.sql：$dir"
  printf '%s' "$v"
}

get_version() { # get_version <dir> <dsn> —— 输出版本号（脏状态去掉 "(dirty)"）
  local dir="$1" dsn="$2" out
  out="$(migrate_db "$dir" "$dsn" version 2>&1)" || true
  printf '%s\n' "$out" | head -n 1 | awk '{print $1}'
}

table_count() { # table_count <dsn> —— public 下除 schema_migrations 外的表数
  local dsn="$1" n
  n="$("$PSQL" "$dsn" -tAc \
    "SELECT count(*) FROM information_schema.tables
      WHERE table_schema = 'public' AND table_name <> 'schema_migrations'")" \
    || die "无法查询 information_schema.tables：$dsn"
  printf '%s' "$n" | tr -d '[:space:]'
}

normalize_dump() { # normalize_dump <file> —— 去注释/SET/随机 restrict 标记/空行/行尾空白
  sed -E \
    -e '/^--/d' \
    -e '/^SET /d' \
    -e '/^\\(un)?restrict /d' \
    -e 's/[[:space:]]+$//' \
    -e '/^$/d' \
    "$1"
}

verify_one() { # verify_one <标签> <迁移目录> <dsn>
  local label="$1" dir="$2" dsn="$3"
  local expected out version before_norm after_norm n dirty

  log "[$label] 目录 $dir"
  expected="$(expected_version "$dir")"
  log "[$label] 期望版本（目录最大 14 位时间戳）：$expected"

  "$PSQL" "$dsn" -tAc 'SELECT 1' >/dev/null 2>&1 \
    || die "[$label] 无法连接数据库：$dsn"

  # 起点必须是干净的一次性库：若上次运行留下 schema_migrations，先全退归零。
  if "$PSQL" "$dsn" -tAc "SELECT to_regclass('public.schema_migrations') IS NOT NULL" 2>/dev/null | grep -qx t; then
    log "[$label] 检测到既有迁移状态，先执行 down -all 归零"
    if ! out="$(migrate_db "$dir" "$dsn" down -all 2>&1)"; then
      printf '%s\n' "$out" | grep -qi 'no migration' \
        || die "[$label] 重置（down -all）失败：$out"
    fi
  fi
  n="$(table_count "$dsn")"
  [ "$n" = "0" ] \
    || die "[$label] 目标库 public 下仍有 $n 个非 schema_migrations 表；migrate-verify 只允许在专用一次性库上运行"

  # 1) 全新库前滚
  log "[$label] 1/9 up（首次前滚）"
  out="$(migrate_db "$dir" "$dsn" up 2>&1)" || die "[$label] 首次 up 失败：$out"

  # 2) 幂等：第二次 up 必须报 no change
  log "[$label] 2/9 up（幂等检查）"
  out="$(migrate_db "$dir" "$dsn" up 2>&1)" || die "[$label] 第二次 up 失败：$out"
  printf '%s\n' "$out" | grep -qi 'no change' \
    || die "[$label] 第二次 up 未输出 no change（幂等被破坏）：$out"

  # 3) 版本 == 最大 14 位时间戳
  log "[$label] 3/9 version 断言"
  version="$(get_version "$dir" "$dsn")"
  [ "$version" = "$expected" ] \
    || die "[$label] version=$version，期望 $expected"

  # 4) 单步往返 down 1 -> up 1
  log "[$label] 4/9 down 1 && up 1"
  out="$(migrate_db "$dir" "$dsn" down 1 2>&1)" || die "[$label] down 1 失败：$out"
  out="$(migrate_db "$dir" "$dsn" up 1 2>&1)"   || die "[$label] up 1 失败：$out"
  version="$(get_version "$dir" "$dsn")"
  [ "$version" = "$expected" ] \
    || die "[$label] 往返后 version=$version，期望 $expected"

  # 首次 schema 快照（全退之前）
  log "[$label] 5/9 pg_dump 首次快照"
  "$PG_DUMP" --schema-only --no-owner --no-privileges "$dsn" > "$TMP_DIR/$label.before.sql" \
    || die "[$label] 首次 pg_dump 失败"
  before_norm="$TMP_DIR/$label.before.norm.sql"
  normalize_dump "$TMP_DIR/$label.before.sql" > "$before_norm"

  # 5) 全退，除 schema_migrations 外无残留表
  log "[$label] 6/9 down -all 并断言无残留表"
  out="$(migrate_db "$dir" "$dsn" down -all 2>&1)" || die "[$label] down -all 失败：$out"
  n="$(table_count "$dsn")"
  [ "$n" = "0" ] \
    || die "[$label] down -all 后 public 下仍有 $n 个非 schema_migrations 表（回退不完整）"

  # 6) 再次前滚
  log "[$label] 7/9 up（全退后前滚）"
  out="$(migrate_db "$dir" "$dsn" up 2>&1)" || die "[$label] 全退后 up 失败：$out"
  version="$(get_version "$dir" "$dsn")"
  [ "$version" = "$expected" ] \
    || die "[$label] 全退前滚后 version=$version，期望 $expected"

  # 7) 对象集一致：normalize 后 diff 为空
  log "[$label] 8/9 pg_dump 二次快照并 diff"
  "$PG_DUMP" --schema-only --no-owner --no-privileges "$dsn" > "$TMP_DIR/$label.after.sql" \
    || die "[$label] 二次 pg_dump 失败"
  after_norm="$TMP_DIR/$label.after.norm.sql"
  normalize_dump "$TMP_DIR/$label.after.sql" > "$after_norm"
  if ! diff -u "$before_norm" "$after_norm" > "$TMP_DIR/$label.diff"; then
    cat "$TMP_DIR/$label.diff" >&2
    die "[$label] 全退再前滚后 schema 快照不一致"
  fi

  # 8) dirty=false
  log "[$label] 9/9 dirty=false 断言"
  dirty="$("$PSQL" "$dsn" -tAc 'SELECT dirty FROM schema_migrations' 2>/dev/null | tr -d '[:space:]')"
  [ "$dirty" = "f" ] \
    || die "[$label] schema_migrations.dirty=$dirty，期望 f"

  log "[$label] PASS"
}

verify_one "main" "$MIGRATIONS_DIR" "$DSN"
verify_one "logs" "$LOGS_MIGRATIONS_DIR" "$LOGS_DSN"

echo
echo "migrate_verify: 全部序列通过（main + logs）"
