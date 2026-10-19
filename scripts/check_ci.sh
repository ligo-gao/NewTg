#!/usr/bin/env bash
# check_ci.sh —— CI workflow 静态验收（Task 15；真源 platform-02 §7 / platform-03 §11.4）
#
# 为什么需要这个脚本：GitHub Actions 无法在本地执行，workflow 的供应链与版本纪律
# 只能靠静态断言守住。任何浮动 action 标签、旧版 PG 或漏掉的阻断 job 都会在
# 合并前被机械拦下，而不是等到第一次真实运行才发现。
#
# 断言项：
#   a. .github/workflows/{ci,release}.yml 存在；
#   b. 每条 `uses:` 均固定到 40 位 commit SHA（GC-13；禁 @v1/@main/@latest 等浮动标签）；
#   c. 两个 workflow 零 `postgres:15`（GC-4 / C-2③ / D4：统一 PG16）；
#   d. ci.yml 的 env 固定声明 MIGRATE_VERSION / KIN_OPENAPI_VERSION（GC-13，禁 @latest）；
#   e. ci.yml 八个阻断 job 齐全，release.yml 具备 tag 触发、packages: write、双架构与 SBOM/provenance；
#   f. 镜像构建上下文为仓库根（仓根 .dockerignore 裁剪），notices 随镜像与 release 附件分发
#      （MIT/ISC 许可文本保留要求；backend/.dockerignore 已迁至仓根）。
#
# 退出码：0 全部通过；1 存在断言失败。本脚本不改任何文件，可安全在 CI 与本机运行。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CI="$ROOT/.github/workflows/ci.yml"
REL="$ROOT/.github/workflows/release.yml"

fail=0
ok()  { printf 'ok   %s\n' "$*"; }
bad() { printf 'FAIL %s\n' "$*" >&2; fail=$((fail + 1)); }

# --- a. 文件存在性 ---------------------------------------------------------
for f in "$CI" "$REL"; do
  if [ -f "$f" ]; then
    ok "存在 ${f#"$ROOT"/}"
  else
    bad "缺少 ${f#"$ROOT"/}"
  fi
done

# 后续断言都读取文件；缺失时直接汇总退出，避免 grep 报错掩盖真实原因。
if [ ! -f "$CI" ] || [ ! -f "$REL" ]; then
  echo "check_ci: 缺少 workflow 文件，跳过其余断言" >&2
  exit 1
fi

# --- b. action 全部 pin 到 40 位 commit SHA -------------------------------
# 覆盖形如 `uses: owner/repo@sha` 与 `owner/repo/subdir@sha`；允许行尾 `# 注释`。
# 只匹配真正以 uses: 开头的行，避免把注释里的示例当成真配置。
uses_refs="$(grep -hE '^[[:space:]]*(-[[:space:]]+)?uses:' "$CI" "$REL" \
  | sed -E 's/^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*//; s/[[:space:]]+#.*$//; s/^["'\'']//; s/["'\'']$//')"

if [ -z "$uses_refs" ]; then
  bad "两个 workflow 中未找到任何 uses:（预期使用 pin 过的官方 action）"
else
  while IFS= read -r ref; do
    [ -n "$ref" ] || continue
    # 本地 action（./...）不涉及供应链 pin，允许；docker:// 形式本项目未使用，同样拒绝。
    case "$ref" in
      ./*)
        ok "本地 action $ref"
        continue
        ;;
    esac
    if [[ "$ref" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*@[0-9a-f]{40}$ ]]; then
      ok "pin $ref"
    else
      bad "action 未固定到 40 位 commit SHA：$ref（GC-13 供应链钉死）"
    fi
  done <<< "$uses_refs"
fi

# 显式点名常见浮动写法，给出比正则更直白的失败原因。
if grep -hE '^[[:space:]]*(-[[:space:]]+)?uses:' "$CI" "$REL" | grep -qE '@(v[0-9]+|main|master|latest)([[:space:]]|$|#)'; then
  bad "检测到浮动 action 标签（@vN/@main/@latest）——必须改为 40 位 commit SHA"
else
  ok "无浮动 action 标签"
fi

# --- c. 统一 PG16（GC-4 / C-2③ / D4） --------------------------------------
# 为什么零命中而不是「只用 16」：一处旧版 postgres 就会让 CI 与部署/docker-compose 漂移。
if grep -q 'postgres:15' "$CI" "$REL"; then
  bad "workflow 中出现 postgres:15（GC-4/C-2③/D4 要求统一 postgres:16-alpine）"
else
  ok "无 postgres:15"
fi
if grep -q 'postgres:16-alpine' "$CI"; then
  ok "ci.yml 使用 postgres:16-alpine"
else
  bad "ci.yml 未使用 postgres:16-alpine（集成与迁移验证均需要 PG16 service container）"
fi

# --- d. 工具版本固定且与 Makefile 单一来源（GC-13） ------------------------
# 为什么比对 Makefile：CI 与本地 make 用不同版本会制造「本地绿、CI 红」的不可复现
# 差异；仓库只有一个构建入口，CI 的 env 必须与它逐字一致，且不得是浮动标签。
make_var() { # make_var <NAME> —— 取 Makefile 中 `NAME ?= value`
  sed -nE "s/^${1}[[:space:]]*\\?=[[:space:]]*([^[:space:]#]+).*/\\1/p" "$ROOT/Makefile" | head -n 1
}
ci_var() { # ci_var <NAME> —— 取 ci.yml env 中的 `NAME: value`
  sed -nE "s/^[[:space:]]+${1}:[[:space:]]*([^[:space:]#]+).*/\\1/p" "$CI" | head -n 1
}
for pair in MIGRATE_VERSION KIN_OPENAPI_VERSION; do
  make_v="$(make_var "$pair")"
  ci_v="$(ci_var "$pair")"
  if [ -z "$ci_v" ] || [ -z "$make_v" ]; then
    bad "${pair} 缺失（Makefile='${make_v:-<空>}' ci.yml='${ci_v:-<空>}'）"
  elif printf '%s' "$ci_v" | grep -qE '^(latest|main|master)$'; then
    bad "${pair} 为浮动取值 '${ci_v}'（GC-13 禁 @latest）"
  elif [ "$ci_v" != "$make_v" ]; then
    bad "${pair} 与 Makefile 不一致（ci.yml='${ci_v}' Makefile='${make_v}'）"
  else
    ok "${pair}=${ci_v} 与 Makefile 一致（GC-13）"
  fi
done
if grep -q 'migrate/v4/cmd/migrate@${MIGRATE_VERSION}' "$CI"; then
  ok "migrate 安装引用 MIGRATE_VERSION"
else
  bad "migrate 安装未引用 \${MIGRATE_VERSION}"
fi
if grep -q 'kin-openapi/cmd/validate@${KIN_OPENAPI_VERSION}' "$CI"; then
  ok "kin-openapi 校验引用 KIN_OPENAPI_VERSION"
else
  bad "kin-openapi 校验未引用 \${KIN_OPENAPI_VERSION}"
fi

# --- e. job 覆盖（platform-02 §7.3–§7.11 的阻断门禁） ---------------------
for job in lint contract-gate unit-test integration-test migrate-verify openapi frontend-build security-headers image-build; do
  if grep -qE "^  ${job}:" "$CI"; then
    ok "ci.yml job ${job}"
  else
    bad "ci.yml 缺少阻断 job ${job}"
  fi
done

# 触发与最小权限：PR + push main；concurrency 取消同 ref 旧任务；默认只读。
grep -q 'pull_request:' "$CI"             && ok "ci.yml 触发 pull_request" || bad "ci.yml 缺少 pull_request 触发"
grep -qE 'branches: \[main\]' "$CI"      && ok "ci.yml 触发 push main"    || bad "ci.yml 缺少 push main 触发"
grep -q '^concurrency:' "$CI"            && ok "ci.yml 声明 concurrency"  || bad "ci.yml 缺少 concurrency"
grep -qE '^permissions:' "$CI"           && ok "ci.yml 声明 permissions"  || bad "ci.yml 缺少 permissions"
grep -qE '^[[:space:]]+contents: read' "$CI" && ok "ci.yml contents: read" || bad "ci.yml 缺少 contents: read"
grep -q 'go test -tags=integration ./...' "$CI" && ok "ci.yml 运行集成测试" || bad "ci.yml 未运行 go test -tags=integration ./..."
grep -q 'check_bundle_size.sh' "$CI"     && ok "ci.yml 引用体积门禁脚本"  || bad "ci.yml 未引用 scripts/check_bundle_size.sh"
grep -q 'check_locale_parity.sh' "$CI"   && ok "ci.yml 引用语种奇偶校验"  || bad "ci.yml 未引用 scripts/check_locale_parity.sh"
grep -q 'oasdiff' "$CI"                  && ok "ci.yml 运行 oasdiff"      || bad "ci.yml 缺少 oasdiff 破坏性变更门禁"

# release.yml：只有 tag 发布是供应链出口，权限/架构/元数据必须齐全。
grep -qE "tags: \[['\"]v\*['\"]\]" "$REL" && ok "release.yml 由 v* tag 触发" || bad "release.yml 缺少 push tags: ['v*']"
grep -q 'packages: write' "$REL"           && ok "release.yml packages: write" || bad "release.yml 缺少 packages: write"
grep -q 'linux/amd64,linux/arm64' "$REL"   && ok "release.yml 双架构"          || bad "release.yml 缺少 linux/amd64,linux/arm64"
grep -qE '^[[:space:]]+provenance: true' "$REL" && ok "release.yml provenance" || bad "release.yml 缺少 provenance: true"
grep -qE '^[[:space:]]+sbom: true' "$REL"       && ok "release.yml sbom"       || bad "release.yml 缺少 sbom: true"
grep -q 'GITHUB_SHA' "$REL"                && ok "release.yml 计算 sha 短标签"  || bad "release.yml 缺少 sha-<短哈希> 标签计算"
grep -q ':latest' "$REL"                   && ok "release.yml latest 标签"      || bad "release.yml 缺少 latest 标签"
grep -qE 'IMAGE_REGISTRY|IMAGE_NAME' "$REL" && ok "release.yml 使用 IMAGE_REGISTRY/IMAGE_NAME" || bad "release.yml 缺少 IMAGE_REGISTRY/IMAGE_NAME"

# --- f. 镜像构建上下文与第三方许可分发（area4 Important 3） -----------------
# 构建上下文必须是仓库根：notices 在仓根，backend 上下文无法 COPY 到运行镜像。
if grep -qE '^[[:space:]]+context: backend$' "$CI" "$REL"; then
  bad "workflow 仍以 backend/ 为构建上下文（notices 无法随镜像分发）"
else
  ok "workflow 不再使用 backend/ 构建上下文"
fi
for f in "$CI" "$REL"; do
  if grep -qE '^[[:space:]]+context: \.$' "$f"; then
    ok "${f#"$ROOT"/} 构建上下文为仓库根"
  else
    bad "${f#"$ROOT"/} 缺少 context: .（仓库根）"
  fi
done
if grep -qF -- 'docker build -f backend/Dockerfile -t newtg/gateway:ci .' "$CI"; then
  ok "ci.yml security-headers 以仓根为上下文构建镜像"
else
  bad "ci.yml security-headers 的 docker build 未使用仓根上下文"
fi
grep -q 'THIRD_PARTY_NOTICES.md' "$REL" && grep -q 'gh release' "$REL" \
  && ok "release.yml 把 THIRD_PARTY_NOTICES.md 挂到 release 附件" \
  || bad "release.yml 缺少 notices release 附件（gh release ...）"
grep -qF -- 'COPY THIRD_PARTY_NOTICES.md /usr/share/licenses/newtg/THIRD_PARTY_NOTICES.md' "$ROOT/backend/Dockerfile" \
  && ok "Dockerfile 把 notices 复制进运行镜像" \
  || bad "Dockerfile 未 COPY THIRD_PARTY_NOTICES.md 到运行镜像"
# 上下文裁剪：仓根 .dockerignore 存在，旧的 backend/.dockerignore 已迁走
# （子目录 .dockerignore 在仓根上下文下不生效，留着会误导）。
[ -f "$ROOT/.dockerignore" ] && ok "存在仓根 .dockerignore" || bad "缺少仓根 .dockerignore（构建上下文未裁剪）"
[ ! -f "$ROOT/backend/.dockerignore" ] && ok "backend/.dockerignore 已迁移" || bad "backend/.dockerignore 仍存在（仓根上下文下不生效）"
grep -qF -- 'Dockerfile -t $(IMAGE) .' "$ROOT/Makefile" \
  && ok "Makefile image 目标使用仓根上下文" \
  || bad "Makefile image 目标未使用仓根上下文"
grep -qF -- 'Dockerfile -t $(IMAGE) --push .' "$ROOT/Makefile" \
  && ok "Makefile image-multiarch 目标使用仓根上下文" \
  || bad "Makefile image-multiarch 目标未使用仓根上下文"

# --- g. i18n 词典奇偶校验（web-00 §7.6 / TC-C-04） --------------------------
# 为什么在这里直接运行而不是只断言 workflow 文本：缺键不会让 TypeScript 或
# 构建失败，运行时只会静默回退英文；门禁必须在本地也能一键复现。
if [ -f "$ROOT/scripts/check_locale_parity.sh" ]; then
  if parity_out="$(bash "$ROOT/scripts/check_locale_parity.sh" 2>&1)"; then
    ok "7 语种词典键集合一致（check_locale_parity.sh）"
  else
    bad "7 语种词典奇偶校验未通过"
    printf '%s\n' "$parity_out" >&2
  fi
else
  bad "缺少 scripts/check_locale_parity.sh"
fi

echo
if [ "$fail" -gt 0 ]; then
  echo "check_ci: $fail 项断言失败" >&2
  exit 1
fi
echo "check_ci: 全部断言通过"
