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

print(f"ok: {len(docs)} docs")
PY
