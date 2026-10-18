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
#   f. gateway healthcheck 走内置子命令 `/newtg healthcheck`（GC-6/R-07）。
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
command -v jq >/dev/null 2>&1 || die "jq 不可用（用于解析 docker compose config --format json）"

rendered=""
if ! rendered="$(cd "$root/deploy" && docker compose -f "$compose" config --format json 2>&1)"; then
  printf '%s\n' "$rendered" >&2
  die "docker compose config 渲染失败"
fi

q() { jq -r "$1" <<<"$rendered"; }

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

if [ "$fail" -ne 0 ]; then
  echo "check_compose: FAIL" >&2
  exit 1
fi
echo "check_compose: PASS"
