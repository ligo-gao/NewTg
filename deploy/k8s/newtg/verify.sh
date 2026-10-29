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
trap 'rm -f "$OUT"' EXIT

"$HELM" lint "$CHART" -f "$CHART/values-ci.yaml"
"$HELM" template newtg "$CHART" -f "$CHART/values-ci.yaml" >"$OUT"

python3 - "$OUT" <<'PY'
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


assert ("Namespace", "newtg") in kinds, kinds
ns = find("Namespace", "newtg")
assert ns["metadata"]["labels"]["pod-security.kubernetes.io/enforce"] == "baseline"

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
# 副本上限 × 每副本连接上限 必须留出 worker/migrate 的余量（PG 基线 256）。
assert hpa["maxReplicas"] * int(cfg["data"]["DB_MAX_OPEN_CONNS"]) <= 200

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
assert job["metadata"]["annotations"]["argocd.argoproj.io/sync-wave"] == "-1"
assert job["spec"]["backoffLimit"] == 0, job["spec"]["backoffLimit"]
assert job["spec"]["template"]["spec"]["restartPolicy"] == "Never"
jc = job["spec"]["template"]["spec"]["containers"][0]
jenv = {e["name"]: e for e in jc["env"]}
assert jenv["RUN_MODE"]["value"] == "migrate"
assert jenv["DB_MIGRATE_ON_START"]["value"] == "false"
# 迁移必须用 newtg_migrate 身份：应用身份的 DSN 不该出现在 Job 里。
assert jenv["DATABASE_URL"]["valueFrom"]["secretKeyRef"]["key"] == "MIGRATE_DATABASE_URL"
assert jenv["DB_LOGS_DSN"]["valueFrom"]["secretKeyRef"]["key"] == "MIGRATE_DB_LOGS_DSN"

print(f"ok: {len(docs)} docs")
PY
