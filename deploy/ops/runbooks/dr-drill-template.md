# 备份恢复演练记录模板

> 依据：`docs/platform-08-备份恢复与灾备演练-设计.md` §2.3（RTO 分段）、
> §4（恢复四型）、§5（轻轨/重轨）、§5.3（7 条断言）。
> 本模板只记录**可执行步骤与实测值**；演练记录不入库（platform-08 决策 D9），
> 建议保存到对象存储 `drills/<ts>/report.md` 与运行日志。

## 1. 基本信息

| 字段 | 填写 |
|---|---|
| 演练编号 | `drill-<YYYYMMDD>-<序号>` |
| 演练日期/时间（UTC） | |
| 演练类型 | □ 每周自动重轨　□ 季度人工全流程　□ 发版前专项 |
| 灾难场景 | □ S1 局部误操作　□ S2 逻辑损坏　□ S3 实例/卷损坏　□ S4 区域级　□ S5 备份链受损 |
| 恢复型 | □ R-A 整库重建　□ R-B 单表局部修补　□ R-C PITR（仅云化）　□ R-D 备份链受损 |
| 执行人 / 复核人 | |
| 环境 | □ 近端（local）　□ 异地（offsite） |
| 应用版本 / 镜像 digest | |
| PostgreSQL 大版本 | |
| 备份产物路径 | `s3://<bucket>/<tier>/newtg-<ts>-main.dump[.age]` |
| 日志库备份路径 | `s3://<bucket>/<tier>/newtg-<ts>-logs.dump[.age]` |
| 产物 SHA-256 | |
| manifest `schema_migrations_version` / `dirty` | |
| manifest `table_count` | |
| 实际库规模（GB）/ 转储大小（GB） | |
| 目标实例 DSN（脱敏） | `postgres://<user>@<host>:<port>/<db>` |
| 是否使用密钥信封 | □ 否　□ 是（L1/L2 取回时间：） |

## 2. 前置条件

- [ ] 备份产物 + `.sha256` + `.manifest.json` 三件齐全；
- [ ] 目标实例与备份**同大版本**（platform-08 §5.2 / U12）；
- [ ] 目标实例是**新实例**，不是生产实例；
- [ ] L1 `age` 私钥可用（加密包）；L2 `CREDENTIAL_ENCRYPTION_KEY` 可用（断言 7）；
- [ ] 演练网络、磁盘、权限就绪；生产流量不受影响；
- [ ] 已通知相关方，演练窗口与回退方案明确。

## 3. 时间线与 RTO 分段（platform-08 §2.3）

| 段 | 动作 | 预算 | 计划开始 | 实际开始 | 实际结束 | 实测耗时 | 偏差/说明 |
|---|---|---|---|---|---|---|---|
| T1 | 发现（告警/人工） | ≤30min | | | | | |
| T2 | 决策（场景/恢复型/授权） | ≤15min | | | | | |
| T3 | 取回（下载 + sha256） | ≤30min | | | | | |
| T4 | 解密（age） | ≤15min | | | | | |
| T5 | 重建（`pg_restore` 到新实例） | ≤30min | | | | | |
| T6 | 断言（§5.3 七条 + 业务抽样） | ≤20min | | | | | |
| T7 | 切流（`/readyz` → 放流 → 观察） | ≤30min | | | | | |
| | **合计** | **≤170min（≈2.8h）** | | | | | 目标 RTO ≤4h |

> 若为每周自动重轨，T1/T2 可记为 0，T7 可只做“可服务验证”不做真实切流；
> 但必须在记录中注明，不能把“未切流”当作“已通过”。

## 4. 执行步骤与证据

### 4.1 取回与轻校验

```bash
export NEWTG_RESTORE_DSN='postgres://<user>:<password>@<new-host>:5432/<restore-db>?sslmode=disable'
export AGE_IDENTITY='/secure/age.key'   # 仅加密包

# V1–V4（不落数据）
deploy/ops/backup/verify.sh \
  --dump /restore/newtg-<ts>-main.dump \
  --identity "$AGE_IDENTITY" \
  --expect-version <schema_migrations_version>
```

| 检查 | 结果 | 证据 |
|---|---|---|
| V1 `pg_restore --list` 可解析 | □ 通过 □ 失败 | |
| V2 SHA-256 一致 | □ 通过 □ 失败 | |
| V3 age 解密成功（或未加密） | □ 通过 □ 失败 □ N/A | |
| V4 manifest version/dirty/table_count | □ 通过 □ 失败 | |

### 4.2 恢复

```bash
# 先看计划
deploy/ops/backup/restore.sh \
  --dump /restore/newtg-<ts>-main.dump \
  --target-dsn "$NEWTG_RESTORE_DSN" --check --dry-run

# 确认后执行（新实例；空库可加 --no-clean）
deploy/ops/backup/restore.sh \
  --dump /restore/newtg-<ts>-main.dump \
  --target-dsn "$NEWTG_RESTORE_DSN" --check --yes
```

日志库：

```bash
deploy/ops/backup/restore.sh \
  --dump /restore/newtg-<ts>-logs.dump \
  --target-dsn "$NEWTG_RESTORE_LOGS_DSN" --kind logs --check --yes
```

| 项 | 结果 | 证据 |
|---|---|---|
| 主库 `pg_restore` 退出码 0 | □ 是 □ 否 | |
| 日志库 `pg_restore` 退出码 0 | □ 是 □ 否 | |
| 恢复日志无 `errors ignored` | □ 是 □ 否 | |
| 恢复后 `schema_migrations.dirty=false` | □ 是 □ 否 | |

### 4.3 七条“备份可用”断言（platform-08 §5.3）

| # | 断言 | 判定 | 结果 | 证据 |
|---|---|---|---|---|
| a1 | 表数 == 期望（含 `schema_migrations`） | `SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_type='BASE TABLE'` | □ 通过 □ 失败 | |
| a2 | `usage_logs` 当月分区存在 | `SELECT to_regclass('public.usage_logs_' || to_char(now() AT TIME ZONE 'UTC','YYYYMM'))` | □ 通过 □ 失败 | |
| a3 | `schema_migrations` 版本 == 期望且 `dirty=false` | `SELECT version, dirty FROM schema_migrations` | □ 通过 □ 失败 | |
| a4 | 逐用户 `balance == Σ wallet_transactions.amount` | `deploy/ops/backup/assert_main.sql` | □ 通过 □ 失败 | |
| a5 | 约束/索引计数一致，无 `errors ignored` | `pg_constraint`/`pg_indexes` 对比 + 恢复日志 | □ 通过 □ 失败 | |
| a6 | 业务抽样 3 用户交叉核对 | `users` / `wallet_transactions` / `request_registry` / `usage_logs` | □ 通过 □ 失败 | |
| a7 | L2 解出一条 `channels.credentials` | 用 `CREDENTIAL_ENCRYPTION_KEY` AES-256-GCM 解密并解析 | □ 通过 □ 失败 | |

> a1 的“期望表数”不要写死设计文档的历史值 18；从当前生产只读查询取实际值，
> 并记录在 §1。`assert_main.sql` 只硬断言关键表存在与资金不变量，避免因表数
> 演进造成假失败。

### 4.4 可服务验证

| 项 | 命令 | 结果 |
|---|---|---|
| 启动 `migrate`（若备份版本低于当前代码） | `RUN_MODE=migrate` 一次性 Job | □ 通过 □ 不适用 |
| `/readyz` | `curl -s -o /dev/null -w '%{http_code}' <new-edge>/readyz` | □ 200 □ 其他 |
| 关键页面/管理 API 冒烟 | 登录、用户列表、日志检索 | □ 通过 □ 失败 |
| 切流（如演练包含） | `edge`/Ingress 切到新实例 | □ 完成 □ 未做 |
| 切流后 5xx/告警 | 观察 15–30 分钟 | □ 无异常 □ 有异常（见 §6） |

## 5. 实测结果汇总

| 指标 | 预算/目标 | 实测 | 结论 |
|---|---|---|---|
| RPO（如果可测） | ≤1h（自建期建议） | | |
| 总 RTO | ≤4h（S4 ≤8h） | | |
| T5 重建耗时 | ≤30min | | |
| 断言通过数 | 7/7 | | |
| 数据丢失窗口 | 记录 | | |

## 6. 偏差、失败与处置

| # | 现象 | 影响 | 处置 | 是否阻断上线 | Owner/到期 |
|---|---|---|---|---|---|
| 1 | | | | | |
| 2 | | | | | |

失败处置（platform-08 §5.4）：

- 单份副本轻校验失败 → 标记不可信、保留取证、告警 B1，依赖其余副本；
- 重轨断言失败 → B6 critical，冻结副本可信标记，人工定位（版本/密钥/损坏）；
- 连续两周演练失败 → 拟阻断发版（需 CI/发布门禁 owner 确认）；
- 每日清单缺份 → B7 critical，核对生命周期/权限/作业是否静默停摆。

## 7. 演练后清理

- [ ] 销毁临时实例/数据库（确认不是生产）；
- [ ] 清理临时解密文件（`umask 077`，演练后 `shred`/删除）；
- [ ] 回收临时 DSN/凭据；
- [ ] 归档产物 SHA-256、manifest、断言输出、时间线；
- [ ] 把实测 RTO 回写 `platform-08` §2.3（如适用）与告警 B5/B6；
- [ ] 记录未闭环项与复查日期。

## 8. 签字

| 角色 | 姓名 | 结论 | 时间 |
|---|---|---|---|
| 演练执行人 | | □ 通过 □ 有条件 □ 不通过 | |
| 复核人 | | □ 通过 □ 有条件 □ 不通过 | |
| 运维负责人 | | □ 通过 □ 有条件 □ 不通过 | |

**结论备注**：

> 注意：本模板与 `deploy/ops/backup/` 只提供命令与断言口径；实际 RPO/RTO、
> 异地副本、对象锁、密钥信封取回时间必须在该次演练中实测，不能引用设计文档的
> 建议值冒充实测值。
