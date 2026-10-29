#!/usr/bin/env bash
# chart 本地门禁：helm lint + 渲染 + 结构断言。
# 用法：bash deploy/k8s/newtg/verify.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
HELM="$ROOT/.tools/bin/helm"
CHART="$ROOT/deploy/k8s/newtg"

if [ ! -x "$HELM" ]; then
  echo "verify: 缺少 $HELM，先跑 bash scripts/install-helm.sh" >&2
  exit 1
fi

OUT="$(mktemp)"
trap 'rm -f "$OUT" "$OUT.mon"' EXIT

"$HELM" lint "$CHART" -f "$CHART/values-ci.yaml"

# initdb SQL 的唯一真源是 deploy/postgres-init/；chart 里那份是 `make
# chart-sync-init` 的复制物。改了源文件忘记同步，会在这里红。
if ! diff -r "$ROOT/deploy/postgres-init" "$CHART/files/postgres-init" >/dev/null; then
  echo "verify: deploy/k8s/newtg/files/postgres-init 与 deploy/postgres-init 不一致，跑 make chart-sync-init" >&2
  diff -r "$ROOT/deploy/postgres-init" "$CHART/files/postgres-init" >&2 || true
  exit 1
fi
# 备份脚本同理：唯一真源在 deploy/ops/backup/。
if ! diff "$ROOT/deploy/ops/backup/backup.sh" "$CHART/files/backup/backup.sh" >/dev/null; then
  echo "verify: deploy/k8s/newtg/files/backup/backup.sh 与 deploy/ops/backup/backup.sh 不一致，跑 make chart-sync-init" >&2
  diff "$ROOT/deploy/ops/backup/backup.sh" "$CHART/files/backup/backup.sh" >&2 || true
  exit 1
fi

"$HELM" template newtg "$CHART" -f "$CHART/values-ci.yaml" >"$OUT"
# 监控默认关：另渲染一份开着的结果，断言 ServiceMonitor 真的会渲染出来。
"$HELM" template newtg "$CHART" -f "$CHART/values-ci.yaml" --set monitoring.enabled=true >"$OUT.mon"

python3 - "$OUT" "$ROOT/deploy/postgres-init" "$CHART" <<'PY'
import pathlib
import sys
import yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
kinds = {(d.get("kind"), d.get("metadata", {}).get("name")) for d in docs}


def find(kind, name=None):
    for d in docs:
        if d.get("kind") != kind:
            continue
        if name is None or d.get("metadata", {}).get("name") == name:
            return d
    raise AssertionError(f"missing {kind} {name or ''}")


assert ("Namespace", "newtg") not in kinds, "Namespace 不该由 chart 渲染"
# 命名空间带着 PSA 标签单独 apply，chart 里没有它，所以在这里查源文件。
assert "pod-security.kubernetes.io/enforce: baseline" in pathlib.Path(sys.argv[3], "namespace.yaml").read_text()

# --- gateway 工作负载 -------------------------------------------------------
cfg = find("ConfigMap", "newtg-config")
# 信任链只能收紧：0.0.0.0/0 等于让 X-Forwarded-For 由客户端决定。
assert cfg["data"]["TRUSTED_PROXIES"] not in ("0.0.0.0/0", "*"), cfg["data"]["TRUSTED_PROXIES"]
assert cfg["data"]["DB_MIGRATE_ON_START"] == "false"

gw = find("Deployment", "gateway")["spec"]
assert gw["replicas"] == 2, gw["replicas"]
pod = gw["template"]["spec"]
assert pod["terminationGracePeriodSeconds"] == 35, pod["terminationGracePeriodSeconds"]
assert pod["securityContext"]["runAsNonRoot"] is True
spread = pod["topologySpreadConstraints"][0]
assert spread["topologyKey"] == "kubernetes.io/hostname"
assert spread["whenUnsatisfiable"] == "DoNotSchedule"

c = pod["containers"][0]
assert c["readinessProbe"]["httpGet"]["path"] == "/readyz"
assert c["livenessProbe"]["httpGet"]["path"] == "/healthz"
assert c["securityContext"]["allowPrivilegeEscalation"] is False
assert c["securityContext"]["capabilities"]["drop"] == ["ALL"]
env = {e["name"]: e for e in c["env"]}
assert env["RUN_MODE"]["value"] == "gateway"
# 应用进程只拿自己用的键；建库口令留在 PG 侧。
secret_keys = {e["valueFrom"]["secretKeyRef"]["key"] for e in c["env"] if "valueFrom" in e}
assert not any(k.startswith("POSTGRES_") for k in secret_keys), secret_keys
assert {"DATABASE_URL", "DB_LOGS_DSN", "REDIS_URL", "METRICS_TOKEN"} <= secret_keys, secret_keys

svc = find("Service", "gateway")["spec"]
assert svc["type"] == "ClusterIP"
assert {(p["name"], p["port"]) for p in svc["ports"]} >= {("http", 3000), ("metrics", 8006)}

pdb = find("PodDisruptionBudget", "gateway")["spec"]
assert pdb["maxUnavailable"] == 1, pdb["maxUnavailable"]

hpa = find("HorizontalPodAutoscaler", "gateway")["spec"]
assert hpa["minReplicas"] == 2 and hpa["maxReplicas"] == 8, hpa
# 连接预算（每副本所有连接池之和）由文件末尾的评审门禁统一核算。

# --- worker 与 migrate ------------------------------------------------------
wk = find("Deployment", "worker")["spec"]
assert wk["replicas"] == 1, wk["replicas"]
# 多副本 worker 会重复跑同一时刻的调度任务。
assert wk["strategy"]["type"] == "Recreate", wk["strategy"]
wpod = wk["template"]["spec"]
assert wpod["terminationGracePeriodSeconds"] == 35
wc = wpod["containers"][0]
wenv = {e["name"]: e.get("value") for e in wc["env"]}
assert wenv["RUN_MODE"] == "worker"
assert wc["readinessProbe"]["httpGet"]["path"] == "/readyz"
hpas = [d for d in docs if d.get("kind") == "HorizontalPodAutoscaler"]
assert all(h["spec"]["scaleTargetRef"]["name"] != "worker" for h in hpas), "worker 不该有 HPA"

job = find("Job", "newtg-migrate")
assert "pre-upgrade" in job["metadata"]["annotations"]["helm.sh/hook"], job["metadata"]["annotations"]
assert job["metadata"]["annotations"]["argocd.argoproj.io/sync-wave"] == "1"
assert job["spec"]["backoffLimit"] == 0, job["spec"]["backoffLimit"]
assert job["spec"]["template"]["spec"]["restartPolicy"] == "Never"
jc = job["spec"]["template"]["spec"]["containers"][0]
jenv = {e["name"]: e for e in jc["env"]}
assert jenv["RUN_MODE"]["value"] == "migrate"
assert jenv["DB_MIGRATE_ON_START"]["value"] == "false"
# 迁移必须用 newtg_migrate 身份：应用身份的 DSN 不该出现在 Job 里。
assert jenv["DATABASE_URL"]["valueFrom"]["secretKeyRef"]["key"] == "MIGRATE_DATABASE_URL"
assert jenv["DB_LOGS_DSN"]["valueFrom"]["secretKeyRef"]["key"] == "MIGRATE_DB_LOGS_DSN"

# --- frontend ---------------------------------------------------------------
fe = find("Deployment", "frontend")["spec"]
assert fe["replicas"] == 2, fe["replicas"]
fpod = fe["template"]["spec"]
fspread = fpod["topologySpreadConstraints"][0]
assert fspread["whenUnsatisfiable"] == "DoNotSchedule", fspread
fc = fpod["containers"][0]
assert "newtg/frontend" in fc["image"], fc["image"]
# 镜像监听 8080，Service 对外 80：两个数字写反就是全站 502。
assert fc["ports"][0]["containerPort"] == 8080, fc["ports"]
assert fc["ports"][0]["name"] == "http"
assert fpod["securityContext"]["runAsUser"] == 101
assert fc["securityContext"]["readOnlyRootFilesystem"] is True
# 存活探针不得依赖后端，否则 gateway 滚动会连带重启前端。
assert fc["livenessProbe"]["httpGet"]["path"] == "/robots.txt"
assert any(v["name"] == "tmp" and "emptyDir" in v for v in fpod["volumes"])

fsvc = find("Service", "frontend")["spec"]
assert [(p["name"], p["port"], p["targetPort"]) for p in fsvc["ports"]] == [("http", 80, "http")], fsvc["ports"]

fpdb = find("PodDisruptionBudget", "frontend")["spec"]
assert fpdb["maxUnavailable"] == 1, fpdb["maxUnavailable"]

# --- Ingress 与证书 ---------------------------------------------------------
def ingress_paths(doc):
    out = []
    for rule in doc["spec"]["rules"]:
        for p in rule["http"]["paths"]:
            out.append((p["path"], p["backend"]["service"]["name"], p["backend"]["service"]["port"]["name"]))
    return out


main = find("Ingress", "newtg")
stream = find("Ingress", "newtg-stream")
mpaths = ingress_paths(main)
spaths = ingress_paths(stream)
assert ("/", "frontend", "http") in mpaths, mpaths
assert ("/api", "gateway", "http") in mpaths, mpaths

# SSE 路径必须关缓冲并给足超时，否则 token 会攒到缓冲区满才下发、60s 被切断。
sann = stream["metadata"]["annotations"]
assert sann["nginx.ingress.kubernetes.io/proxy-buffering"] == "off", sann
assert sann["nginx.ingress.kubernetes.io/proxy-read-timeout"] == "3600", sann
assert sann["nginx.ingress.kubernetes.io/proxy-send-timeout"] == "3600", sann
assert {"/v1", "/v1beta", "/pg"} <= {p for p, svc, port in spaths if svc == "gateway"}, spaths

for ing in (main, stream):
    assert ing["spec"]["tls"][0]["secretName"], ing["metadata"]["name"]
    assert ing["spec"]["ingressClassName"] == "nginx"
    assert ing["spec"]["rules"][0]["host"] == "app.example.com"

# Review Focus 4：中继请求绝不能被重放（重复计费）。任何注解都不许打开重试。
for d in docs:
    for k, v in (d.get("metadata", {}).get("annotations") or {}).items():
        assert k not in ("nginx.ingress.kubernetes.io/proxy-next-upstream",
                         "nginx.ingress.kubernetes.io/app-root"), {k: v}
        assert "non_idempotent" not in str(v), {k: v}

cert = find("Certificate", "newtg-tls")["spec"]
assert cert["dnsNames"] == ["app.example.com"], cert["dnsNames"]
assert cert["secretName"] == main["spec"]["tls"][0]["secretName"], cert["secretName"]

# --- PostgreSQL -------------------------------------------------------------
pg = find("StatefulSet", "postgres")["spec"]
assert pg["serviceName"] == "postgres"
assert pg["replicas"] == 1
assert pg["podManagementPolicy"] == "OrderedReady"
pgc = pg["template"]["spec"]["containers"][0]
# 连接上限必须和 HPA 的连接预算对得上，否则扩容会把 PG 打到 max_connections。
assert "max_connections=256" in pgc["args"], pgc["args"]
vct = pg["volumeClaimTemplates"]
assert len(vct) == 1, len(vct)
assert vct[0]["spec"]["storageClassName"] == "local-path", vct[0]["spec"]
pgsvc = find("Service", "postgres")["spec"]
assert pgsvc["clusterIP"] == "None", pgsvc  # headless
assert [(p["name"], p["port"]) for p in pgsvc["ports"]] == [("postgres", 5432)]
assert find("PodDisruptionBudget", "postgres")["spec"]["minAvailable"] == 1

init = find("ConfigMap", "newtg-postgres-init")["data"]
assert set(init) == {"10-roles.sql", "20-databases.sql", "30-metrics-db.sql"}, set(init)
# 渲染结果必须与真源逐字节相同：缩进/块标量写错会让 initdb 静默不建角色。
for name, body in init.items():
    src = pathlib.Path(sys.argv[2], name).read_text()
    assert body == src, f"{name} 渲染结果与 deploy/postgres-init/{name} 不一致"

# --- Redis ------------------------------------------------------------------
rd = find("StatefulSet", "redis")["spec"]
assert rd["serviceName"] == "redis" and rd["replicas"] == 1
rc = rd["template"]["spec"]["containers"][0]
cmd = " ".join(rc["args"])
assert "--port 0" in cmd, cmd            # 明文端口必须关掉
assert "--tls-port 6379" in cmd, cmd
assert "--appendonly yes" in cmd, cmd
# 口令只能是变量引用；字面量口令会泄漏进 kubectl describe 与 PodSpec。
assert '--requirepass "$REDIS_PASSWORD"' in cmd, cmd
assert rc["env"][0]["valueFrom"]["secretKeyRef"]["key"] == "REDIS_PASSWORD"
assert len(rd["volumeClaimTemplates"]) == 1
assert rd["volumeClaimTemplates"][0]["spec"]["storageClassName"] == "local-path"
rsvc = find("Service", "redis")["spec"]
assert rsvc["clusterIP"] == "None", rsvc
assert [(p["name"], p["port"]) for p in rsvc["ports"]] == [("redis", 6379)]
assert find("PodDisruptionBudget", "redis")["spec"]["minAvailable"] == 1

# --- NetworkPolicy ----------------------------------------------------------
nps = [d for d in docs if d.get("kind") == "NetworkPolicy"]
assert len(nps) >= 5, len(nps)
by_name = {n["metadata"]["name"]: n for n in nps}

deny = by_name["default-deny-ingress"]["spec"]
assert deny["podSelector"] == {}, deny["podSelector"]
assert deny["policyTypes"] == ["Ingress"]

# Review Focus 3 的钉子：kubelet 探针必须有一个明确的节点网段来源。
cidrs = [r["ipBlock"]["cidr"]
         for n in nps for rule in n["spec"].get("ingress") or []
         for r in rule.get("from") or [] if "ipBlock" in r]
assert cidrs == ["10.0.20.0/24"], cidrs
# 入站侧绝不能有「整网放行」；0.0.0.0/0 只允许出现在 worker 的出站规则里。
for n in nps:
    for rule in n["spec"].get("ingress") or []:
        for r in rule.get("from") or []:
            assert r.get("ipBlock", {}).get("cidr") != "0.0.0.0/0", n["metadata"]["name"]

gw_np = by_name["allow-ingress-controller"]["spec"]
sel = gw_np["ingress"][0]["from"][0]["namespaceSelector"]["matchLabels"]
assert sel["kubernetes.io/metadata.name"] == "ingress-nginx", sel
assert 3000 in {p["port"] for p in gw_np["ingress"][0]["ports"]}

fe_np = by_name["allow-ingress-controller-to-frontend"]["spec"]
assert {8080, 80} <= {p["port"] for p in fe_np["ingress"][0]["ports"]}

data_np = by_name["allow-app-to-data"]["spec"]
srcs = data_np["ingress"][0]["from"][0]["podSelector"]["matchExpressions"][0]["values"]
assert set(srcs) == {"gateway", "worker", "migrate", "backup"}, srcs
# newtg_app 以外的身份不该出现在数据层的入站来源里（例如 frontend）。
assert "frontend" not in srcs

wk_np = by_name["worker-egress"]["spec"]
assert wk_np["policyTypes"] == ["Egress"]
eg = wk_np["egress"]
assert any("ipBlock" in t and t["ipBlock"]["cidr"] == "0.0.0.0/0"
           for rule in eg for t in rule["to"]), eg
# 有 Egress 策略就必须显式放行 DNS，否则 worker 解析不了任何域名。
assert any(53 in {p["port"] for p in rule["ports"]} for rule in eg), eg

print(f"ok: {len(docs)} docs")

# --- 备份与监控 -------------------------------------------------------------
cronjobs = {d["metadata"]["name"]: d for d in docs if d.get("kind") == "CronJob"}
assert set(cronjobs) == {"newtg-backup-hourly", "newtg-backup-daily", "newtg-backup-monthly"}, set(cronjobs)
for name, cj in cronjobs.items():
    jc = cj["spec"]["jobTemplate"]["spec"]
    c = jc["template"]["spec"]["containers"][0]
    benv = {e["name"]: e for e in c["env"]}
    # 主库与日志库各一份：日志库不可静默跳过。
    assert benv["NEWTG_MAIN_DSN"]["valueFrom"]["secretKeyRef"]["key"] == "BACKUP_DATABASE_URL", name
    assert benv["NEWTG_LOGS_DSN"]["valueFrom"]["secretKeyRef"]["key"] == "BACKUP_DB_LOGS_DSN", name
    assert benv["LABEL"]["value"] == name.split("-")[-1], name
    # 没有接收者就是把明文数据库倒进对象存储。
    assert benv["AGE_RECIPIENT"]["value"], name
    assert jc["backoffLimit"] == 1
    assert cj["spec"]["concurrencyPolicy"] == "Forbid"
    assert c["securityContext"]["readOnlyRootFilesystem"] is True

# 默认不渲染 ServiceMonitor：没装 CRD 的集群会整份 chart 装不上。
assert "ServiceMonitor" not in {d.get("kind") for d in docs}, "默认不该渲染 ServiceMonitor"
PY

python3 - "$OUT.mon" <<'PY2'
import sys
import yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
sm = [d for d in docs if d.get("kind") == "ServiceMonitor"]
assert len(sm) == 2, len(sm)
for each in sm:
    ep = each["spec"]["endpoints"][0]
    assert ep["port"] == "metrics" and ep["path"] == "/metrics", ep
    assert ep["authorization"]["credentials"]["key"] == "METRICS_TOKEN", ep
print("ok: ServiceMonitor（gateway + worker）渲染且带 METRICS_TOKEN 鉴权")
PY2

# --- 整分支评审的修复门禁 -----------------------------------------------------
# 这些断言对应评审里的 Critical / Important 条目，每一条都描述一种「本地
# helm template 通过、真上集群才炸」的情形，因此必须在这里钉死。
python3 - "$OUT" "$OUT.mon" <<'PY3'
import sys
import yaml

main = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
mon = [d for d in yaml.safe_load_all(open(sys.argv[2])) if d]
fails = []


def check(cond, msg):
    if not cond:
        fails.append(msg)


def find(docs, kind, name=None):
    for d in docs:
        if d.get("kind") != kind:
            continue
        if name is None or d["metadata"]["name"] == name:
            return d
    return None


# 1) Namespace 不能由 chart 渲染：`--create-namespace` 先建同名命名空间，
#    随后提交清单里的 Namespace 对象会 409；反过来手工建又没有 helm 归属
#    元数据，会在 existingResourceConflict 处被拒。它必须留在 chart 之外。
check(find(main, "Namespace") is None,
      "chart 不该渲染 Namespace（会与 --create-namespace / 归属检查冲突）；见 deploy/k8s/newtg/namespace.yaml")

# 2) 首次 install 时 PG 还不存在：pre-install hook 必然连不上库。
job = find(main, "Job", "newtg-migrate")
hook = job["metadata"]["annotations"]["helm.sh/hook"]
check("post-install" in hook, f"migrate 必须 post-install 才有 PG 可连：{hook}")
check("pre-install" not in hook, f"migrate 不能 pre-install：{hook}")
init = job["spec"]["template"]["spec"].get("initContainers") or []
check(any("pg_isready" in " ".join(c.get("command", [])) for c in init),
      "migrate 需要等 PG 可连的 initContainer（post-install 时 StatefulSet 刚提交）")

# 3) 备份 Pod 必须能连数据层，否则三层备份一次都不会成功。
np_src = find(main, "NetworkPolicy", "allow-app-to-data")["spec"]["ingress"][0]["from"][0]["podSelector"]["matchExpressions"][0]["values"]
check("backup" in np_src, f"NetworkPolicy 漏了 backup，CronJob 连不上 PG：{np_src}")

# 4) 跨机 PG 必须 TLS（spec D4）：服务端开 ssl，客户端挂 CA。
pg = find(main, "StatefulSet", "postgres")["spec"]["template"]["spec"]
pgc = pg["containers"][0]
args = " ".join(pgc["args"])
check("ssl=on" in args, f"postgres 未开 TLS：{args}")
check("ssl_cert_file=" in args and "ssl_key_file=" in args and "ssl_ca_file=" in args, args)
check(any("tls" in m["name"] for m in pgc["volumeMounts"]), pgc["volumeMounts"])
gw = find(main, "Deployment", "gateway")["spec"]["template"]["spec"]["containers"][0]
check(any(m.get("mountPath", "").startswith("/etc/ssl/postgres") for m in gw.get("volumeMounts") or []),
      "gateway 没挂 PG CA，DSN 无法用 sslmode=verify-full")

# 5) 连接预算必须按「每进程所有池之和」算：gateway 主库+日志库+指标库(5)，
#    worker 再多一个保留池。只按一个池算会低估约 2 倍。
cfg = find(main, "ConfigMap", "newtg-config")["data"]
x = int(cfg["DB_MAX_OPEN_CONNS"])
hpa = find(main, "HorizontalPodAutoscaler", "gateway")["spec"]
pg_max = int(find(main, "StatefulSet", "postgres")["spec"]["template"]["spec"]["containers"][0]["args"][1].split("=")[1])
budget = hpa["maxReplicas"] * (2 * x + 5) + 3 * x
check(budget <= pg_max, f"连接预算超了：{budget} > {pg_max}（X={x}，含保留池）")

# 6) 指标端口要能从 Prometheus 命名空间进来，否则 ServiceMonitor 全是 down。
prom = [n for n in main if n.get("kind") == "NetworkPolicy"
        and any(s.get("namespaceSelector", {}).get("matchLabels", {}).get("kubernetes.io/metadata.name") == "monitoring"
                for r in n["spec"].get("ingress") or [] for s in r.get("from") or [])]
check(prom and any(8006 in {p["port"] for r in p["spec"]["ingress"] for p in r.get("ports") or []} for p in prom),
      "没有放行 Prometheus → :8006 的 NetworkPolicy，ServiceMonitor 抓不到")

# 7) Redis 必须能按 values 钉到数据节点：只给 PG 有 nodeSelector 时，给
#    data-redis 打污点后 Redis 直接 Pending。
rs = find(main, "StatefulSet", "redis")["spec"]["template"]["spec"]
check(rs.get("nodeSelector"), "redis 没有 nodeSelector，无法钉到数据节点")
check(rs.get("tolerations"), "redis 没有 tolerations，数据节点打污点后直接 Pending")

# 8) worker 的指标端口要能被发现：Service + 独立 ServiceMonitor。
wsvc = find(main, "Service", "worker")
check(wsvc and any(p["name"] == "metrics" for p in wsvc["spec"]["ports"]), "worker 缺 metrics Service")
sms = [d for d in mon if d.get("kind") == "ServiceMonitor"]
check(any(s["spec"]["selector"]["matchLabels"].get("app.kubernetes.io/name") == "worker" for s in sms),
      "没有 worker 的 ServiceMonitor，worker 指标采集不到")

# 9) liveness 不许查依赖：单副本 worker 被 DB 抖动重启会停掉计费兜底。
wc = find(main, "Deployment", "worker")["spec"]["template"]["spec"]["containers"][0]
check(wc["livenessProbe"]["httpGet"]["path"] == "/healthz",
      f"worker liveness 该探 /healthz：{wc['livenessProbe']['httpGet']['path']}")

if fails:
    print("verify: 修复门禁未通过", file=sys.stderr)
    for f in fails:
        print(f"  - {f}", file=sys.stderr)
    sys.exit(1)
print(f"ok: 评审门禁 {9} 条")
PY3
