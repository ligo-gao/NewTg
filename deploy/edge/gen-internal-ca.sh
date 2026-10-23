#!/usr/bin/env bash
#
# 生成 NEWTG edge 的内部 CA（根证书 + 叶证书），默认只写入 staging 目录
# deploy/edge/certs-internal-ca/，绝不动线上 deploy/edge/certs/。
#
# 用法：
#   ./gen-internal-ca.sh            # 只在 certs-internal-ca/ 生成/续签叶证书（默认，安全）
#   ./gen-internal-ca.sh --apply    # 备份线上证书后，把新叶证书复制到 certs/
#   ./gen-internal-ca.sh --force    # 轮换 staging 根 CA（所有客户端需重新信任）
#   ./gen-internal-ca.sh --help
#
# 设计依据：docs/2026-10-23-NEWTG审核改进计划.md 阶段 0/1.5 与第四节「HTTPS」决策。
# Let's Encrypt 无法为内网 IP 签发证书，因此内网使用「内部 CA + 分发根证书」。
#
# 根证书（newtg-root-ca.crt）是每个客户端必须安装并信任的文件；叶证书
# （newtg.crt/newtg.key）只部署在 edge 上。默认复用已存在的根 CA，只重新签发
# 叶证书——这样重跑不会轮换信任；轮换根 CA 必须显式加 --force。

set -euo pipefail
umask 077

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
EDGE_DIR="$SCRIPT_DIR"
LIVE_CERTS_DIR="$EDGE_DIR/certs"
OUT_DIR="${NEWTG_INTERNAL_CA_DIR:-$EDGE_DIR/certs-internal-ca}"

ROOT_KEY="$OUT_DIR/newtg-root-ca.key"
ROOT_CRT="$OUT_DIR/newtg-root-ca.crt"
LEAF_KEY="$OUT_DIR/newtg.key"
LEAF_CSR="$OUT_DIR/newtg.csr"
LEAF_CRT="$OUT_DIR/newtg.crt"
LEAF_EXT="$OUT_DIR/newtg.ext"

SAN="IP:10.211.55.5,DNS:localhost,IP:127.0.0.1"
ROOT_DAYS=3650
LEAF_DAYS=365

APPLY=0
FORCE=0

usage() {
	cat <<'EOF'
用法: gen-internal-ca.sh [--apply] [--force]

  (无参数)   在 staging 目录 certs-internal-ca/ 生成根 CA（若不存在）并重新
             签发叶证书。已存在的根 CA 默认复用，不轮换客户端信任。
  --apply    生成后备份 deploy/edge/certs/ 的 newtg.crt/newtg.key 到
             backup-<UTC 时间戳>/，再把新叶证书复制进 deploy/edge/certs/。
             只打印重启命令，不自动执行。
  --force    轮换根 CA（会覆盖 staging 根证书；所有客户端必须重新安装信任）。
  -h, --help 显示本帮助。

环境变量:
  NEWTG_INTERNAL_CA_DIR  覆盖 staging 输出目录（默认 deploy/edge/certs-internal-ca）。
EOF
}

for arg in "$@"; do
	case "$arg" in
	--apply) APPLY=1 ;;
	--force) FORCE=1 ;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "错误：未知参数：$arg" >&2
		usage >&2
		exit 2
		;;
	esac
done

if ! command -v openssl >/dev/null 2>&1; then
	echo "错误：未找到 openssl。请先安装 openssl（内部 CA 生成依赖它）。" >&2
	exit 1
fi

# canonical_path 规范化路径（允许目标不存在）。优先 realpath -m；否则对已存在
# 目录用 cd + pwd -P，对不存在的目标只创建其父目录再拼接。
canonical_path() {
	local p="$1" resolved
	# 尝试 GNU `realpath -m`（允许目标不存在）；不支持时走下面的可移植回退。
	if resolved="$(realpath -m -- "$p" 2>/dev/null)"; then
		printf '%s\n' "$resolved"
		return
	fi
	if [[ -d "$p" ]]; then
		(cd -- "$p" && pwd -P)
		return
	fi
	local parent base
	parent="$(dirname -- "$p")"
	base="$(basename -- "$p")"
	mkdir -p -- "$parent"
	printf '%s/%s\n' "$(cd -- "$parent" && pwd -P)" "$base"
}

# 安全闸门：输出目录绝不能落在线上证书目录内。脚本会 rm staging 文件，
# 若 OUT_DIR 指向 deploy/edge/certs，会静默删掉线上 key/crt。
OUT_DIR_CANON="$(canonical_path "$OUT_DIR")"
LIVE_CERTS_CANON="$(canonical_path "$LIVE_CERTS_DIR")"
case "$OUT_DIR_CANON/" in
"$LIVE_CERTS_CANON"/*)
	echo "错误：输出目录解析为线上证书目录或其子目录：$OUT_DIR_CANON" >&2
	echo "拒绝在此写入或删除文件。线上证书替换只能通过 --apply（会先备份）。" >&2
	echo "请改用其他 staging 目录（默认 deploy/edge/certs-internal-ca/）。" >&2
	exit 1
	;;
esac

ROOT_EXISTS=0
if [[ -f "$ROOT_KEY" && -f "$ROOT_CRT" ]]; then
	ROOT_EXISTS=1
elif [[ -e "$ROOT_KEY" || -e "$ROOT_CRT" ]]; then
	echo "错误：staging 目录中的根证书不完整（key 与 crt 只有一个）：$OUT_DIR" >&2
	echo "请人工检查；确认重建可删除该目录后重跑。" >&2
	exit 1
fi

mkdir -p "$OUT_DIR"

if (( ROOT_EXISTS == 1 )) && (( FORCE == 1 )); then
	echo "警告：--force 已启用，将轮换根 CA；已安装旧根证书的客户端会全部失效。" >&2
	rm -f "$ROOT_KEY" "$ROOT_CRT" "${ROOT_CRT%.crt}.srl"
	ROOT_EXISTS=0
fi

if (( ROOT_EXISTS == 1 )); then
	echo "==> 复用已有根 CA（不轮换信任）：$ROOT_CRT"
else
	echo "==> 生成根 CA（有效期 ${ROOT_DAYS} 天）"
	openssl req -x509 -newkey rsa:4096 -nodes -sha256 -days "$ROOT_DAYS" \
		-keyout "$ROOT_KEY" -out "$ROOT_CRT" \
		-subj "/CN=NEWTG Internal Root CA/O=NEWTG" \
		-addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
		-addext "keyUsage=critical,keyCertSign,cRLSign" \
		-addext "subjectKeyIdentifier=hash"
fi

# 复用分支同样收紧权限（历史文件或手工操作可能放宽过）。
chmod 600 "$ROOT_KEY"
chmod 644 "$ROOT_CRT"

echo "==> 生成叶证书（SAN=${SAN}，有效期 ${LEAF_DAYS} 天）"
rm -f "$LEAF_KEY" "$LEAF_CSR" "$LEAF_CRT" "$LEAF_EXT"
openssl req -newkey rsa:2048 -nodes -sha256 \
	-keyout "$LEAF_KEY" -out "$LEAF_CSR" \
	-subj "/CN=10.211.55.5/O=NEWTG"
chmod 600 "$LEAF_KEY"

cat >"$LEAF_EXT" <<EOF
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=${SAN}
EOF

openssl x509 -req -in "$LEAF_CSR" -CA "$ROOT_CRT" -CAkey "$ROOT_KEY" \
	-CAcreateserial -out "$LEAF_CRT" -days "$LEAF_DAYS" -sha256 \
	-extfile "$LEAF_EXT"
rm -f "$LEAF_CSR" "$LEAF_EXT"
chmod 644 "$LEAF_CRT"

echo "==> 校验"
openssl verify -CAfile "$ROOT_CRT" "$LEAF_CRT"
openssl x509 -in "$LEAF_CRT" -noout -subject -issuer -dates -ext subjectAltName

echo
echo "staging 输出目录：$OUT_DIR"
ls -l "$ROOT_CRT" "$ROOT_KEY" "$LEAF_CRT" "$LEAF_KEY"

if (( APPLY == 1 )); then
	if [[ ! -f "$LIVE_CERTS_DIR/newtg.crt" || ! -f "$LIVE_CERTS_DIR/newtg.key" ]]; then
		echo "错误：$LIVE_CERTS_DIR 缺少 newtg.crt/newtg.key，拒绝 --apply。" >&2
		exit 1
	fi
	STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
	BACKUP_DIR="$LIVE_CERTS_DIR/backup-$STAMP"
	mkdir -p "$BACKUP_DIR"
	cp -a "$LIVE_CERTS_DIR/newtg.crt" "$LIVE_CERTS_DIR/newtg.key" "$BACKUP_DIR/"
	echo
	echo "==> 已备份线上证书到 $BACKUP_DIR"
	# 原子替换：先写同目录临时文件，再 rename 覆盖；避免 Caddy 读到半写文件。
	TMP_CRT="$LIVE_CERTS_DIR/.newtg.crt.tmp"
	TMP_KEY="$LIVE_CERTS_DIR/.newtg.key.tmp"
	cleanup_tmp() { rm -f "$TMP_CRT" "$TMP_KEY"; }
	trap cleanup_tmp EXIT
	apply_leaf() {
		install -m 644 "$LEAF_CRT" "$TMP_CRT"
		install -m 600 "$LEAF_KEY" "$TMP_KEY"
		mv -f "$TMP_CRT" "$LIVE_CERTS_DIR/newtg.crt"
		mv -f "$TMP_KEY" "$LIVE_CERTS_DIR/newtg.key"
	}
	if ! apply_leaf; then
		echo "错误：写入线上证书失败。备份在 $BACKUP_DIR；恢复方法：" >&2
		echo "  cp -a \"$BACKUP_DIR/newtg.crt\" \"$BACKUP_DIR/newtg.key\" \"$LIVE_CERTS_DIR/\"" >&2
		exit 1
	fi
	trap - EXIT
	echo "==> 已把新叶证书复制到 $LIVE_CERTS_DIR/（尚未重载 Caddy）"
else
	echo
	echo "==> 未改动线上目录 $LIVE_CERTS_DIR（默认行为）。"
fi

echo
echo "下一步（运维）："
echo "  1) 把根证书安装到每个客户端并加入信任库（这是必须信任的文件）："
echo "       $ROOT_CRT"
echo "     Linux:   sudo cp \"$ROOT_CRT\" /usr/local/share/ca-certificates/newtg-root-ca.crt && sudo update-ca-certificates"
echo "     macOS:   「钥匙串访问」→ 系统 → 导入 → 始终信任"
echo "     Windows: 导入到「受信任的根证书颁发机构」"
if (( APPLY == 0 )); then
	echo "  2) 客户端信任根证书后，替换线上叶证书（会先自动备份）："
	echo "       $SCRIPT_DIR/gen-internal-ca.sh --apply"
fi
echo "  3) 重启 edge 使新证书生效（Caddyfile 设了 admin off，reload 不可用；脚本不会自动执行）："
echo "       docker restart newtg-edge-1"
echo "  4) 校验："
echo "       curl --cacert \"$ROOT_CRT\" https://10.211.55.5:8443/readyz"
