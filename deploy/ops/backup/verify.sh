#!/usr/bin/env bash
# NewTG 备份轻校验（platform-08 §5.1 的 V1–V4）。
#
# 只读校验，不把数据恢复到任何数据库：
#   V1  pg_restore --list 能解析（包结构完好）
#   V2  SHA-256 与 sidecar 一致（传输/存储未损坏）
#   V3  age 解密成功（仅加密包；证明 L1 密钥正确）
#   V4  manifest 的 version/dirty/table_count 自洽且符合期望
#
# 用法：verify.sh --help

set -euo pipefail
umask 077

usage() {
    cat <<'EOF'
NewTG 备份轻校验（V1–V4）

用法:
  verify.sh --dump FILE [选项]

必填:
  --dump FILE             转储文件（.dump 或 .dump.age）

可选:
  --manifest FILE         manifest 路径；默认 <dump>.manifest.json
  --sha256 FILE           SHA-256 sidecar；默认 <dump>.sha256
  --identity FILE         age 私钥；默认 $AGE_IDENTITY。加密包必填
  --expect-version N      期望 schema_migrations.version（数字）
  --expect-tables N       期望表数（information_schema BASE TABLE）
  --skip-content-check    只做 V2/V4，不做 V1/V3（不读取包内容）
  -h, --help              显示本帮助

退出码:
  0 全部通过；1 校验失败；2 用法错误。

示例:
  deploy/ops/backup/verify.sh --dump /backups/newtg-20261022T010000Z-main.dump
  AGE_IDENTITY=/secure/age.key \
    deploy/ops/backup/verify.sh --dump /backups/newtg-20261022T010000Z-main.dump.age \
      --expect-version 20261024100000
EOF
}

die()  { printf 'verify: %s\n' "$*" >&2; exit 1; }
ok()   { printf 'ok   %s\n' "$*"; }
note() { printf '[verify] %s\n' "$*"; }

dump=""
manifest=""
sha_file=""
identity="${AGE_IDENTITY:-}"
expect_version=""
expect_tables=""
skip_content=0

while [ $# -gt 0 ]; do
    case "$1" in
        --dump)
            [ $# -ge 2 ] || { printf 'verify: --dump requires a value\n' >&2; exit 2; }
            dump="$2"; shift 2 ;;
        --manifest)
            [ $# -ge 2 ] || { printf 'verify: --manifest requires a value\n' >&2; exit 2; }
            manifest="$2"; shift 2 ;;
        --sha256)
            [ $# -ge 2 ] || { printf 'verify: --sha256 requires a value\n' >&2; exit 2; }
            sha_file="$2"; shift 2 ;;
        --identity)
            [ $# -ge 2 ] || { printf 'verify: --identity requires a value\n' >&2; exit 2; }
            identity="$2"; shift 2 ;;
        --expect-version)
            [ $# -ge 2 ] || { printf 'verify: --expect-version requires a value\n' >&2; exit 2; }
            expect_version="$2"; shift 2 ;;
        --expect-tables)
            [ $# -ge 2 ] || { printf 'verify: --expect-tables requires a value\n' >&2; exit 2; }
            expect_tables="$2"; shift 2 ;;
        --skip-content-check)
            skip_content=1; shift ;;
        -h|--help)
            usage; exit 0 ;;
        *)
            printf 'verify: unknown argument: %s (try --help)\n' "$1" >&2; exit 2 ;;
    esac
done

[ -n "$dump" ] || die "--dump is required (try --help)"
[ -f "$dump" ] || die "dump not found: $dump"
[ -n "$manifest" ] || manifest="${dump}.manifest.json"
[ -n "$sha_file" ] || sha_file="${dump}.sha256"
[ -f "$manifest" ] || die "manifest not found: $manifest"
[ -f "$sha_file" ] || die "sha256 sidecar not found: $sha_file"

if [ -n "$expect_version" ] && ! [[ "$expect_version" =~ ^[0-9]+$ ]]; then
    die "--expect-version must be numeric"
fi
if [ -n "$expect_tables" ] && ! [[ "$expect_tables" =~ ^[0-9]+$ ]]; then
    die "--expect-tables must be numeric"
fi

command -v sha256sum >/dev/null 2>&1 || die "sha256sum not found"
command -v python3 >/dev/null 2>&1 || die "python3 not found"
if [ "$skip_content" -eq 0 ]; then
    command -v pg_restore >/dev/null 2>&1 || die "pg_restore not found (install PostgreSQL 16 client)"
fi

# ---- V2：SHA-256 ---------------------------------------------------------
expected_sha="$(awk '{print $1}' "$sha_file" | tr -d '[:space:]' | tr '[:upper:]' '[:lower:]')"
[[ "$expected_sha" =~ ^[0-9a-fA-F]{64}$ ]] || die "malformed sha256 sidecar: $sha_file"
actual_sha="$(sha256sum "$dump" | awk '{print $1}')"
if [ "$expected_sha" = "$actual_sha" ]; then
    ok "V2 sha256 matches ($actual_sha)"
else
    die "V2 sha256 mismatch: sidecar=$expected_sha actual=$actual_sha"
fi

# ---- V4：manifest --------------------------------------------------------
meta=""
if ! meta="$(python3 - "$manifest" "$actual_sha" "$expect_version" "$expect_tables" <<'PY'
import json
import sys

path, actual_sha, expect_version, expect_tables = sys.argv[1:5]
with open(path, encoding="utf-8") as f:
    m = json.load(f)

required = ["kind", "created_at", "dump_file", "sha256", "encrypted",
            "schema_migrations_version", "dirty", "table_count"]
missing = [k for k in required if k not in m]
if missing:
    raise SystemExit(f"V4 manifest missing keys: {', '.join(missing)}")

if m["dirty"] is not False:
    raise SystemExit(f"V4 manifest dirty={m['dirty']!r}, expected false")
if m["sha256"].lower() != actual_sha.lower():
    raise SystemExit("V4 manifest sha256 != sidecar sha256")
try:
    version = int(m["schema_migrations_version"])
    tables = int(m["table_count"])
except (TypeError, ValueError) as exc:
    raise SystemExit(f"V4 manifest has non-numeric version/table_count: {exc}")
if version <= 0:
    raise SystemExit(f"V4 manifest schema_migrations_version={version}, expected >0")
if tables <= 0:
    raise SystemExit(f"V4 manifest table_count={tables}, expected >0")
if expect_version and version != int(expect_version):
    raise SystemExit(f"V4 version={version}, expected {expect_version}")
if expect_tables and tables != int(expect_tables):
    raise SystemExit(f"V4 table_count={tables}, expected {expect_tables}")

encrypted = m["encrypted"]
if not isinstance(encrypted, bool):
    raise SystemExit("V4 manifest encrypted must be boolean")
print(f"kind={m['kind']} version={version} dirty=false tables={tables} encrypted={str(encrypted).lower()} pg={m.get('pg_version','?')}")
PY
)"; then
    die "V4 manifest validation failed"
fi
ok "V4 manifest: $meta"

# ---- V3 + V1：解密与结构 --------------------------------------------------
if [ "$skip_content" -eq 1 ]; then
    note "skip-content-check: V1/V3 skipped"
    printf 'verify: PASS\n'
    exit 0
fi

encrypted="$(
    python3 - "$manifest" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    print("true" if json.load(f).get("encrypted") else "false")
PY
)"

tmp=""
cleanup() { [ -z "$tmp" ] || rm -f "$tmp"; }
trap cleanup EXIT

plain="$dump"
if [ "$encrypted" = "true" ]; then
    command -v age >/dev/null 2>&1 || die "age not found but manifest says encrypted=true"
    [ -n "$identity" ] || die "encrypted dump requires --identity or \$AGE_IDENTITY"
    [ -f "$identity" ] || die "age identity not found: $identity"
    tmp="$(mktemp)"
    note "decrypting to a private temporary file for V1/V3"
    age --decrypt --identity "$identity" --output "$tmp" "$dump"
    plain="$tmp"
    ok "V3 age decrypt succeeded"
else
    ok "V3 not encrypted (skipped)"
fi

toc="$(mktemp)"
cleanup_toc() { rm -f "$toc"; cleanup; }
trap cleanup_toc EXIT
pg_restore --list "$plain" > "$toc"
if [ ! -s "$toc" ]; then
    die "V1 pg_restore --list produced an empty TOC"
fi
ok "V1 pg_restore --list parsed ($(wc -l < "$toc" | tr -d '[:space:]') TOC lines)"

printf 'verify: PASS\n'
