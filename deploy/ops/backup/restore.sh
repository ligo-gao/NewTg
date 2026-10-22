#!/usr/bin/env bash
# NewTG 备份恢复到“新实例”。
#
# 设计依据：platform-08 §4（恢复四型）、§4.2（R-A 完整路径）、§5.3（断言集）。
#
# 硬性纪律：
#   - 恢复永远到新实例；本脚本不提供“恢复到生产 DSN”的默认值，
#     目标 DSN 必须显式给出（--target-dsn 或 $NEWTG_RESTORE_DSN）。
#   - 默认执行 pg_restore --clean --if-exists；对空库可用 --no-clean。
#   - 没有 --yes 不执行恢复（只打印计划），避免误操作。
#   - 恢复前默认先跑 verify.sh（V1–V4）；可用 --skip-verify 跳过，但不推荐。
#
# 用法：restore.sh --help

set -euo pipefail
umask 077

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
    cat <<'EOF'
NewTG 备份恢复脚本

用法:
  restore.sh --dump FILE --target-dsn DSN [选项]

必填:
  --dump FILE             转储文件（.dump 或 .dump.age）
  --target-dsn DSN        目标新实例 DSN；也可用 $NEWTG_RESTORE_DSN

可选:
  --kind main|logs        断言类型与默认 manifest 语义；默认 main
  --manifest FILE         manifest 路径；默认 <dump>.manifest.json
  --identity FILE         age 私钥；默认 $AGE_IDENTITY
  --check                 恢复后运行 assert_<kind>.sql
  --no-clean              不传 --clean --if-exists（空库首次恢复）
  --skip-verify           跳过 verify.sh（不推荐）
  --yes                   确认执行（没有它只打印计划）
  --dry-run               只打印计划并退出 0
  -h, --help              显示本帮助

退出码:
  0 成功/计划已打印；1 执行失败；2 用法错误或缺少 --yes。

示例:
  export NEWTG_RESTORE_DSN='postgres://newtg_migrate:<password>@127.0.0.1:55432/newtg_restore?sslmode=disable'
  deploy/ops/backup/restore.sh \
    --dump /backups/newtg-20261022T010000Z-main.dump \
    --target-dsn "$NEWTG_RESTORE_DSN" --check --yes

注意:
  目标库应与备份同大版本（platform-08 U12）；跨大版本恢复不在脚本保证范围内。
  恢复后若备份 schema 版本低于当前代码，按 platform-08 §4.4 先跑一次
  migrate（RUN_MODE=migrate）再启动应用。
EOF
}

die()  { printf 'restore: %s\n' "$*" >&2; exit 1; }
note() { printf '[restore] %s\n' "$*"; }

dump=""
target_dsn="${NEWTG_RESTORE_DSN:-}"
manifest=""
identity="${AGE_IDENTITY:-}"
kind="main"
do_check=0
do_clean=1
skip_verify=0
confirmed=0
dry_run=0

while [ $# -gt 0 ]; do
    case "$1" in
        --dump)
            [ $# -ge 2 ] || { printf 'restore: --dump requires a value\n' >&2; exit 2; }
            dump="$2"; shift 2 ;;
        --target-dsn)
            [ $# -ge 2 ] || { printf 'restore: --target-dsn requires a value\n' >&2; exit 2; }
            target_dsn="$2"; shift 2 ;;
        --kind)
            [ $# -ge 2 ] || { printf 'restore: --kind requires a value\n' >&2; exit 2; }
            kind="$2"; shift 2 ;;
        --manifest)
            [ $# -ge 2 ] || { printf 'restore: --manifest requires a value\n' >&2; exit 2; }
            manifest="$2"; shift 2 ;;
        --identity)
            [ $# -ge 2 ] || { printf 'restore: --identity requires a value\n' >&2; exit 2; }
            identity="$2"; shift 2 ;;
        --check)
            do_check=1; shift ;;
        --no-clean)
            do_clean=0; shift ;;
        --skip-verify)
            skip_verify=1; shift ;;
        --yes)
            confirmed=1; shift ;;
        --dry-run)
            dry_run=1; shift ;;
        -h|--help)
            usage; exit 0 ;;
        *)
            printf 'restore: unknown argument: %s (try --help)\n' "$1" >&2; exit 2 ;;
    esac
done

[ -n "$dump" ] || die "--dump is required (try --help)"
[ -f "$dump" ] || die "dump not found: $dump"
[ -n "$target_dsn" ] || die "--target-dsn or \$NEWTG_RESTORE_DSN is required (no default target)"
case "$kind" in
    main|logs) ;;
    *) die "--kind must be main or logs" ;;
esac
[ -n "$manifest" ] || manifest="${dump}.manifest.json"

if [ "$dry_run" -eq 0 ] && [ "$confirmed" -ne 1 ]; then
    note "plan: restore '$dump' -> <target DSN>"
    note "kind=$kind clean=$do_clean check=$do_check verify=$((1 - skip_verify))"
    die "refusing to restore without --yes (use --dry-run to inspect the plan)"
fi

if [ "$dry_run" -eq 0 ]; then
    command -v pg_restore >/dev/null 2>&1 || die "pg_restore not found (install PostgreSQL 16 client)"
    command -v psql >/dev/null 2>&1 || die "psql not found (install PostgreSQL 16 client)"
fi

if [ "$skip_verify" -eq 0 ]; then
    verify_args=(--dump "$dump")
    [ -f "$manifest" ] && verify_args+=(--manifest "$manifest")
    [ -n "$identity" ] && verify_args+=(--identity "$identity")
    note "running verify.sh (V1–V4)"
    bash "$script_dir/verify.sh" "${verify_args[@]}"
else
    note "skip-verify: V1–V4 not executed"
fi

# 判断是否加密（manifest 缺失时按扩展名兜底）。
encrypted="false"
if [ -f "$manifest" ]; then
    encrypted="$(
        python3 - "$manifest" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    print("true" if json.load(f).get("encrypted") else "false")
PY
)"
elif [[ "$dump" == *.age ]]; then
    encrypted="true"
fi

if [ "$dry_run" -eq 1 ]; then
    # 不展开目标 DSN，避免口令进入输出。
    note "dry-run: pg_restore --no-owner $([ "$do_clean" -eq 1 ] && printf -- '--clean --if-exists ')--dbname <target DSN> <$dump>"
    [ "$do_check" -eq 1 ] && note "dry-run: psql <target DSN> -f ${script_dir}/assert_${kind}.sql"
    printf 'restore: dry-run OK\n'
    exit 0
fi

tmp=""
cleanup() { [ -z "$tmp" ] || rm -f "$tmp"; }
trap cleanup EXIT

plain="$dump"
if [ "$encrypted" = "true" ]; then
    command -v age >/dev/null 2>&1 || die "age not found but dump is encrypted"
    [ -n "$identity" ] || die "encrypted dump requires --identity or \$AGE_IDENTITY"
    [ -f "$identity" ] || die "age identity not found: $identity"
    tmp="$(mktemp)"
    note "decrypting to a private temporary file"
    age --decrypt --identity "$identity" --output "$tmp" "$dump"
    plain="$tmp"
fi

restore_args=(--no-owner --dbname "$target_dsn")
if [ "$do_clean" -eq 1 ]; then
    restore_args+=(--clean --if-exists)
fi

note "restoring into <target DSN> (kind=$kind, clean=$do_clean)"
pg_restore "${restore_args[@]}" "$plain"
note "pg_restore completed"

if [ "$do_check" -eq 1 ]; then
    assert_sql="${script_dir}/assert_${kind}.sql"
    [ -f "$assert_sql" ] || die "assertion SQL not found: $assert_sql"
    note "running assertions: $assert_sql"
    psql "$target_dsn" -v ON_ERROR_STOP=1 -f "$assert_sql"
fi

printf 'restore: OK\n'
