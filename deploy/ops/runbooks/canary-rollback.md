# 灰度与回滚 Runbook

> 依据：`docs/platform-07-发布与灰度-设计.md`（发布顺序 §3、灰度 §4、回滚 §5、
> 就绪清单 §7）、`docs/platform-05-可观测性-设计.md`（告警 A1–A21）。
> 本 runbook 只给执行步骤；不重新定义阈值、迁移写法或键名。

## 0. 适用范围与前提

- **生产灰度 = K8s 单 Deployment 暂停式滚动 + 人工门禁**（platform-07 裁决 G3）。
  compose 双服务只用于**预演**，不是生产灰度机制。
- **发布单元 = 不可变镜像 tag `sha-<短哈希>` + 一个迁移版本**（G1）。
  `latest` 不得用于发布或回滚。
- 本 runbook 不覆盖破坏型/不可回滚迁移：那类变更走 platform-07 §3.4 的
  维护窗口整体切换，且**只能前滚**。

## 1. 前置条件（任一不满足不得开始）

| # | 条件 | 检查 | 通过标准 |
|---|---|---|---|
| P1 | CI 门禁全绿 | CI 流水线 | lint / contract-gate / unit / integration / migrate-verify / frontend-build / image-build 全过 |
| P2 | 迁移往返幂等 | `make migrate-verify` | 第二次 up 无变化；往返版本一致 |
| P3 | 发布单元确定 | `cat VERSION`；镜像 digest | VERSION 与 `sha-<短哈希>` 对应；记录 digest |
| P4 | 备份可用 | `deploy/ops/backup/verify.sh --dump <最新转储>` | `verify: PASS`（V1–V4） |
| P5 | 回滚目标可拉取 | `docker pull <registry>/<image>:sha-<旧短哈希>` | 成功，且记录 digest |
| P6 | 迁移兼容窗口 | 本次迁移仅纯新增或 expand 段 | 无 DROP/收紧 NOT NULL/收紧 CHECK（platform-07 §3.2） |
| P7 | 可观测就绪 | Prometheus 规则列表 | `deploy/ops/prometheus/alerts.yaml` 已加载；critical 通道测试通过 |
| P8 | 当前无资损红线告警 | Prometheus | A5 / A6 / A16 / A19 均未触发 |
| P9 | 灰度窗口冻结 | 变更记录 | HPA 冻结、无节点排空、无非兼容配置变更 |

## 2. 发布顺序（固定四步，不得互换）

```
① 备份 → ② 一次性 migrate → ③ 暂停式滚动 → ④ 逐档观察
```

1. **备份**（主库 + 日志库）：
   ```bash
   export NEWTG_MAIN_DSN='postgres://newtg_backup:<password>@postgres:5432/newtg?sslmode=disable'
   export NEWTG_LOGS_DSN='postgres://newtg_backup:<password>@postgres:5432/newtg_logs?sslmode=disable'
   deploy/ops/backup/backup.sh --out-dir /backups --label prerelease
   deploy/ops/backup/verify.sh --dump /backups/newtg-<ts>-prerelease-main.dump
   ```
   记录转储路径、SHA-256、manifest 的 `schema_migrations_version`。
2. **一次性 migrate**：生产用独立 Job / compose 一次性服务，`RUN_MODE=migrate`；
   gateway 保持 `DB_MIGRATE_ON_START=false`。**严禁每个副本各自迁移**。
3. **滚动**：本节 §3/§4。
4. **观察**：每档驻留 ≥30 分钟 **且** ≥20 次成功结算，取较大者；观察窗口
   必须覆盖最长告警窗口（10m），不得提前 `resume`。

## 3. compose 预演（只验证流程，不是生产灰度）

> 当前 `deploy/docker-compose.yml` 是单 `gateway` 服务 + `deploy.replicas: 2`。
> 预演“新旧并存”需要临时的 `gateway-stable` / `gateway-canary` override；
> **该 override 不得提交**，也不得修改正在运行的 `postgres`/`redis`。

1. 备份 + 迁移（同 §2）。
2. 用临时 override 起两个 gateway 服务，各自固定镜像 tag，Caddy upstream
   轮询 + `/readyz` 健康检查（platform-07 §4.2）。
3. 逐档调整 Caddy upstream 权重（0/33/67/100），每档执行 §5 的门禁。
4. 通过判据（platform-03 §7）：S4 配置秒级全副本生效、S6 杀副本仍可用、
   S11 优雅下线不丢在飞结算。
5. 预演结束 `docker compose down`（**不要** `-v`，避免删数据卷），清理临时
   override 与预演账号。

## 4. K8s 生产灰度

前提：`strategy.rollingUpdate: { maxSurge: 1, maxUnavailable: 0 }`；gateway
至少 3 副本；readinessProbe 使用 `/readyz`；HPA 与本窗口冻结。

### L1 首例（33%，1/3）

```bash
kubectl -n newtg set image deployment/gateway \
  gateway=<registry>/<image>:sha-<新短哈希>
kubectl -n newtg rollout pause deployment/gateway
kubectl -n newtg rollout status deployment/gateway --timeout=10m
kubectl -n newtg get pods -l app=gateway -o wide
```

断言：恰好 1 个新 Pod 且 `Ready`；旧 Pod 仍服务；Service Endpoints 只含
通过 `/readyz` 的 Pod。

### L2 过半（67%，2/3）

门禁（§5）通过后：

```bash
kubectl -n newtg rollout resume deployment/gateway
kubectl -n newtg rollout pause deployment/gateway
kubectl -n newtg rollout status deployment/gateway --timeout=10m
kubectl -n newtg get pods -l app=gateway -o wide
```

断言：2 个新 Pod `Ready`，1 个旧 Pod 仍在。

### L3 全量（100%，3/3）

```bash
kubectl -n newtg rollout resume deployment/gateway
kubectl -n newtg rollout status deployment/gateway --timeout=15m
```

收口：全量稳定 ≥24h 后再清理旧 ReplicaSet；期间保留旧镜像与回滚 tag。

## 5. 晋级门禁（每档必须全过）

| # | 门禁 | 命令 / 查询 | 通过标准 |
|---|---|---|---|
| G-1 | 就绪 | `kubectl get pods -l app=gateway`；`curl -s -o /dev/null -w '%{http_code}' <edge>/readyz` | 新 Pod 全 Ready；HTTP 200 |
| G-2 | 关键告警无触发 | A3 / A5 / A6 / A16 / A17 / A19 | 均未触发 |
| G-3 | 无 5xx 突增 | `sum(rate(newtg_http_requests_total{status_class="5xx"}[5m]))` | 不高于升级前基线 |
| G-4 | 在飞不丢 | `newtg_billing_sessions_in_flight` / registry in_flight；客户端无可见 5xx | 无异常悬空；优雅下线生效 |

PromQL 样板：

```promql
# A3
sum(rate(newtg_relay_errors_total[5m])) / sum(rate(newtg_relay_requests_total[5m])) > 0.05
# A5
sum(rate(newtg_billing_settle_credits_total[10m])) == 0
  and sum(rate(newtg_relay_requests_total[10m])) > 0
# A19
newtg_migration_version{dirty="true"} == 1
```

> 未校准的阈值不足以支撑自动晋级/自动回滚（platform-07 §5.1）；本流程一律
> **人工决策**。

## 6. 回滚触发与动作

### 6.1 触发信号（阈值引 platform-05 §6.1）

| 信号 | 告警 | 动作 |
|---|---|---|
| 中继错误率 >0.05 持续 5m | A3 | **回滚**或暂停晋级 |
| 计费管道停摆（有流量无结算） | A5 | **立即回滚**（资损红线） |
| 钳制事件任何一次 | A6 | 暂停晋级 + 查定价；连续出现回滚 |
| DB 错误速率 >0 | A16 | **立即回滚**（资损风险） |
| 单渠道错误率 >0.5 持续 5m | A1 | 先查渠道；跨渠道才回滚版本 |
| Redis 不可用 | A17 | 查依赖；依赖故障本身不回滚，但暂停晋级 |
| 迁移 dirty | A19 | **停发布**，按 platform-04 §7 处置，不得直接回滚代码 |
| 结算积压 >15m | A9 | 查 worker/registry；持续则暂停晋级 |

### 6.2 回滚命令（推荐显式 pin）

```bash
# 路径 B（推荐）：pin 回不可变旧 tag
kubectl -n newtg set image deployment/gateway \
  gateway=<registry>/<image>:sha-<旧短哈希>
kubectl -n newtg rollout status deployment/gateway --timeout=10m
kubectl -n newtg get pods -l app=gateway -o wide

# 路径 A（备选）：回退到上一 ReplicaSet
kubectl -n newtg rollout undo deployment/gateway
```

**schema 不回退**：靠兼容窗口让旧代码继续读写 expand 后的 schema。若已执行
收缩/破坏型迁移，则**不可回滚，只能前滚修复**；若迁移失败残留 `dirty=true`，
先按 platform-04 §7 处置迁移（`down` 或从转储恢复），使
`schema_migrations.dirty=false` 且版本落到期望值。

回滚后检查：

1. `kubectl rollout status` 完成，所有 Pod Ready；
2. `/readyz` 200（注意 platform-07 §5.5 的“期望版本 ≥ 最低需求”口径）；
3. 5xx 回到基线，A3/A5/A6/A16/A19 消除；
4. `deploy/ops/loadtest/ledger_check.sh` 全局断言通过（余额/流水）；
5. 保留证据：告警快照、Pod 版本分布、日志、`usage_logs` 尾部、备份路径。

## 7. 验收标准

- [ ] 发布顺序严格为 备份 → migrate → 滚动 → 观察；
- [ ] 每档驻留 ≥30 分钟且 ≥20 次成功结算，未跳档；
- [ ] 每档 G-1..G-4 全过并留记录；
- [ ] 回滚演练至少一次：命令 <1 分钟内发起，旧版本在排空截止内就绪，
      无客户端可见 5xx 增量，`schema_migrations.dirty=false`；
- [ ] 旧镜像 tag 可拉取，回滚目标 digest 已记录；
- [ ] 灰度期无 HPA 扩缩、无节点排空、无非兼容配置变更。

## 8. 需要真实环境才能验收

- K8s Deployment/PDB/HPA/Ingress 的实际参数与权限；
- `/readyz` 五断言在真实 PG/Redis/迁移下的结果；
- 长连接（WS/SSE）排空截止与 Ingress 空闲超时；
- 告警通道（Alertmanager → N8 Webhook/邮件/IM）的实际投递；
- 真实上游错误与上游限流对 A1/A3/A4 的影响。
