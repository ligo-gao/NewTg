#!/usr/bin/env bash
# 安装固定版本 helm 到仓库内 .tools/（已 gitignore），不污染系统 PATH。
# 用法：bash scripts/install-helm.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HELM_VERSION="v3.16.4"
DEST="$ROOT/.tools"
BIN="$DEST/bin/helm"

if [ -x "$BIN" ]; then
  "$BIN" version --short
  exit 0
fi

case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *)
    echo "install-helm: unsupported arch $(uname -m)" >&2
    exit 1
    ;;
esac

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

URL="https://get.helm.sh/helm-${HELM_VERSION}-linux-${ARCH}.tar.gz"
echo "install-helm: downloading $URL"
curl -fsSL "$URL" -o "$TMP/helm.tgz"
tar -xzf "$TMP/helm.tgz" -C "$TMP"

mkdir -p "$DEST/bin"
mv "$TMP/linux-${ARCH}/helm" "$BIN"
chmod +x "$BIN"
"$BIN" version --short
