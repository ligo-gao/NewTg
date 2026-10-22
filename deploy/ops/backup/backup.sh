#!/usr/bin/env bash
# NewTG 主库 + 日志库全量备份。
#
# 设计依据：docs/platform-08-备份恢复与灾备演练-设计.md §3（备份策略）、
# §3.5（sidecar）、§5.1（轻校验 V1–V4）。本脚本只做“一次备份”，周期与
# GFS 分层（hourly/daily/monthly）由外部调度器负责（systemd timer / CronJob）。
#
# 产物（每个数据库一份）：
#   newtg-<UTC ts>[-label]-main.dump[.age]
#   newtg-<UTC ts>[-label]-main.dump[.age].sha256        # 单行 SHA-256
#   newtg-<UTC ts>[-label]-main.dump[.age].manifest.json
#   （日志库同名，后缀 -logs）
#
# 安全：
#   - DSN 只从环境变量/参数读取，不写入产物、不打印；
#   - 不写死任何口令；建议使用只读角色 newtg_backup；
#   - 可选 age 产生端加密（AGE_RECIPIENT / --encrypt-recipient），私钥不入包。
#
# 用法：backup.sh --help

set -euo pipefail
umask 077

usage() {
    cat <<'EOF'
NewTG 备份脚本

用法:
  backup.sh [选项]

DSN（必填，优先用环境变量，避免进入 shell history）:
  --main-dsn DSN          主库 DSN；默认 $NEWTG_MAIN_DSN 或 $DATABASE_URL
  --logs-dsn DSN          日志库 DSN；默认 $NEWTG_LOGS_DSN 或 $DB_LOGS_DSN
  --no-logs               只备份主库（不要求日志库 DSN）

输出:
  --out-dir DIR           产物目录；默认 $NEWTG_BACKUP_DIR 或 ./deploy/backups
  --label NAME            产物名附加标签（仅 [A-Za-z0-9._-]）
  --encrypt-recipient R   age 接收者公钥（如 age1...）；默认 $AGE_RECIPIENT
                          设置后产物为 .dump.age，明文不落盘

其他:
  -h, --help              显示本帮助

示例:
  export NEWTG_MAIN_DSN='postgres://newtg_backup:<password>@postgres:5432/newtg?sslmode=disable'
  export NEWTG_LOGS_DSN='postgres://newtg_backup:<password>@postgres:5432/newtg_logs?sslmode=disable'
  deploy/ops/backup/backup.sh --out-dir /backups
  deploy/ops/backup/backup.sh --encrypt-recipient "$AGE_RECIPIENT"

退出码:
  0 成功；1 参数/依赖/备份失败；2 用法错误。
EOF
}

die()  { printf 'backup: %s\n' "$*" >&2; exit 1; }
note() { printf '[backup] %s\n' "$*"; }

main_dsn="${NEWTG_MAIN_DSN:-${DATABASE_URL:-}}"
logs_dsn="${NEWTG_LOGS_DSN:-${DB_LOGS_DSN:-}}"
out_dir="${NEWTG_BACKUP_DIR:-./deploy/backups}"
label=""
recipient="${AGE_RECIPIENT:-}"
include_logs=1

while [ $# -gt 0 ]; do
    case "$1" in
        --main-dsn)
            [ $# -ge 2 ] || { printf 'backup: --main-dsn requires a value\n' >&2; exit 2; }
            main_dsn="$2"; shift 2 ;;
        --logs-dsn)
            [ $# -ge 2 ] || { printf 'backup: --logs-dsn requires a value\n' >&2; exit 2; }
            logs_dsn="$2"; shift 2 ;;
        --out-dir)
            [ $# -ge 2 ] || { printf 'backup: --out-dir requires a value\n' >&2; exit 2; }
            out_dir="$2"; shift 2 ;;
        --label)
            [ $# -ge 2 ] || { printf 'backup: --label requires a value\n' >&2; exit 2; }
            label="$2"; shift 2 ;;
        --encrypt-recipient)
            [ $# -ge 2 ] || { printf 'backup: --encrypt-recipient requires a value\n' >&2; exit 2; }
            recipient="$2"; shift 2 ;;
        --no-logs)
            include_logs=0; shift ;;
        -h|--help)
            usage; exit 0 ;;
        *)
            printf 'backup: unknown argument: %s (try --help)\n' "$1" >&2; exit 2 ;;
    esac
done

[ -n "$main_dsn" ] || die "main DSN is required (--main-dsn, \$NEWTG_MAIN_DSN or \$DATABASE_URL)"
if [ "$include_logs" -eq 1 ] && [ -z "$logs_dsn" ]; then
    die "logs DSN is required (--logs-dsn, \$NEWTG_LOGS_DSN or \$DB_LOGS_DSN); pass --no-logs to skip"
fi
if [ -n "$label" ] && ! [[ "$label" =~ ^[A-Za-z0-9._-]+$ ]]; then
    die "invalid --label '$label': only [A-Za-z0-9._-] are allowed"
fi

command -v pg_dump >/dev/null 2>&1 || die "pg_dump not found (install PostgreSQL 16 client)"
command -v psql >/dev/null 2>&1 || die "psql not found (install PostgreSQL 16 client)"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum not found"
command -v python3 >/dev/null 2>&1 || die "python3 not found (used for manifest JSON escaping)"
if [ -n "$recipient" ]; then
    command -v age >/dev/null 2>&1 || die "age not found but --encrypt-recipient/AGE_RECIPIENT is set"
fi

mkdir -p "$out_dir"
[ -w "$out_dir" ] || die "output directory is not writable: $out_dir"

ts="$(date -u +%Y%m%dT%H%M%SZ)"
prefix="newtg-${ts}${label:+-${label}}"

# 不打印 DSN；psql 只输出单行值。
psql_scalar() { # psql_scalar <dsn> <sql>
    psql "$1" -Atq -v ON_ERROR_STOP=1 -c "$2"
}

json_string() { # json_string <value>
    python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$1"
}

scalar_or_null() { # scalar_or_null <value>
    if [ -n "$1" ]; then json_string "$1"; else printf 'null\n'; fi
}

# dump_one <kind> <dsn>
dump_one() {
    local kind="$1" dsn="$2"
    local base="${prefix}-${kind}.dump"
    local out="$base"

    if [ -n "$recipient" ]; then
        out="${base}.age"
    fi

    note "writing ${kind} dump -> ${out_dir}/${out}"
    rm -f "$out_dir/$out"

    if [ -n "$recipient" ]; then
        # pipefail 保证 pg_dump 或 age 任一失败都返回非零；明文不落盘。
        pg_dump "$dsn" --format=custom --no-owner \
            | age --recipient "$recipient" --output "$out_dir/$out"
    else
        pg_dump "$dsn" --format=custom --no-owner \
            --file "$out_dir/$out"
    fi

    [ -s "$out_dir/$out" ] || die "${kind} dump is empty: $out_dir/$out"

    local sha size version dirty tables pgver
    sha="$(sha256sum "$out_dir/$out" | awk '{print $1}')"
    size="$(wc -c < "$out_dir/$out" | tr -d '[:space:]')"
    printf '%s\n' "$sha" > "$out_dir/$out.sha256"

    local row
    row="$(psql_scalar "$dsn" "SELECT version || '|' || dirty FROM schema_migrations ORDER BY version DESC LIMIT 1")"
    [ -n "$row" ] || die "${kind}: schema_migrations is empty; refusing to produce a manifest for an unmigrated database"
    version="${row%%|*}"
    dirty="${row#*|}"
    [[ "$version" =~ ^[0-9]+$ ]] || die "${kind}: unexpected schema_migrations.version '$version'"
    case "$dirty" in
        t|true|1)  dirty=true ;;
        f|false|0) dirty=false ;;
        *) die "${kind}: unexpected schema_migrations.dirty '$dirty'" ;;
    esac

    tables="$(psql_scalar "$dsn" "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_type='BASE TABLE'")"
    [[ "$tables" =~ ^[0-9]+$ ]] || die "${kind}: could not read table count"
    pgver="$(psql_scalar "$dsn" "SHOW server_version")"
    [ -n "$pgver" ] || die "${kind}: could not read server_version"

    {
        printf '{\n'
        printf '  "kind": %s,\n' "$(json_string "$kind")"
        printf '  "created_at": %s,\n' "$(json_string "$(date -u +%Y-%m-%dT%H:%M:%SZ)")"
        printf '  "dump_file": %s,\n' "$(json_string "$out")"
        printf '  "sha256": %s,\n' "$(json_string "$sha")"
        printf '  "dump_size_bytes": %s,\n' "$size"
        printf '  "encrypted": %s,\n' "$([ -n "$recipient" ] && printf true || printf false)"
        printf '  "age_recipient": %s,\n' "$(scalar_or_null "$recipient")"
        printf '  "pg_version": %s,\n' "$(json_string "$pgver")"
        printf '  "schema_migrations_version": %s,\n' "$version"
        printf '  "dirty": %s,\n' "$dirty"
        printf '  "table_count": %s,\n' "$tables"
        printf '  "producer_version": %s\n' "$(json_string "deploy/ops/backup/backup.sh")"
        printf '}\n'
    } > "$out_dir/$out.manifest.json"

    note "${kind}: ${size} bytes, sha256=${sha}"
    note "${kind}: manifest ${out}.manifest.json"
}

dump_one main "$main_dsn"
if [ "$include_logs" -eq 1 ]; then
    dump_one logs "$logs_dsn"
fi

note "done: $out_dir"
printf 'backup: OK\n'
