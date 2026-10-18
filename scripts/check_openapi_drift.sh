#!/usr/bin/env bash
# OpenAPI 生成物漂移检查（platform-03 §11.4-4）：
# 重跑 bundle + 双端代码生成，生成物必须与入库版本逐字一致。
# 版本与 Makefile 同源（scripts/openapi.env）。
set -euo pipefail

cd "$(dirname "$0")/.."

# shellcheck disable=SC1091
. scripts/openapi.env

echo "==> bundle: api/openapi/openapi.yaml → api/dist/openapi.bundle.yaml"
npx --yes "@redocly/cli@${REDOCLY_VERSION}" bundle api/openapi/openapi.yaml -o api/dist/openapi.bundle.yaml

echo "==> 生成前端类型: frontend/src/api/schema.d.ts"
npx --yes "openapi-typescript@${OPENAPI_TS_VERSION}" api/dist/openapi.bundle.yaml -o frontend/src/api/schema.d.ts

echo "==> 生成后端 DTO: backend/internal/api/dto/models.gen.go"
(
  cd backend
  GOPROXY="${NEWTG_GOPROXY:-https://goproxy.cn,direct}" go run \
    "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@${OAPI_CODEGEN_VERSION}" \
    -generate types -package dto \
    -o internal/api/dto/models.gen.go \
    ../api/dist/openapi.bundle.yaml
)

if ! git diff --exit-code -- api/dist frontend/src/api/schema.d.ts backend/internal/api/dto; then
  echo "FAIL: OpenAPI 生成物存在漂移——spec 变更必须与生成物同一次提交重新生成" >&2
  exit 1
fi

echo "PASS: OpenAPI 生成物无漂移"
