#!/usr/bin/env bash
# compose 骨架验收（M0 Task 11）：渲染 deploy/docker-compose.yml 后逐项断言。
#
# 用法：bash deploy/scripts/check_compose.sh   （仓库根或任意目录均可）
#
# 断言项（plan Task 11 Step 1）：
#   a. gateway/worker/migrate 三服务 RUN_MODE 正确，且全配置无 NEWTG_ROLE；
#   b. 四个必需密钥 + 备份密钥全部带 `${VAR:?...}` fail-fast 守卫；
#   c. postgres 镜像为 postgres:16-alpine（GC-4，禁止 15）；
#   d. backend 网络 internal: true；
#   e. gateway 副本数 == 2、worker == 1；
#   f. gateway healthcheck 走内置子命令 `/newtg healthcheck`（GC-6/R-07）；
#   g. gateway/migrate/worker 的 stop_grace_period 均为 35s（终审修复回归守卫）；
#   h. R-23：worker 挂非 internal 的 egress 网，postgres/redis 不挂。
#   i. R-25/R-59/R-60：日志库仅 usage_logs 授 newtg_app 五列 UPDATE
#      (status,cost,refunded,error_class,error_message)；audit_logs 保持 append-only；
#      R-56/R-61：日志库迁移预建 12 个月分区并清除旧授权（运行期 worker 无 DDL 权限）。
#
# 渲染所需口令在脚本内以哑值兜底（shell 环境优先于 deploy/.env），因此本脚本
# 不依赖、也不创建 deploy/.env，可直接在 CI 中运行。
set -euo pipefail

fail=0
ok()  { printf 'ok   %s\n' "$*"; }
bad() { printf 'FAIL %s\n' "$*" >&2; fail=1; }
die() { printf 'FAIL %s\n' "$*" >&2; exit 1; }

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
compose="$root/deploy/docker-compose.yml"

# --- 交付物存在性 ---------------------------------------------------------
for f in \
  "$compose" \
  "$root/deploy/.env.example" \
  "$root/deploy/edge/Caddyfile" \
  "$root/deploy/postgres-init/10-roles.sql" \
  "$root/deploy/postgres-init/20-databases.sql"; do
  [ -f "$f" ] || die "缺少文件 $f"
done

# --- 渲染前置：密钥哑值（仅静态渲染，非真实口令） --------------------------
: "${POSTGRES_PASSWORD:=check-compose-dummy}"
: "${REDIS_PASSWORD:=check-compose-dummy}"
: "${POSTGRES_APP_PASSWORD:=check-compose-dummy}"
: "${POSTGRES_MIGRATE_PASSWORD:=check-compose-dummy}"
: "${POSTGRES_BACKUP_PASSWORD:=check-compose-dummy}"
: "${SESSION_SECRET:=check-compose-dummy}"
: "${CREDENTIAL_ENCRYPTION_KEY:=check-compose-dummy}"
export POSTGRES_PASSWORD REDIS_PASSWORD POSTGRES_APP_PASSWORD \
  POSTGRES_MIGRATE_PASSWORD POSTGRES_BACKUP_PASSWORD \
  SESSION_SECRET CREDENTIAL_ENCRYPTION_KEY

command -v docker >/dev/null 2>&1 || die "docker 不可用"
# jq 是本检查解析 `docker compose config --format json` 的唯一手段；缺失时
# 不能降级成「只跑 grep」——那会把所有结构化断言静默变成永远通过。
command -v jq >/dev/null 2>&1 \
  || die "jq 缺失：本检查必须用 jq 解析 docker compose config --format json（required for this check），请先安装 jq"

rendered=""
if ! rendered="$(cd "$root/deploy" && docker compose -f "$compose" config --format json 2>&1)"; then
  printf '%s\n' "$rendered" >&2
  die "docker compose config 渲染失败"
fi

# 结构化取值。jq 解析失败时打印 FAIL 并以空值返回：调用方的断言分支会再次
# 记录期望值，但至少不会因 set -e 在命令替换里直接中止而只留下空白输出。
q() { # q <jq-filter>
  local out
  if ! out="$(jq -r "$1" <<<"$rendered" 2>&1)"; then
    bad "jq 解析失败（filter: $1）：$out"
    return 0
  fi
  printf '%s\n' "$out"
}

# set -e + pipefail 下，grep 无匹配（退出码 1）发生在命令替换里会直接终止脚本，
# 连 FAIL 行都不会打印。这两个 helper 把「无匹配」降级为正常空值/0，交给断言分支报错。
line_of()  { grep -nF -- "$1" "$2" | head -1 | cut -d: -f1 || true; }
count_of() { grep -cF -- "$1" "$2" || true; }

# --- a. RUN_MODE 与 NEWTG_ROLE -------------------------------------------
for pair in "gateway gateway" "worker worker" "migrate migrate"; do
  set -- $pair
  got="$(q ".services.${1}.environment.RUN_MODE // \"\"")"
  if [ "$got" = "$2" ]; then
    ok "services.$1 RUN_MODE=$2"
  else
    bad "services.$1 RUN_MODE=${got:-<未设置>}，期望 $2"
  fi
done
if grep -q 'NEWTG_ROLE' <<<"$rendered"; then
  bad "渲染配置中出现 NEWTG_ROLE（裁决 R-01：实现用 RUN_MODE）"
else
  ok "渲染配置不含 NEWTG_ROLE"
fi

# --- b. 密钥 fail-fast 守卫 ----------------------------------------------
for v in POSTGRES_PASSWORD REDIS_PASSWORD POSTGRES_APP_PASSWORD \
         POSTGRES_MIGRATE_PASSWORD POSTGRES_BACKUP_PASSWORD; do
  if grep -Fq "${v}:?set ${v} in deploy/.env" "$compose"; then
    ok "${v} 带 :? 守卫"
  else
    bad "${v} 缺少 \${${v}:?set ${v} in deploy/.env} 守卫"
  fi
done

# --- c. postgres:16-alpine ------------------------------------------------
pg_image="$(q '.services.postgres.image // ""')"
if [ "$pg_image" = "postgres:16-alpine" ]; then
  ok "services.postgres 镜像为 postgres:16-alpine"
else
  bad "services.postgres 镜像为 ${pg_image:-<空>}，期望 postgres:16-alpine"
fi
if grep -q 'postgres:15' "$compose"; then
  bad "compose 中出现 postgres:15（GC-4 禁止）"
else
  ok "compose 不含 postgres:15"
fi

# --- d. backend 网络 internal: true --------------------------------------
backend_internal="$(q '.networks.backend.internal // false')"
if [ "$backend_internal" = "true" ]; then
  ok "networks.backend.internal=true"
else
  bad "networks.backend.internal=${backend_internal}，期望 true"
fi

# --- e. 副本数 ------------------------------------------------------------
gw_replicas="$(q '.services.gateway.deploy.replicas // 0')"
wk_replicas="$(q '.services.worker.deploy.replicas // 0')"
if [ "$gw_replicas" = "2" ]; then ok "gateway 副本数=2"; else bad "gateway 副本数=${gw_replicas}，期望 2"; fi
if [ "$wk_replicas" = "1" ]; then ok "worker 副本数=1"; else bad "worker 副本数=${wk_replicas}，期望 1"; fi

# --- f. 内置 healthcheck 子命令 ------------------------------------------
gw_hc="$(q '.services.gateway.healthcheck.test | join(" ")')"
if [[ "$gw_hc" == *"/newtg healthcheck"* ]] && [[ "$gw_hc" == *"http://127.0.0.1:3000/readyz"* ]]; then
  ok "gateway healthcheck：$gw_hc"
else
  bad "gateway healthcheck 未走内置子命令 /newtg healthcheck --url .../readyz（实际：${gw_hc:-<空>}）"
fi
# R-07：alpine 运行镜像不装 wget，任何服务都不得以 wget 探活。
if grep -q 'wget' <<<"$rendered"; then
  bad "渲染配置中出现 wget（R-07：运行镜像无 wget）"
else
  ok "渲染配置不含 wget"
fi

# --- g. 优雅停机窗口（终审修复回归守卫） --------------------------------
# Docker 默认只等 10s，短于应用 SHUTDOWN_GRACE_SECONDS（默认 30s）；gateway/worker
# 会硬断在途请求，migrate 被 SIGKILL 会在 schema_migrations 留下 dirty=true。
# 三处都必须显式为 35s（30s 窗口 + 5s 余量），任何一处回退都要在评审前红灯。
for svc in gateway migrate worker; do
  sg="$(q ".services.${svc}.stop_grace_period // \"\"")"
  if [ "$sg" = "35s" ]; then
    ok "services.$svc stop_grace_period=35s"
  else
    bad "services.$svc stop_grace_period=${sg:-<未设置>}，期望 35s（优雅停机 30s + 5s 余量）"
  fi
done

# --- h. R-23 worker 出站面 ------------------------------------------------
# worker 的渠道测试/Webhook/GeoIP 需要出站（security-08 §2.11），因此必须挂到
# 非 internal 的 egress；同时 DB/Redis 所在的 backend 保持 internal，且数据面
# 服务不得被顺手挂上 egress（否则 internal 的隔离意图被绕过）。
egress_internal="$(q '.networks.egress.internal // false')"
if [ "$egress_internal" = "false" ]; then
  ok "networks.egress 非 internal（worker 可出站）"
else
  bad "networks.egress.internal=${egress_internal}，期望 false（R-23：worker 必须能出站）"
fi
worker_on_egress="$(q '.services.worker.networks // {} | has("egress")')"
if [ "$worker_on_egress" = "true" ]; then
  ok "worker 已挂 egress 网"
else
  bad "worker 未挂 egress 网（R-23：渠道测试/Webhook/GeoIP 将无法出站）"
fi
for svc in postgres redis; do
  on_egress="$(q ".services.${svc}.networks // {} | has(\"egress\")")"
  if [ "$on_egress" = "false" ]; then
    ok "services.$svc 不挂 egress 网"
  else
    bad "services.$svc 不得挂 egress 网（数据面必须留在 internal 的 backend）"
  fi
done

# --- 附加：服务表冻结值 ---------------------------------------------------
[ "$(q '.services.worker.container_name // ""')" = "newtg-worker" ] \
  && ok "worker container_name=newtg-worker" \
  || bad "worker container_name 期望 newtg-worker"
[ "$(q '.services.migrate.restart // ""')" = "no" ] \
  && ok "migrate restart=no（一次性服务）" \
  || bad "migrate restart 期望 \"no\""
[ "$(q '.services.postgres.ports // [] | length')" = "0" ] \
  && ok "postgres 不发布宿主端口" \
  || bad "postgres 不应发布宿主端口"
[ "$(q '.services.redis.ports // [] | length')" = "0" ] \
  && ok "redis 不发布宿主端口" \
  || bad "redis 不应发布宿主端口"

# 应用镜像统一走 NEWTG_IMAGE（默认与 Makefile 的 IMAGE ?= newtg/gateway:dev 对齐）。
img_refs="$(count_of 'image: ${NEWTG_IMAGE:-newtg/gateway:dev}' "$compose")"
if [ "$img_refs" = "3" ]; then
  ok "gateway/worker/migrate 统一使用 \${NEWTG_IMAGE:-newtg/gateway:dev}"
else
  bad "gateway/worker/migrate 应统一使用 \${NEWTG_IMAGE:-newtg/gateway:dev}（匹配 $img_refs 处，期望 3）"
fi

# Caddy：live_feed 是 SSE，须在 /api/* 的压缩路由之前声明（handle 先匹配先生效）。
caddyfile="$root/deploy/edge/Caddyfile"
sse_home="$(line_of 'handle /api/home/live-feed' "$caddyfile")"
sse_public="$(line_of 'handle /api/public/live-feed' "$caddyfile")"
api_block="$(line_of 'handle /api/*' "$caddyfile")"
if [ -n "$sse_home" ] && [ -n "$sse_public" ] && [ -n "$api_block" ] \
   && [ "$sse_home" -lt "$api_block" ] && [ "$sse_public" -lt "$api_block" ]; then
  ok "Caddy SSE 路由先于 /api/*（不压缩）"
else
  bad "Caddy 的 /api/home/live-feed 与 /api/public/live-feed 必须声明在 /api/* 之前"
fi

edge_pub="$(q '.services.edge.ports[0].published // ""')"
edge_target="$(q '.services.edge.ports[0].target // ""')"
if [ "$edge_pub" = "8080" ] && [ "$edge_target" = "80" ]; then
  ok "edge 宿主端口 8080:80"
else
  bad "edge 宿主端口期望 8080:80（实际 ${edge_pub}:${edge_target}）"
fi

# DB 账号归属：gateway/worker 用 newtg_app，migrate 用 newtg_migrate（两个库均如此）。
for svc in gateway worker; do
  for key in DATABASE_URL DB_LOGS_DSN; do
    val="$(q ".services.${svc}.environment.${key} // \"\"")"
    if [[ "$val" == postgres://newtg_app:* ]]; then
      ok "services.$svc $key 使用 newtg_app"
    else
      bad "services.$svc $key 未使用 newtg_app"
    fi
  done
done
for key in DATABASE_URL DB_LOGS_DSN; do
  val="$(q ".services.migrate.environment.${key} // \"\"")"
  if [[ "$val" == postgres://newtg_migrate:* ]]; then
    ok "services.migrate $key 使用 newtg_migrate"
  else
    bad "services.migrate $key 未使用 newtg_migrate"
  fi
done

# --- i. 日志库 UPDATE 例外与分区预建（R-25/R-56/R-59/R-60/R-61） ---------
# usage_logs 是唯一需要 newtg_app UPDATE 的日志表，且只授五列；audit_logs
# 必须 append-only。运行期 worker（newtg_app）没有 DDL 权限，分区由迁移预建。
init_sql="$root/deploy/postgres-init/20-databases.sql"
if grep -Fq 'GRANT UPDATE (status, cost, refunded, error_class, error_message) ON usage_logs TO newtg_app' "$init_sql"; then
  ok "20-databases.sql 为 usage_logs 授予 newtg_app 五列 UPDATE（R-60）"
else
  bad "20-databases.sql 缺少 usage_logs 的五列 UPDATE 授权（R-60）"
fi
if grep -Eq 'GRANT[[:space:]]+UPDATE[^;]*ON[[:space:]]+audit_logs' "$init_sql"; then
  bad "audit_logs 不得授予 UPDATE（保持 append-only）"
else
  ok "audit_logs 未授予 UPDATE（append-only）"
fi
# initdb 早于建表，新库的实际授权与 12 个月分区预建由日志库迁移执行。
if grep -Fq 'GRANT UPDATE (status, cost, refunded, error_class, error_message) ON usage_logs TO newtg_app' "$root"/backend/migrations-logs/*.up.sql 2>/dev/null; then
  ok "日志库迁移授予 usage_logs 的五列 UPDATE（R-60）"
else
  bad "日志库迁移缺少 usage_logs 的五列 UPDATE 授权"
fi
if grep -Fq 'REVOKE UPDATE ON usage_logs FROM newtg_app' "$root"/backend/migrations-logs/*.up.sql 2>/dev/null; then
  ok "日志库迁移清除旧表级/旧列级授权（R-61）"
else
  bad "日志库迁移缺少旧授权清理（R-61）"
fi
if grep -Fq 'FOR i IN 0..11 LOOP' "$root"/backend/migrations-logs/*.up.sql 2>/dev/null; then
  ok "日志库迁移预建 12 个月 usage_logs 分区（R-56）"
else
  bad "日志库迁移缺少 12 个月分区预建（R-56）"
fi

if [ "$fail" -ne 0 ]; then
  echo "check_compose: FAIL" >&2
  exit 1
fi
echo "check_compose: PASS"
