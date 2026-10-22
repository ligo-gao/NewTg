# NewTG 运维制品（M7 上线准备）

本目录承接 `docs/未完成清单.md` §19 **P1-14** 中可在仓库内完成的部分，并为
M7 上线窗口提供可直接执行的制品。设计依据（**只引用，不修改**）：

- `docs/platform-05-可观测性-设计.md` — 指标集、`/metrics`、告警 A1–A21
- `docs/platform-06-压测方案-设计.md` — 压测场景、成本护栏、准入判据 J1–J22
- `docs/platform-07-发布与灰度-设计.md` — 发布顺序、灰度阶梯、回滚
- `docs/platform-08-备份恢复与灾备演练-设计.md` — RPO/RTO、备份验证、演练

## 目录

| 路径 | 用途 |
|---|---|
| `prometheus/alerts.yaml` | Prometheus 告警规则（worker 健康、结算积压、Webhook、余额、错误率/延迟、依赖） |
| `backup/backup.sh` | 主库 + 日志库全量备份（`pg_dump -Fc`），生成 `.sha256` 与 `.manifest.json` |
| `backup/verify.sh` | 备份轻校验 V1–V4（结构 / 校验和 / 解密 / manifest） |
| `backup/restore.sh` | 恢复到新实例，可选恢复后断言 |
| `backup/assert_main.sql` | 主库恢复断言（余额非负、余额=流水、迁移非 dirty） |
| `backup/assert_logs.sql` | 日志库恢复断言（表/分区存在、status 枚举、重复 request_id） |
| `loadtest/loadtest.py` | stdlib-only 并发压测（`/v1/chat/completions`，支持流式） |
| `loadtest/ledger_check.sh` | 压测后账本一致性断言（usage_logs / wallet_transactions / request_registry） |
| `runbooks/canary-rollback.md` | 灰度放量与回滚 runbook（compose 预演 + K8s 生产） |
| `runbooks/go-live-checklist.md` | 上线检查表（逐项命令与通过标准） |
| `runbooks/dr-drill-template.md` | 备份恢复演练记录模板 |

## 前置工具

| 工具 | 用途 | 缺失时 |
|---|---|---|
| `bash` ≥ 4.2 | 全部 shell 脚本 | 不支持 |
| `pg_dump` / `pg_restore` / `psql`（PostgreSQL 16 客户端） | 备份、恢复、断言 | 脚本直接失败（非零退出） |
| `python3` ≥ 3.10 | 压测脚本、manifest JSON 校验 | 压测/manifest 校验失败 |
| `age`（可选） | 备份包产生端加密 | 仅在启用加密时需要 |
| `promtool`（可选） | 告警规则语法校验 | 不影响规则加载 |

`docker compose` 形态下，可在一次性 `postgres:16-alpine` 容器内执行备份脚本
（该镜像自带 `pg_dump`/`pg_restore`/`psql`），见 `backup/README.md`。

## 密钥与数据红线

1. **DSN 只经环境变量传入**：`NEWTG_MAIN_DSN` / `NEWTG_LOGS_DSN` / `NEWTG_RESTORE_DSN`。
   仓库内不存在任何真实口令；脚本不会把 DSN 写入产物或日志。
2. **备份使用只读角色** `newtg_backup`（`deploy/postgres-init/10-roles.sql`）；
   恢复演练使用独立临时实例，**不得**对生产实例执行 `pg_restore --clean`。
3. **压测令牌只从 `NEWTG_API_TOKEN` 读取**，脚本不接收命令行 token，避免进入
   shell history；压测账号必须挂 `loadtest` 分组，压后按 `platform-06` §7.4 清理。
4. `.sha256` 与 `.manifest.json` 不含密钥；`age` 私钥（L1）与
   `CREDENTIAL_ENCRYPTION_KEY`（L2）**绝不**进入备份包或本目录。

## 本目录无法替代的“真实环境验收”

以下项目必须在上线环境执行后才能勾选（见 `runbooks/go-live-checklist.md`）：

- 告警阈值校准（`platform-05` U-05-2）：依赖真实压测稳态分布；
- 备份 RPO/RTO 实测（`platform-08` §2.3 七段预算）：依赖真实库规模与链路；
- 异地副本、对象锁、离线月基线的实际配置；
- K8s 灰度、PDB、HPA 冻结、Ingress/长连接超时；
- 真实上游压测（`platform-06` J1–J22，含成本与上游限流）；
- 支付、邮件、Webhook 等外部依赖的实际可用性。
