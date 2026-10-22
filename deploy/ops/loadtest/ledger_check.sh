#!/usr/bin/env bash
# NewTG 压测后账本一致性断言。
#
# 设计口径：
#   - platform-06 §4.1/§5.1 J22：压测全程无钳制、无负余额、无预扣悬挂；
#   - platform-08 §5.3 断言 4：逐用户 users.balance == Σ wallet_transactions.amount；
#   - platform-05 §5.1：request_id 是 usage_logs / wallet_transactions 的关联键。
#
# 输入：
#   - loadtest.py --request-id-file 产出的成功请求 ID（每行一个 UUID）；
#   - 主库 DSN、日志库 DSN（只从环境变量/参数读取，不写死口令）。
#
# 断言：
#   A. 主库：无负余额；逐用户 balance == Σ amount；无 settlement_pending 任务。
#   B. 日志库：每个成功 request_id 恰好 1 行 usage_logs；status ∈ 0..3；
#      success 行 cost >= 0；refused 行 cost == 0。
#   C. 跨库：每个 request_id 的钱包净额 == -cost（成功）；失败/退款净额 == 0。
#      （若压测账号走订阅额度而非钱包，用 --skip-wallet-ledger 跳过 C。）
#   D. request_registry：指定 request_id 无残留 in_flight。
#
# 用法：ledger_check.sh --help

set -euo pipefail
umask 077

usage() {
    cat <<'EOF'
NewTG 压测账本一致性断言

用法:
  ledger_check.sh [选项]

DSN（必填；优先环境变量）:
  --main-dsn DSN          主库；默认 $NEWTG_MAIN_DSN
  --logs-dsn DSN          日志库；默认 $NEWTG_LOGS_DSN

输入:
  --request-ids FILE      loadtest.py --request-id-file 产物；可省略（只跑全局断言）
  --user-id N             只对该用户做余额断言；省略则检查全部用户
  --wait-seconds N        等待 usage_logs/registry 收敛的秒数（默认 30）
  --skip-wallet-ledger    跳过跨库“净额 == -cost”断言（订阅额度账号）

其他:
  -h, --help              显示本帮助

退出码:
  0 全部通过；1 断言失败；2 用法错误。

示例:
  NEWTG_MAIN_DSN='postgres://newtg_app:<password>@postgres:5432/newtg?sslmode=disable' \
  NEWTG_LOGS_DSN='postgres://newtg_app:<password>@postgres:5432/newtg_logs?sslmode=disable' \
  deploy/ops/loadtest/ledger_check.sh --request-ids /tmp/newtg-ids.txt --user-id 12345
EOF
}

die()  { printf 'ledger_check: %s\n' "$*" >&2; exit 1; }
note() { printf '[ledger_check] %s\n' "$*"; }
ok()   { printf 'ok   %s\n' "$*"; }

main_dsn="${NEWTG_MAIN_DSN:-}"
logs_dsn="${NEWTG_LOGS_DSN:-}"
ids_file=""
user_id=""
wait_seconds=30
skip_wallet=0

while [ $# -gt 0 ]; do
    case "$1" in
        --main-dsn)
            [ $# -ge 2 ] || { printf 'ledger_check: --main-dsn requires a value\n' >&2; exit 2; }
            main_dsn="$2"; shift 2 ;;
        --logs-dsn)
            [ $# -ge 2 ] || { printf 'ledger_check: --logs-dsn requires a value\n' >&2; exit 2; }
            logs_dsn="$2"; shift 2 ;;
        --request-ids)
            [ $# -ge 2 ] || { printf 'ledger_check: --request-ids requires a value\n' >&2; exit 2; }
            ids_file="$2"; shift 2 ;;
        --user-id)
            [ $# -ge 2 ] || { printf 'ledger_check: --user-id requires a value\n' >&2; exit 2; }
            user_id="$2"; shift 2 ;;
        --wait-seconds)
            [ $# -ge 2 ] || { printf 'ledger_check: --wait-seconds requires a value\n' >&2; exit 2; }
            wait_seconds="$2"; shift 2 ;;
        --skip-wallet-ledger)
            skip_wallet=1; shift ;;
        -h|--help)
            usage; exit 0 ;;
        *)
            printf 'ledger_check: unknown argument: %s (try --help)\n' "$1" >&2; exit 2 ;;
    esac
done

[ -n "$main_dsn" ] || die "main DSN required (--main-dsn or \$NEWTG_MAIN_DSN)"
[ -n "$logs_dsn" ] || die "logs DSN required (--logs-dsn or \$NEWTG_LOGS_DSN)"
command -v psql >/dev/null 2>&1 || die "psql not found (install PostgreSQL 16 client)"
[[ "$wait_seconds" =~ ^[0-9]+$ ]] || die "--wait-seconds must be a non-negative integer"
if [ -n "$user_id" ] && ! [[ "$user_id" =~ ^[0-9]+$ ]]; then
    die "--user-id must be numeric"
fi
[ -z "$ids_file" ] || [ -f "$ids_file" ] || die "request ids file not found: $ids_file"

psql_scalar() { # psql_scalar <dsn> <sql>
    psql "$1" -Atq -v ON_ERROR_STOP=1 -c "$2"
}

user_filter=""
[ -z "$user_id" ] || user_filter=" AND u.id = ${user_id}"

# ---- A. 主库全局断言 ------------------------------------------------------
note "main: balance / ledger / settlement_pending assertions"
main_sql="$(cat <<SQL
DO \$\$
DECLARE bad bigint;
BEGIN
    SELECT count(*) INTO bad FROM users u WHERE u.balance < 0${user_filter};
    IF bad > 0 THEN
        RAISE EXCEPTION 'ledger main: % users with negative balance', bad;
    END IF;

    SELECT count(*) INTO bad
      FROM (
            SELECT u.id
              FROM users u
              LEFT JOIN (
                    SELECT user_id, sum(amount) AS total
                      FROM wallet_transactions
                     GROUP BY user_id
              ) w ON w.user_id = u.id
             WHERE u.balance <> COALESCE(w.total, 0)${user_filter}
           ) AS mismatch;
    IF bad > 0 THEN
        RAISE EXCEPTION 'ledger main: % users whose balance != sum(wallet_transactions.amount)', bad;
    END IF;

    IF to_regclass('public.tasks') IS NOT NULL THEN
        SELECT count(*) INTO bad
          FROM tasks
         WHERE private_data @> '{"settlement_pending": true}'::jsonb;
        IF bad > 0 THEN
            RAISE EXCEPTION 'ledger main: % tasks flagged settlement_pending', bad;
        END IF;
    END IF;

    RAISE NOTICE 'ledger main: PASS';
END
\$\$;
SQL
)"
psql "$main_dsn" -v ON_ERROR_STOP=1 -c "$main_sql"
ok "main: no negative balance, ledger equation holds, no settlement_pending"

# ---- 读取并校验 request ids ------------------------------------------------
expected_count=0
ids_sql=""
if [ -n "$ids_file" ]; then
    tmp_ids="$(mktemp)"
    trap 'rm -f "$tmp_ids" ${tmp_logs:-} ${tmp_wallet:-}' EXIT

    while IFS= read -r raw || [ -n "$raw" ]; do
        id="$(printf '%s' "$raw" | tr -d '[:space:]')"
        [ -z "$id" ] && continue
        if ! [[ "$id" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]]; then
            die "invalid request id in $ids_file: $id"
        fi
        printf '%s\n' "$id"
    done < "$ids_file" | sort -u > "$tmp_ids"

    expected_count="$(wc -l < "$tmp_ids" | tr -d '[:space:]')"
    [ "$expected_count" -gt 0 ] || die "request ids file is empty: $ids_file"
    [ "$expected_count" -le 5000 ] || die "too many request ids ($expected_count); split the run (max 5000)"

    while IFS= read -r id; do
        [ -n "$ids_sql" ] && ids_sql+=","
        ids_sql+="'$id'"
    done < "$tmp_ids"
    ids_sql="($ids_sql)"
fi

# ---- D. 等待收敛 + request_registry --------------------------------------
if [ -n "$ids_sql" ]; then
    note "waiting up to ${wait_seconds}s for usage_logs and request_registry to converge"
    deadline=$((SECONDS + wait_seconds))
    while :; do
        found="$(psql_scalar "$logs_dsn" "SELECT count(DISTINCT request_id) FROM usage_logs WHERE request_id IN $ids_sql")"
        inflight="$(psql_scalar "$main_dsn" "SELECT count(*) FROM request_registry WHERE request_id IN $ids_sql AND state = 'in_flight'")"
        if [ "$found" -ge "$expected_count" ] && [ "$inflight" -eq 0 ]; then
            break
        fi
        if [ "$SECONDS" -ge "$deadline" ]; then
            break
        fi
        sleep 2
    done
    [ "$found" -ge "$expected_count" ] \
        || die "only ${found}/${expected_count} request_ids landed in usage_logs within ${wait_seconds}s"
    [ "$inflight" -eq 0 ] \
        || die "${inflight} request_ids still in_flight in request_registry after ${wait_seconds}s"
    ok "usage_logs: all ${expected_count} request_ids landed"
    ok "request_registry: no in_flight rows"
fi

# ---- B. 日志库 per-request 断言 ------------------------------------------
if [ -n "$ids_sql" ]; then
    note "logs: usage_logs per-request assertions"
    logs_sql="$(cat <<SQL
DO \$\$
DECLARE bad bigint;
BEGIN
    SELECT count(*) INTO bad
      FROM (
            SELECT request_id
              FROM usage_logs
             WHERE request_id IN $ids_sql
             GROUP BY request_id
            HAVING count(*) <> 1
           ) AS dup;
    IF bad > 0 THEN
        RAISE EXCEPTION 'ledger logs: % request_ids do not have exactly one usage_logs row', bad;
    END IF;

    SELECT count(*) INTO bad
      FROM usage_logs
     WHERE request_id IN $ids_sql
       AND status NOT IN (0, 1, 2, 3);
    IF bad > 0 THEN
        RAISE EXCEPTION 'ledger logs: % usage_logs rows with invalid status', bad;
    END IF;

    SELECT count(*) INTO bad
      FROM usage_logs
     WHERE request_id IN $ids_sql
       AND status = 1
       AND COALESCE(cost, 0) < 0;
    IF bad > 0 THEN
        RAISE EXCEPTION 'ledger logs: % success rows with negative cost', bad;
    END IF;

    SELECT count(*) INTO bad
      FROM usage_logs
     WHERE request_id IN $ids_sql
       AND status = 0
       AND COALESCE(cost, 0) <> 0;
    IF bad > 0 THEN
        RAISE EXCEPTION 'ledger logs: % refused rows with non-zero cost', bad;
    END IF;

    RAISE NOTICE 'ledger logs: PASS';
END
\$\$;
SQL
)"
    psql "$logs_dsn" -v ON_ERROR_STOP=1 -c "$logs_sql"
    ok "logs: one row per request, valid status/cost"
fi

# ---- C. 跨库：钱包净额 == -cost ------------------------------------------
if [ -n "$ids_sql" ] && [ "$skip_wallet" -eq 0 ]; then
    note "cross-db: wallet net amount vs usage_logs.cost"
    tmp_logs="$(mktemp)"
    tmp_wallet="$(mktemp)"

    psql "$logs_dsn" -Atq -F '|' -v ON_ERROR_STOP=1 \
        -c "SELECT request_id, status, COALESCE(cost, 0) FROM usage_logs WHERE request_id IN $ids_sql" \
        > "$tmp_logs"
    psql "$main_dsn" -Atq -F '|' -v ON_ERROR_STOP=1 \
        -c "SELECT reference_id, sum(amount) FROM wallet_transactions WHERE reference_type = 'usage_log' AND reference_id IN $ids_sql GROUP BY reference_id" \
        > "$tmp_wallet"

    ledger_bad="$(awk -F'|' '
        NR == FNR { net[tolower($1)] = $2; next }
        {
            id = tolower($1)
            status = $2
            cost = $3
            n = (id in net) ? net[id] : 0
            if (status == 1) {
                if (n + cost != 0) {
                    printf "request %s: wallet net=%s cost=%s (expected net=-cost)\n", id, n, cost
                    bad++
                }
            } else if (n != 0) {
                printf "request %s: status=%s wallet net=%s (expected 0)\n", id, status, n
                bad++
            }
        }
        END { exit (bad > 0) }
    ' "$tmp_wallet" "$tmp_logs")" && ledger_rc=0 || ledger_rc=$?

    if [ "$ledger_rc" -ne 0 ]; then
        printf '%s\n' "$ledger_bad" >&2
        die "cross-db ledger assertions failed"
    fi
    ok "cross-db: wallet net == -cost for success; failed/refunded net == 0"
elif [ -n "$ids_sql" ]; then
    note "skip-wallet-ledger: cross-db net assertion skipped"
fi

printf 'ledger_check: PASS\n'
