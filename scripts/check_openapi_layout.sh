#!/usr/bin/env bash
# 校验 OpenAPI 源目录树与入口 $ref 汇总（platform-03 §9.1 / C-6·D8：仓根 api/，非 backend/api/）。
# 失败即非零退出；供 CI 与本地 `make api-check` 调用。
set -euo pipefail

cd "$(dirname "$0")/.."

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

ENTRY=api/openapi/openapi.yaml

[ -f "$ENTRY" ] || fail "缺少入口 $ENTRY"
[ ! -e backend/api/openapi.yaml ] || fail "旧路径 backend/api/openapi.yaml 仍存在（契约源必须在仓根 api/openapi/）"

# 1) 五个公共组件文件
for f in security problem pagination params responses; do
  [ -f "api/openapi/components/$f.yaml" ] || fail "缺少 api/openapi/components/$f.yaml"
done

# 2) 四个域 paths + schemas 文件（另含 ops 探测域）
for d in ops public user admin relay; do
  [ -f "api/openapi/paths/$d.yaml" ] || fail "缺少 api/openapi/paths/$d.yaml"
done
for d in public user admin relay; do
  [ -f "api/openapi/schemas/$d.yaml" ] || fail "缺少 api/openapi/schemas/$d.yaml"
done

# 3) 入口必须是 OpenAPI 3.1
grep -qE '^openapi: 3\.1\.[0-9]+$' "$ENTRY" || fail "入口 openapi 版本必须为 3.1.x"

# 4) 入口 paths 下每一项都必须是 $ref（不允许内联 path item）
awk '
  /^paths:[[:space:]]*$/ { inpaths = 1; next }
  inpaths && /^[^[:space:]#]/ { inpaths = 0 }
  inpaths && /^  \// { paths++ }
  inpaths && /\$ref:/ { refs++ }
  END { if (paths == 0 || paths != refs) exit 1 }
' "$ENTRY" || fail "入口 paths 未全部使用 \$ref 汇总（每条 path 一个 \$ref）"

# 5) 五个公共组件与四个域 paths 文件均须被入口引用
for c in security problem pagination params responses; do
  grep -q "components/$c.yaml" "$ENTRY" || fail "入口未引用 components/$c.yaml"
done
for d in ops public user admin relay; do
  grep -q "paths/$d.yaml" "$ENTRY" || fail "入口未引用 paths/$d.yaml"
done
for d in public user admin; do
  grep -q "schemas/$d.yaml" "$ENTRY" || fail "入口未引用 schemas/$d.yaml"
done

# 6) relay schema 源文件存在但不承载协议 schema（§9.3：真源在 contract/adapter）
grep -q 'x-codegen: false' api/openapi/paths/relay.yaml || fail "relay paths 必须打 x-codegen: false"

echo "PASS: OpenAPI layout 校验通过（仓根 api/openapi/，入口 paths 全为 \$ref）"
