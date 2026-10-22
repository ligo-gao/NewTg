# 备份 / 恢复 / 校验

依据 `docs/platform-08-备份恢复与灾备演练-设计.md`：

- 备份对象是 **PostgreSQL 主库 + 日志库两个 database**（§3.1）；Redis、
  `web/dist` 不备份（§3.1）。
- 自建期 RPO ≤ 1h、RTO ≤ 4h（S4 区域级 ≤ 8h）为**建议值，待产品裁决与 M7
  实测**（§2.2/§2.3）。本目录只提供执行脚本，不承诺数值。
- 每次备份后做轻校验 V1–V4（§5.1）；重轨演练记录模板见
  `../runbooks/dr-drill-template.md`。

## 脚本

| 脚本 | 作用 |
|---|---|
| `backup.sh` | 主库 + 日志库 `pg_dump -Fc`，生成 `.sha256` 与 `.manifest.json`；可选 age 加密 |
| `verify.sh` | V1 结构 / V2 校验和 / V3 解密 / V4 manifest，不落数据 |
| `restore.sh` | 恢复到显式指定的新实例，可选 `--check` 跑断言 |
| `assert_main.sql` | 主库断言：关键表、非 dirty、负余额、`balance == Σ amount` |
| `assert_logs.sql` | 日志库断言：关键表、非 dirty、status 枚举、当月分区、重复 request_id |

全部脚本支持 `--help`，失败即非零退出；不写死口令，不打印 DSN。

## 备份

```bash
export NEWTG_MAIN_DSN='postgres://newtg_backup:<password>@postgres:5432/newtg?sslmode=disable'
export NEWTG_LOGS_DSN='postgres://newtg_backup:<password>@postgres:5432/newtg_logs?sslmode=disable'

deploy/ops/backup/backup.sh --out-dir /backups
```

产物：

```
newtg-20261022T010000Z-main.dump
newtg-20261022T010000Z-main.dump.sha256
newtg-20261022T010000Z-main.dump.manifest.json
newtg-20261022T010000Z-logs.dump
...
```

### 在 compose 内执行（宿主机无 PG 客户端时）

不要改动正在运行的 `postgres`/`redis` 容器；用一次性容器挂载脚本与备份目录：

```bash
cd deploy
docker run --rm \
  --network newtg_backend \
  -v "$PWD/backups:/backups" \
  -v "$PWD/ops/backup:/ops:ro" \
  -e NEWTG_MAIN_DSN -e NEWTG_LOGS_DSN \
  postgres:16-alpine bash /ops/backup.sh --out-dir /backups
```

`NEWTG_MAIN_DSN` / `NEWTG_LOGS_DSN` 由当前 shell 环境注入，不要把口令写进
命令行或 compose 文件。网络名以 `docker network ls` 实际值为准。

### 调度（外部，不在脚本内）

**systemd timer（自建期，示意）**：

```ini
# /etc/systemd/system/newtg-backup.service
[Service]
Type=oneshot
User=newtg
EnvironmentFile=/etc/newtg/backup.env   # chmod 600，含两个 DSN
ExecStart=/opt/newtg/deploy/ops/backup/backup.sh --out-dir /backups
```

```ini
# /etc/systemd/system/newtg-backup.timer
[Timer]
OnCalendar=hourly
Persistent=true
[Install]
WantedBy=timers.target
```

**K8s（M7）**：由 `CronJob` 承载（`platform-08` §3.6 / `platform-03` §8.2），
周期与 GFS 保留按 `platform-08` §3.2；本目录不提供 K8s 清单全集。

GFS 分层（hourly 48h / daily 30d / monthly 12m）与对象存储上传、版本化、
对象锁、离线月基线属于**编排/存储侧**，本脚本只产出单次全量产物。

## 校验

```bash
deploy/ops/backup/verify.sh --dump /backups/newtg-20261022T010000Z-main.dump
```

加密包：

```bash
AGE_IDENTITY=/secure/age.key \
  deploy/ops/backup/verify.sh \
    --dump /backups/newtg-20261022T010000Z-main.dump.age \
    --expect-version 20261024100000
```

`--expect-version` 应取目标发布单元在 `schema_migrations` 中的版本；不传则只
校验 manifest 自洽。`--expect-tables` 同理——设计文档的「18 表」是冻结口径的
历史值，当前仓库实际表数更多，**不要**把 18 写死进脚本；演练时从生产只读
查询取实际值并记录。

## 恢复

**纪律：恢复到新实例，不对生产实例执行 `pg_restore --clean`。**

```bash
export NEWTG_RESTORE_DSN='postgres://newtg_migrate:<password>@127.0.0.1:55432/newtg_restore?sslmode=disable'

# 1) 先看计划（不写库）
deploy/ops/backup/restore.sh \
  --dump /backups/newtg-20261022T010000Z-main.dump \
  --target-dsn "$NEWTG_RESTORE_DSN" --check --dry-run

# 2) 确认后执行（--no-clean 仅用于全新空库）
deploy/ops/backup/restore.sh \
  --dump /backups/newtg-20261022T010000Z-main.dump \
  --target-dsn "$NEWTG_RESTORE_DSN" --check --yes
```

日志库同理，加 `--kind logs` 并用日志库的目标 DSN。

恢复后：

1. 断言失败 → **不上线**（`platform-08` §5.4），保留实例取证。
2. 备份 schema 版本低于当前代码 → 先跑一次性 `migrate`（`RUN_MODE=migrate`），
   再启动应用（`platform-08` §4.4；**不要**用新代码硬启旧库）。
3. `/readyz` 返回 200 后才切流；切流步骤见 `platform-08` §4.3 与
   `../runbooks/canary-rollback.md`。

## 安全

- `pg_dump` 使用 `newtg_backup`（只读 + `pg_dump`）；恢复演练使用独立临时实例
  的 `newtg_migrate` 或临时超级用户，**不得**在生产 DSN 上演练。
- 备份保留 `GRANT`/`REVOKE`（只加 `--no-owner`，不加 `--no-privileges`）；目标
  实例必须先按 `deploy/postgres-init/10-roles.sql` 建好四个角色，否则恢复时的
  `GRANT` 会因角色不存在而报错。`--no-owner` 让对象归恢复执行者所有，避免因
  原属主角色不存在而失败。
- `age` 接收者公钥可入库；私钥（L1）与 `CREDENTIAL_ENCRYPTION_KEY`（L2）
  按 `platform-08` §3.4 分离托管，绝不进入本目录、备份包或 Git。
- 产物目录权限由 `umask 077` 收紧；上传对象存储后按保留策略清理近端暂存。
