# NEW TG

AI API 网关与计费平台：统一接入多个上游模型供应商，对下游发放 API 令牌、计量用量、结算费用，并提供控制台、官网与运维接口。

## 能力概览

- **统一接入**：单一二进制按 `RUN_MODE` 切换 `gateway`（对外服务）、`worker`（后台任务）、`migrate`（迁移）三角色。
- **协议中转**：`chat` / `responses` / `videos` / `batch` 等中继端点，按渠道分流上游。
- **令牌与配额**：令牌签发、限流、额度与模型级配置、用量日志。
- **计费**：定价策略、预扣结算与退款、钱包与账户流水、订阅套餐、账单周期任务。
- **运营**：渠道健康检查、上游对账、模型性能排行与定价页数据、公告与通知。
- **安全**：RBAC、审计日志、强制管理员 MFA、`base_url` 变更二次验证、敏感字段 AES-256-GCM 加密落库。
- **可观测**：Prometheus 指标（`/metrics`，`METRICS_TOKEN` 鉴权）、健康探针 `/healthz`、`/readyz`。

## 仓库结构

| 目录 | 内容 |
|---|---|
| `backend/` | Go 服务：`cmd/newtg` 入口，`internal/` 分域（relay、billing、auth、rbac、audit、store…），迁移在 `migrations/` |
| `frontend/` | 控制台前端：React + Rsbuild，Bun 管理依赖 |
| `website/` | 官网：Next.js 静态导出 |
| `packages/` | 前后端共用包（`public-shell` 等） |
| `api/` | OpenAPI 3 分域契约源与生成物（前后端类型由此生成，禁手改） |
| `deploy/` | 单机 compose、Kubernetes Helm chart（`deploy/k8s/newtg`）、边缘代理与运维脚本 |
| `scripts/` | 仓库内工具脚本 |

## 技术栈

Go 1.26 · PostgreSQL 16 · Redis 7 · React + Rsbuild + Bun · Next.js（静态导出）· Helm 3 · Caddy（边缘）· OpenAPI 3

## 快速开始（容器）

前置：Docker 与 Docker Compose v2。

```bash
cp deploy/.env.example deploy/.env          # 逐项填写，密钥用 openssl rand -hex 32 生成
make compose-up                             # PG + Redis + migrate + gateway + worker + edge
```

入口 `http://localhost:8080`（HTTPS 走 `8443`）。`deploy/.env` 含敏感口令，已在 `.gitignore` 中，绝不入库。

## 本机开发

前置：Go 1.26+、Bun、Node（仅官网需要）。

```bash
make build          # 构建单产物 bin/newtg
make run-gateway    # 本机起 gateway（读环境变量配置）
make run-worker     # 本机起 worker
make web-dev        # 前端开发服务器，代理到 localhost:3000
make help           # 全部目标与说明
```

## 测试与门禁

```bash
make test               # Go 单元测试（无外部依赖，CI 门禁）
make test-integration   # 需要真实 PostgreSQL + Redis
make web-test           # 前端 Vitest
make api-check          # OpenAPI：布局 + 生成 + lint + 运行时校验
make chart-lint         # Helm chart lint + 渲染结构断言
```

## 部署

- **单机**：`deploy/docker-compose.yml`（`make compose-up`）。
- **多机 / Kubernetes**：`deploy/k8s/newtg` Helm chart，PostgreSQL、Redis、前端（2 副本）、后端（gateway 多副本 + HPA、worker）按节点池分离部署。安装步骤、密钥清单、TLS 与备份配置见 [deploy/k8s/newtg/README.md](deploy/k8s/newtg/README.md)。

## 配置与安全约定

- 数据面口令**互不复用**：`POSTGRES_PASSWORD`（引导）、`POSTGRES_APP_PASSWORD`（DML）、`POSTGRES_MIGRATE_PASSWORD`（唯一 DDL 账号）、`POSTGRES_BACKUP_PASSWORD`（只读备份）、`POSTGRES_RETENTION_PASSWORD`、`REDIS_PASSWORD` 各用各的。
- `SESSION_SECRET` 参与 CSRF 与审计标识派生，多副本必须一致。
- `CREDENTIAL_ENCRYPTION_KEY` 为 32 字节（64 位 hex）AES-256-GCM 主密钥，丢失即无法解密渠道凭证。
- 生产环境须设置 `TRUSTED_PROXIES`，只信任边缘代理地址，否则无法从 `X-Forwarded-For` 还原真实客户端 IP。

第三方组件许可见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
