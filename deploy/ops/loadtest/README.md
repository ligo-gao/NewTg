# NewTG 压测（最小可用）

本目录是 `docs/platform-06-压测方案-设计.md` 的**可执行最小闭环**，不是完整
S1–S13 场景套件。设计文档冻结的 k6 + longconn 形态、完整场景矩阵与 J1–J22
判据仍按设计执行；本目录提供：

- `loadtest.py`：stdlib-only 并发压测 `/v1/chat/completions`（非流式/流式，
  Python 3.10+）；
- `ledger_check.sh`：压后账本一致性断言（usage_logs / wallet_transactions /
  request_registry）。

## 前置条件（缺一不可）

1. **令牌**：`NEWTG_API_TOKEN` 环境变量，值必须是 `loadtest` 分组下的令牌；
   脚本不接受命令行 token，避免 shell history 泄露。
2. **模型**：`NEWTG_MODEL` 或 `--model`，必须是 `model_pricing` 中
   **prompt+completion 单价最低**的文本模型（platform-06 §2.4 护栏 1）。
3. **入口**：`NEWTG_BASE_URL` 或 `--base-url`。压测环境不暴露公网
   （platform-06 §8.4），建议 `http://127.0.0.1:8080`（compose edge）或内网地址。
4. **成本护栏**：`max_tokens` ≤ 64（脚本默认 16）；对真上游并发 ≤ 32/渠道；
   单次运行花费 ≤ 等值 50 元；每次运行前 ≥1 分钟预热（脚本不自动预热，需先
   小流量跑一轮）。
5. **数据隔离**：压测账号/令牌/用量必须挂 `loadtest` 分组；压后按
   platform-06 §7.4 清理，不得污染公开排行榜/实时动态。

## 运行

```bash
export NEWTG_API_TOKEN='<loadtest token>'
export NEWTG_MODEL='<cheapest text model>'
export NEWTG_BASE_URL='http://127.0.0.1:8080'

# 先 dry-run 核对配置（不发请求）
python3 deploy/ops/loadtest/loadtest.py --dry-run

# 非流式：100 请求 / 4 并发
python3 deploy/ops/loadtest/loadtest.py \
  --requests 100 --concurrency 4 \
  --output /tmp/newtg-run.json \
  --request-id-file /tmp/newtg-ids.txt

# 流式（SSE）：记录 TTFT
python3 deploy/ops/loadtest/loadtest.py \
  --stream --requests 50 --concurrency 5 \
  --output /tmp/newtg-stream.json \
  --request-id-file /tmp/newtg-stream-ids.txt

# 恒定到达率（10 req/s），用于接近 S1/S2 的负载形态
python3 deploy/ops/loadtest/loadtest.py --rate 10 --requests 300 --concurrency 10
```

退出码：

- `0`：成功请求 > 0，且 `error_rate ≤ --max-error-rate`（默认 0.05，对齐 J7），
  且（若设置）`p95 ≤ --max-p95-ms`；
- `1`：无成功请求或超过门禁；
- `2`：参数/令牌/模型缺失。

> 客户端延迟**只作对照**，不作为容量判据（platform-06 裁决 G4）。判据主锚是
> 服务端 `platform-05` §3.2 的指标；本脚本的 p95 用于快速发现明显退化。

## 账本一致性断言

`loadtest.py` 只把 **2xx 响应的 `X-Request-ID`** 写入 `--request-id-file`。
`ledger_check.sh` 用这些 ID 做：

| # | 断言 | 依据 |
|---|---|---|
| A | 主库无负余额；逐用户 `balance == Σ wallet_transactions.amount`；无 `settlement_pending` 任务 | platform-08 §5.3 / J22 |
| B | 每个成功 ID 恰好 1 行 `usage_logs`；`status ∈ 0..3`；success 行 `cost >= 0`；refused 行 `cost == 0` | platform-05 §5.1 / billing-07 |
| C | 跨库：成功请求的钱包净额 `Σ amount == -usage_logs.cost`；失败/退款净额 `== 0` | 预扣-结算不变量 |
| D | 指定 request_id 无残留 `request_registry.state='in_flight'` | 崩溃兜底 / J22 |

`status` 枚举：`0=refused / 1=success / 2=error / 3=refunded`
（`backend/internal/billing/engine.go`）。

```bash
export NEWTG_MAIN_DSN='postgres://newtg_app:<password>@postgres:5432/newtg?sslmode=disable'
export NEWTG_LOGS_DSN='postgres://newtg_app:<password>@postgres:5432/newtg_logs?sslmode=disable'

deploy/ops/loadtest/ledger_check.sh \
  --request-ids /tmp/newtg-ids.txt \
  --user-id 12345 \
  --wait-seconds 30
```

- `--wait-seconds` 用于等待 `usage_logs` 异步批量落库（500 条/5 秒）与
  `request_registry` 收敛；超时即失败。
- 压测账号若走**订阅额度**而非钱包，跨库净额断言不适用，加
  `--skip-wallet-ledger`；此时 A/B/D 仍然执行。
- 省略 `--request-ids` 时只做全局断言（A），用于巡检。

## 停压与现场采集（platform-06 §3.5）

脚本不会自动停压；出现以下任一情况必须**人工立即停压**并保留现场：

1. `newtg_billing_quota_clamp_total` 任何增量；
2. 负余额或余额与流水不符（`ledger_check.sh` A）；
3. 预扣悬挂增长（`newtg_worker_abandoned_oldest_age_seconds` / registry in_flight）；
4. `newtg_migration_version{dirty="true"}=1`；
5. `newtg_db_errors_total` 写失败速率；
6. PG 池等待持续 > 5m 不回落；
7. 突破上游并发/花费护栏。

现场采集清单：指标快照、PG 会话/锁、`usage_logs` 尾部、gateway/worker 日志尾部、
`ledger_check.sh` 输出、本次 `run_id` 与 `request-id-file`。

## 与设计文档的差距（必须如实记录）

- 未实现 k6 `constant-arrival-rate` 的完整档位/阈值编排；`--rate` 只是近似。
- 未实现 longconn（S3/S6/S7）、管理面 S8、故障注入 S11、soak S13。
- 未自动计算“附加延迟”（需要服务端指标与同窗上游耗时，按 platform-06 §4.3
  人工汇总）。
- 真实上游不可注入故障，J1/J2/J6 只能近似（platform-06 §11 U-01）。
- 完整报告必须按 platform-06 §7.5 的字段另行归档到
  `loadtest/report/runs/<run_id>/`（本目录不创建该归档，避免把实测产物入库）。
