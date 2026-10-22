# NewTG 上线检查表（M7 准入闸门）

> 依据：`docs/platform-07-发布与灰度-设计.md` §7（就绪清单）、
> `docs/platform-08-备份恢复与灾备演练-设计.md`、`docs/platform-06-压测方案-设计.md`。
> 用法：逐项执行、填写“结果/证据”；任一 **阻断项** 未过 = **不得上线**。
> 标注「真实环境」的项在本仓库内只能做静态检查，必须在目标环境复核。

## 0. 判定规则

- **阻断项（B）**：不过不得上线，也不得进入业务阶段 0。
- **观察项（O）**：可带风险上线，但必须登记 owner 与到期时间。
- 每项证据（命令输出、截图、链接）归档到上线记录，不写入 Git 的密钥/令牌。

## 1. 版本与制品

| # | 检查 | 命令 / 方法 | 通过标准 | 类型 | 结果 |
|---|---|---|---|---|---|
| 1.1 | 版本一致 | `cat VERSION`；镜像 tag | VERSION 与部署 `sha-<短哈希>` 对应 | B | |
| 1.2 | 镜像可拉取 | `docker pull <registry>/<image>:sha-<短哈希>` | 成功，记录 digest | B | |
| 1.3 | 回滚镜像可拉取 | 同上，旧 tag | 成功，记录 digest | B | |
| 1.4 | 前端产物 | `test -d frontend/dist && ls frontend/dist` | 非空；与后端版本同批构建 | B | |
| 1.5 | 无 `latest` 引用 | `grep -R "image:.*:latest" deploy/ || true` | 部署清单不使用 `latest` | B | |
| 1.6 | 构建元信息 | `curl -s <edge>/api/system/status` | version/commit/build_time 与发布单元一致 | O | |

## 2. 备份与恢复

| # | 检查 | 命令 / 方法 | 通过标准 | 类型 | 结果 |
|---|---|---|---|---|---|
| 2.1 | 发布前备份 | `deploy/ops/backup/backup.sh --out-dir /backups --label prerelease` | 主库+日志库各一份非空转储 + sidecar | B | |
| 2.2 | 轻校验 | `deploy/ops/backup/verify.sh --dump <dump>` | `verify: PASS`（V1–V4） | B | |
| 2.3 | 恢复演练 | `deploy/ops/backup/restore.sh --dump <dump> --target-dsn <临时实例> --check --yes` | 断言 PASS；`/readyz` 200 | B | |
| 2.4 | RTO 记录 | 演练记录 | T1–T7 实测合计 ≤ 预算（platform-08 §2.3） | O | |
| 2.5 | 异地副本 | 对象存储/离线介质 | 异地可读；月基线离线副本存在 | B（真实环境） | |
| 2.6 | 密钥分离 | 保险箱/密钥系统 | L1 age 私钥、L2 `CREDENTIAL_ENCRYPTION_KEY` 分开托管；历史版本 ≥12 月 | B（真实环境） | |
| 2.7 | 备份目录权限 | `ls -ld /backups`；`umask` | 仅运维可读；不含明文密钥 | B | |

## 3. 数据库迁移

| # | 检查 | 命令 / 方法 | 通过标准 | 类型 | 结果 |
|---|---|---|---|---|---|
| 3.1 | 迁移 Job 成功 | K8s Job / compose `migrate` | 退出码 0；日志无 `dirty` | B | |
| 3.2 | 迁移未 dirty | `psql -tAc "SELECT dirty FROM schema_migrations"` | `f` | B | |
| 3.3 | 版本匹配 | `SELECT version FROM schema_migrations` ↔ `/readyz` | 一致；`/readyz` R3 ok | B | |
| 3.4 | 不每副本迁移 | 检查 gateway env | `DB_MIGRATE_ON_START=false` | B | |
| 3.5 | 兼容窗口 | 审阅本次迁移 | 仅纯新增/expand；无破坏型 | B | |
| 3.6 | 分区预建 | `SELECT count(*) FROM pg_inherits WHERE inhparent='usage_logs'::regclass` | 当月+下月存在 | B | |
| 3.7 | 日志库迁移 | 日志库 `schema_migrations` | 版本正确、非 dirty | B | |

## 4. 配置与密钥

| # | 检查 | 命令 / 方法 | 通过标准 | 类型 | 结果 |
|---|---|---|---|---|---|
| 4.1 | 必需密钥齐备 | 对比 `deploy/.env.example` | 六项数据面口令 + `SESSION_SECRET` + `CREDENTIAL_ENCRYPTION_KEY` 非空 | B | |
| 4.2 | 无默认口令 | `grep -R "change-me\|password123" deploy/` | 无命中 | B | |
| 4.3 | 密钥不复用 | 审阅 `.env` | 每个角色口令独立 | B | |
| 4.4 | `TRUSTED_PROXIES` | 检查环境 | 非空且不是 `0.0.0.0/0`（生产） | B | |
| 4.5 | `PUBLIC_BASE_URL` | `curl $PUBLIC_BASE_URL/api/status` | 200；OAuth/支付回调可拼接 | B | |
| 4.6 | `METRICS_TOKEN` | 检查环境；`curl -H 'Authorization: Bearer …' :8006/metrics` | 已设置；未设置时 `/metrics` 不启动 | B | |
| 4.7 | TLS 证书 | `openssl s_client -connect <host>:443` | 链有效；剩余有效期 ≥30 天 | B（真实环境） | |
| 4.8 | 主密钥轮换预案 | 轮换演练记录 | 多副本一致；灰度期不轮换 | O | |
| 4.9 | 配置冻结 | 变更记录 | 灰度窗口无非兼容配置变更 | B | |

## 5. 可观测性与告警

| # | 检查 | 命令 / 方法 | 通过标准 | 类型 | 结果 |
|---|---|---|---|---|---|
| 5.1 | 指标暴露 | `curl <pod>:8006/metrics` | 关键指标存在（QPS/延迟/结算/worker/池） | B | |
| 5.2 | 规则加载 | Prometheus 规则页 / `promtool check rules deploy/ops/prometheus/alerts.yaml` | 无语法错误；规则在册 | B | |
| 5.3 | 告警通道 | 测试告警 | Alertmanager → Webhook/邮件/IM 实际收到 | B（真实环境） | |
| 5.4 | 去重/抑制 | 触发一次重复告警 | 同 `(alertname,labels)` 只发一次；依赖故障抑制派生 | O | |
| 5.5 | 日志格式 | `docker logs <gateway>` / Loki | 每行合法 JSON；含 `request_id`；无多行 | B | |
| 5.6 | 日志保留 | 采集后端 retention | ≥30 天（部署默认 90 天） | B（真实环境） | |
| 5.7 | 告警阈值校准 | 压测报告 | A1/A3/A4/A14/A15 等已用稳态分布校准并回写 | O（M7） | |
| 5.8 | 版本可区分 | `newtg_build_info` | 灰度期可按 version/commit 区分新旧 Pod | B | |

## 6. 灰度与回滚

| # | 检查 | 命令 / 方法 | 通过标准 | 类型 | 结果 |
|---|---|---|---|---|---|
| 6.1 | 回滚演练完成 | 演练记录 | 命令 <1 分钟发起；无 5xx 增量；dirty=false | B | |
| 6.2 | PDB | `kubectl get pdb -n newtg` | gateway PDB `maxUnavailable: 1`（N≥2） | B（真实环境） | |
| 6.3 | 滚动策略 | `kubectl get deploy gateway -o yaml` | `maxSurge:1`、`maxUnavailable:0` | B（真实环境） | |
| 6.4 | HPA 冻结 | `kubectl get hpa -n newtg` | 灰度窗口不扩缩 | B | |
| 6.5 | 无节点排空 | 变更记录 | L1/L2 驻留期不 drain | B | |
| 6.6 | 门禁信号 | Prometheus | A3/A5/A6/A16/A17/A19 未触发 | B | |
| 6.7 | 长连接 | Ingress/edge 配置 | WS 升级透传；空闲超时 ≥ 最长空洞或禁用 | B（真实环境） | |
| 6.8 | 优雅下线 | `SHUTDOWN_GRACE_SECONDS=30`；`stop_grace_period=35s` | 在飞结算完成、无 5xx | B | |

## 7. 压测与容量

| # | 检查 | 命令 / 方法 | 通过标准 | 类型 | 结果 |
|---|---|---|---|---|---|
| 7.1 | 压测环境冻结 | 报告 | 镜像 digest、env、池大小、PG/Redis 版本、数据基线记录 | B | |
| 7.2 | 成本护栏 | 报告 | 最低价模型；max_tokens ≤64；≤32 并发/渠道；≤50 元/run | B | |
| 7.3 | S1/S2 通过 | `loadtest.py` + 服务端指标 | 错误率 <0.05；J1/J2/J5/J6 达标 | B | |
| 7.4 | 账本一致性 | `ledger_check.sh` | A/B/C/D 全过；无负余额、无 settlement_pending、无 in_flight | B | |
| 7.5 | 停压条件 | 压测记录 | 全程未触发钳制/负余额/迁移 dirty/DB 写失败 | B | |
| 7.6 | J1–J22 | platform-06 §5.1 | 逐条判定；一票否决项为 0 | B | |
| 7.7 | 上游受限档 | 报告 | 作废档有理由；未把上游限流当系统失败 | O | |
| 7.8 | 容量记录 | 报告 | 单副本 RPS/在飞并发/附加 p95 记录 | O | |

## 8. 业务上线阶段（platform-07 §6）

### 阶段 0 内测（Stripe-only）

| # | 检查 | 通过标准 | 类型 | 结果 |
|---|---|---|---|---|
| 8.1 | 关闭公开注册 | `auth.signup_enabled=false` | B | |
| 8.2 | Stripe 开启 / 支付宝关闭 | `pay.stripe.enabled=true`；`pay.alipay.enabled=false` | B | |
| 8.3 | 核心渠道 | 仅启用稳定渠道 | B | |
| 8.4 | 白名单/管理员建号 | 流程可用 | B | |
| 8.5 | 退出条件 | 白名单 ≥N；连续 7 天无 A5/A16 | O | |

### 阶段 1 限额公测

| # | 检查 | 通过标准 | 类型 | 结果 |
|---|---|---|---|---|
| 8.6 | 开放注册 + 邮箱验证 | `auth.signup_enabled=true`；`auth.email_verification_required=true` | B | |
| 8.7 | 人机验证 | `auth.turnstile.enabled=true` | O | |
| 8.8 | 限流收紧 | `rl.user_model_rpm=60`、`rl.token_rpm=60`、`rl.user_global_rpm=120` | B | |
| 8.9 | 注册赠送 | `billing.signup_grant_credits=0`（或产品裁决值） | O | |
| 8.10 | 免费模型预扣 | `billing.free_model_preconsume_enabled=true` | O | |
| 8.11 | 退出条件 | 配额/限流无异常、支付对账通过，连续 14 天 | O | |

### 阶段 2 全量公开

| # | 检查 | 通过标准 | 类型 | 结果 |
|---|---|---|---|---|
| 8.12 | 全渠道 | `channels.status=enabled` | B | |
| 8.13 | 支付宝 | `pay.alipay.enabled=true`，走同幂等入账服务 | B | |
| 8.14 | 公开页/排行榜 | `nav.rankings` 等开启；样本量满足 | O | |
| 8.15 | 业务回退口径 | 不回收已注册用户/余额/流水；只收紧要新增入口 | B | |

## 9. 安全与合规

| # | 检查 | 通过标准 | 类型 | 结果 |
|---|---|---|---|---|
| 9.1 | 日志脱敏 | 日志/备份不含令牌、口令、DSN、Prompt 正文 | B | |
| 9.2 | 备份不含密钥 | 扫描转储/manifest 无 L1/L2/DSN | B | |
| 9.3 | 管理面暴露 | `/metrics`、pprof、管理 API 不经公网 | B | |
| 9.4 | 审计保留 | `audit_logs` 保留策略生效 | B | |
| 9.5 | 依赖最小权限 | `newtg_app`/`newtg_migrate`/`newtg_backup`/`newtg_retention` 权限符合设计 | B | |
| 9.6 | 备份对象锁 | 版本化 + 治理模式 30 天 + 仅 put 凭据 | B（真实环境） | |

## 10. 签字与结论

| 角色 | 姓名 | 结论（通过/有条件/不通过） | 时间 | 证据链接 |
|---|---|---|---|---|
| 发布负责人 | | | | |
| 运维/SRE | | | | |
| 安全 | | | | |
| 产品 | | | | |

**有条件通过的遗留项**（逐条写 owner + 到期时间）：

1. …
2. …

**最终结论**：`□ 通过　□ 有条件通过　□ 不通过`

> 本检查表是 M7 准入闸门（platform-07 裁决 G7）；任一项阻断项不过，不得进入
> 灰度或业务阶段 0。“真实环境”项必须在目标环境复核，不能以本仓库静态检查代替。
