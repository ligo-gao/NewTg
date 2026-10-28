# Prometheus 告警规则

`alerts.yaml` 是**纯 Prometheus rule file**（顶层 `groups:`），可直接被
Prometheus `rule_files` 加载；也可包进 Prometheus Operator 的
`PrometheusRule`（见文件头注释）。

## 规则分组

| 组 | 覆盖 |
|---|---|
| `newtg-availability` | 实例失联、入口 5xx、入口 p95 |
| `newtg-relay` | 中继错误率、渠道错误率/自动禁用、TTFT、入口延迟 |
| `newtg-billing` | 结算停摆、钳制、402 突增（balance_low 信号）、预扣残留、结算积压 |
| `newtg-worker` | 关键任务失败/停摆、从未成功、租约过期、Stream 积压 |
| `newtg-notify` | Webhook / 通知投递失败 |
| `newtg-database` | 连接池饱和/等待、DB 错误、迁移 dirty |
| `newtg-dependency` | Redis / PG 依赖、配置广播 |
| `newtg-selfmonitoring` | Prometheus 自身 TSDB 基数 |

## 阈值来源与“待实测”

- 来源为 `platform-05 §6.1 A<n>` 的规则，阈值逐字引用设计文档；其中
  A1/A3/A4/A14/A15 等仍带【假设】标记，必须用 M7 压测稳态分布校准
  （`platform-06` §6.2 R1/R2），校准后回写 `platform-05` 并删除【假设】。
- 来源为 `newtg-ops-initial` 的规则（5xx 比率 0.01、入口 p95 2s、
  402 增量 20）是本仓库新增的【假设】初值，**不是设计文档已冻结的数值**。
- 有设计来源但当前实现未暴露的指标：
  - `newtg_worker_webhook_failures_total`（platform-05 §3.2.5 #36）
  - `newtg_worker_notifications_total`（#35）
  - `newtg_worker_task_lease_expired_total`（#27）
  - `newtg_billing_reserved_credits`（#23）
  - `newtg_db_errors_total`（#40）
  - `newtg_config_reload_total`（#48）

  对应规则保留在文件中，指标落地后自动生效；在此之前它们不会触发（没有
  时间序列）。不要为了“让它触发”而伪造指标或改写 fan-out。

## 加载与校验

```bash
# Prometheus 直接加载
promtool check rules deploy/ops/prometheus/alerts.yaml

# 无 promtool 时至少做 YAML 结构检查（仓库 CI 可用的最小校验）
python3 - <<'PY'
import yaml
with open('deploy/ops/prometheus/alerts.yaml', encoding='utf-8') as f:
    doc = yaml.safe_load(f)
assert isinstance(doc.get('groups'), list) and doc['groups']
for g in doc['groups']:
    assert g.get('name') and isinstance(g.get('rules'), list)
    for r in g['rules']:
        assert r.get('alert') and r.get('expr')
        assert r.get('labels', {}).get('severity') in {'info', 'warning', 'critical'}
        assert 'runbook' in r.get('annotations', {})
print('alerts.yaml: OK', sum(len(g['rules']) for g in doc['groups']), 'rules')
PY
```

## Alertmanager 配套（需在真实环境配置）

`alerts.yaml` 不含 receiver。按 `platform-05` §6.3 的裁决复用 N8 业务
Webhook（HMAC-SHA256、1/5/25 分钟重试、连续 >50 暂停）+ 邮件 + IM，并使用
`platform-05` §6.2 的去重/抑制：

- 去重键：`(alertname, labels)`；
- `NewTGDependencyDown` 抑制派生的错误率/池告警（`inhibit_rules`）；
- 静默必须记录原因与到期时间，不得长期静默资损红线规则（A5/A6/A16/A19）。

## 真实环境验收（本目录无法完成）

1. 规则被 Prometheus/Operator 成功加载（`promtool check rules` 或 UI 规则页）。
2. 每条 critical 规则至少做一次**受控触发**（例如停 Redis 触发 A17；在
   非生产环境把 `newtg_worker_task_last_success_timestamp_seconds` 置旧触发
   A11），并确认 Alertmanager 收到、去重、抑制生效。
3. 用 `platform-06` 的压测稳态数据校准 A1/A3/A4/A14/A15 等阈值并回写设计。
