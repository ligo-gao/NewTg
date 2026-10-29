# NEW TG 多机分离部署（Helm chart）

一套 chart 部署五个角色，彼此通过节点池与 NetworkPolicy 隔离：

| 角色 | 资源 | 落在哪类节点 |
|---|---|---|
| gateway（2→8 副本） | Deployment + Service + PDB + HPA | 应用节点池 |
| frontend（2 副本） | Deployment + Service + PDB | 应用节点池 |
| worker（1 副本） | Deployment（Recreate） | 应用节点池 |
| web 前端站点 | 与 frontend 同一个镜像（nginx 双根） | 同上 |
| PostgreSQL 16 | StatefulSet + headless Service + PDB | 数据节点池（PG） |
| Redis 7（TLS + AOF） | StatefulSet + headless Service + PDB | 数据节点池（Redis） |
| migrate | Job（Helm hook，pre-install/pre-upgrade） | 任意 |
| 备份 | 3 个 CronJob（hourly / daily / monthly） | 任意 |

设计与决策依据见 `docs/superpowers/specs/2026-10-29-k8s-separated-deployment-design.md`。

---

## 1. 集群前置检查清单

部署前逐条确认，缺一条就会以很难定位的形态失败（例如「部署成功但整站 503」）。

- [ ] **CNI 支持 NetworkPolicy**（Calico / Cilium / kube-router 等）。flannel 默认不支持，
      chart 里 6 条策略会全部静默失效，数据层直接裸奔。
- [ ] **云平台负载均衡**：L4 / TCP 透传（不在 LB 上做 TLS 终止、不做 HTTP 健康检查），
      **空闲超时 ≥ 3600 秒**。低于这个值时 SSE 长连接会被静默切断，
      表现为「对话到一半突然结束」而不是报错。
- [ ] **StorageClass**：PG 与 Redis 各需要一块本地盘（`storageClass.data`）。
      PG 默认 50Gi、Redis 10Gi，见 `values.yaml`。
- [ ] **镜像仓库**：可拉取 `gateway`、`frontend`、`backup` 三个镜像；
      私有仓库需要 `image.pullSecrets`。
      三个镜像分别由 `make image`、`make frontend-image`、`make backup-image` 构建。
- [ ] **cert-manager** 已安装，且有一个可用的 DNS-01 ClusterIssuer
      （`certificate.issuerRef.name`）。不用 cert-manager 时把
      `certificate.enabled=false` 并自行提供 `newtg-tls` Secret。
- [ ] **ingress-nginx** 已安装，控制器所在命名空间与 `networkPolicy.ingressNamespace`
      一致（默认 `ingress-nginx`）。
- [ ] **helm ≥ 3.16**（本仓库用 `bash scripts/install-helm.sh` 装到 `.tools/bin/`）。
- [ ] **节点池与污点**：数据节点池建议打污点独占，并用
      `postgres.nodeSelector` / `postgres.tolerations`（Redis 同理，改模板里的对应字段）
      把 PG / Redis 钉在各自的数据节点上。gateway 与 frontend 的两副本靠
      `topologySpreadConstraints`（`DoNotSchedule`）保证落在两台机器上——
      **少于两台应用节点时 Pod 会一直 Pending，这是有意为之**。
- [ ] **命名空间 PSA**：chart 创建的 `newtg` 命名空间带
      `pod-security.kubernetes.io/enforce: baseline`。

## 2. 预创建 Secret（chart 不渲染任何 Secret）

所有口令在集群外生成，chart 只引用名字。**不要**把它们写进 values 文件。

### 2.1 `newtg-secrets`（`secrets.existingSecret`）

| 键 | 用途 | 生成建议 |
|---|---|---|
| `SESSION_SECRET` | 会话签名 | `openssl rand -base64 48` |
| `CREDENTIAL_ENCRYPTION_KEY` | 渠道凭据加密（L2 材料） | `openssl rand -base64 32` |
| `DATABASE_URL` | 应用连主库（`newtg_app`） | 见下方 DSN 格式 |
| `DB_LOGS_DSN` | 应用连日志库（`newtg_app`） | 同上 |
| `PERF_METRICS_DSN` | 指标库（`newtg_app`） | 同上 |
| `DB_RETENTION_DSN` | 保留任务（`newtg_retention`，**只碰日志库**） | 同上 |
| `REDIS_URL` | `rediss://:<REDIS_PASSWORD>@redis:6379/0` | — |
| `METRICS_TOKEN` | `/metrics` Bearer 鉴权 | `openssl rand -hex 32` |
| `MIGRATE_DATABASE_URL` / `MIGRATE_DB_LOGS_DSN` / `MIGRATE_PERF_METRICS_DSN` | 迁移身份（`newtg_migrate`，唯一 DDL 权限） | 同 DSN 格式 |
| `BACKUP_DATABASE_URL` / `BACKUP_DB_LOGS_DSN` | 备份身份（`newtg_backup`，只读） | 同 DSN 格式 |
| `POSTGRES_PASSWORD` | 引导超级用户（仅 initdb 用） | `openssl rand -base64 32` |
| `POSTGRES_APP_PASSWORD` / `POSTGRES_MIGRATE_PASSWORD` / `POSTGRES_BACKUP_PASSWORD` / `POSTGRES_RETENTION_PASSWORD` | 四个应用角色口令（initdb 读取） | 各自独立随机，互不派生 |
| `REDIS_PASSWORD` | Redis `--requirepass` | `openssl rand -base64 32` |

DSN 格式（主机名就是 Service 名）：

```
postgres://<角色>:<口令>@postgres:5432/newtg?sslmode=disable
postgres://<角色>:<口令>@postgres:5432/newtg_logs?sslmode=disable
postgres://<角色>:<口令>@postgres:5432/newtg_metrics?sslmode=disable
```

> 库名与 `values.postgres.database/logsDatabase/metricsDatabase` 必须一致；
> 改了 values 里的库名却不同步改 DSN，Pod 会在启动期以「库不存在」失败。

### 2.2 `newtg-redis-tls`（`redis.tls.existingSecret`）

三个键：`ca.crt`（私有 CA）、`tls.crt`、`tls.key`（服务端证书，SAN 必须含 `redis`）。

```sh
openssl req -x509 -newkey rsa:4096 -nodes -days 3650 \
  -keyout ca.key -out ca.crt -subj "/CN=newtg-redis-ca"
openssl req -newkey rsa:2048 -nodes -keyout tls.key -out tls.csr -subj "/CN=redis"
printf "subjectAltName=DNS:redis\n" > san.cnf
openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out tls.crt -days 825 -extfile san.cnf
```

Redis 侧的 CA 以 `ca.crt` 挂到 gateway / worker 的 `/etc/ssl/redis/ca.crt`，
由 `REDIS_TLS_CA` 指给 Go 进程（见 `backend/internal/platform/redis.go`）。

### 2.3 备份凭据 `newtg-backup-s3`（`backup.s3.credentialsSecret`）

| 键 | 说明 |
|---|---|
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | **只授 PutObject**（外加 ListMultipartUpload 若用分片上传） |

桶侧要求：

- 开启版本控制与对象锁（防勒索删除）；
- 配三条生命周期规则，按前缀执行保留期：`hourly/` 2 天、`daily/` 30 天、
  `monthly/` 12 个月。**保留期只在桶上实现**——备份凭据没有 DeleteObject，
  Job 自身删不了任何对象。
- 加密用 age：`backup.ageRecipient` 是接收者公钥（可入库），私钥（L1 材料）
  **绝不入库**，离线保管。

创建示例：

```sh
kubectl -n newtg create secret generic newtg-secrets --from-env-file=secrets.env
kubectl -n newtg create secret generic newtg-redis-tls \
  --from-file=ca.crt --from-file=tls.crt --from-file=tls.key
kubectl -n newtg create secret generic newtg-backup-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=... --from-literal=AWS_SECRET_ACCESS_KEY=...
```

## 3. 部署

```sh
# 1) 必填项（缺项时 helm 直接渲染失败，不会带默认值上线）
cat > my-values.yaml <<'EOF'
image:
  registry: registry.example.com
  tag: "1.0.0"
publicBaseURL: https://app.example.com
trustedProxies: 10.244.3.0/27     # ingress-nginx 控制器的 Pod 网段
ingress:
  host: app.example.com
storageClass:
  data: local-path
backup:
  image: registry.example.com/newtg/backup:1
  ageRecipient: age1...
  s3:
    bucket: newtg-backup
    region: cn-north-1
networkPolicy:
  nodeCIDR: 10.0.20.0/24          # kubelet 探针来源（节点网段）
EOF

# 2) 渲染自检（不需要集群）
make chart-lint

# 3) 安装 / 升级（migrate Job 作为 pre-install/pre-upgrade hook 先跑）
helm upgrade --install newtg deploy/k8s/newtg -n newtg --create-namespace -f my-values.yaml
```

### 3.1 首次上线顺序

1. `helm upgrade --install` → `newtg-migrate` Job 先执行，失败则整个 release 不会继续；
2. `kubectl -n newtg get pods` 等 postgres / redis 变成 Ready；
3. gateway / frontend 两副本跨机器就绪，`curl -I https://app.example.com/healthz` 返回 200；
4. 打开 `https://app.example.com/` 走 setup 向导创建管理员，**随后**立即在
   设置里确认 `PUBLIC_BASE_URL` 与站点地址一致；
5. `kubectl -n newtg create job --from=cronjob/newtg-backup-hourly backup-once`，
   确认对象落到 `s3://<bucket>/hourly/...` 并记录一次恢复演练结果。

### 3.2 回滚

```sh
helm -n newtg history newtg
helm -n newtg rollback newtg <REVISION>
```

回滚**不会**回滚数据库：迁移是单向的（`migrate` Job 只 up）。涉及 schema 的
升级要先确认 down 迁移可用，或从备份恢复。

## 4. Runbook

### 4.1 PG / Redis 口令轮换

1. 在库内改口令（`ALTER ROLE newtg_app PASSWORD '...'` 等）；
2. 更新 `newtg-secrets` 里四个 `POSTGRES_*_PASSWORD` 键，并同步改掉对应的 DSN 键
   （`DATABASE_URL` / `DB_LOGS_DSN` / `PERF_METRICS_DSN` / `DB_RETENTION_DSN`）；
   迁移身份与备份身份的 `MIGRATE_*` / `BACKUP_*` 键一并更新；
3. `kubectl -n newtg rollout restart deploy/gateway deploy/worker`；
4. 口令轮换**不需要**重跑迁移；下次 `helm upgrade` 时 migrate Job 会用新的
   `MIGRATE_*` 键，所以第 2 步漏掉它们会在下一次升级时才暴露。

> 顺序很重要：先改库，再改 Secret，最后重启。反过来会让应用在两者之间用旧口令连库。

### 4.2 `kubectl drain` 卡住不返回

现象：drain 一直等，`evicted` 不再增加。原因是 PDB：

- gateway / frontend：`maxUnavailable: 1`，2 副本时永远还能再走一个；
- postgres / redis：`minAvailable: 1`——**单副本下任何驱逐都会被拒绝**，
  drain 会一直等下去。

处理：数据节点排空必须人工安排停机窗口，先 `kubectl -n newtg delete pdb postgres`
（或 `redis`）→ 驱逐 → 重建 PDB，并在窗口内接受数据库不可用。
不要在业务高峰期做这件事。

### 4.3 Redis 证书轮换

1. 用同一 CA 签一张新证书（SAN 仍然是 `redis`），或先换 CA；
2. 更新 `newtg-redis-tls`；
3. `kubectl -n newtg rollout restart statefulset/redis`；
4. `kubectl -n newtg rollout restart deploy/gateway deploy/worker`（客户端要重读 CA）；
5. 验证：`kubectl -n newtg exec statefulset/redis -- redis-cli --tls \
   --cacert /etc/redis/tls/ca.crt -a "$REDIS_PASSWORD" --no-auth-warning ping` 返回 `PONG`。

换 CA 时必须**先**把新 CA 同时放进 `ca.crt`（追加而不是替换），重启客户端，
再重启服务端，最后移除旧 CA——否则中间一定有一段连接失败。

## 5. 本地校验与「必须上集群验证」的清单

本地只能验证到结构与渲染：

```sh
bash scripts/install-helm.sh   # 装 helm 到 .tools/bin
make chart-lint                # = bash deploy/k8s/newtg/verify.sh
```

`verify.sh` 断言了：副本数、探针路径、terminationGracePeriodSeconds、
连接预算（HPA 上限 × DB_MAX_OPEN_CONNS）、TRUSTED_PROXIES 不是 `0.0.0.0/0`、
SSE 注解、frontend 8080/80 映射、PG 与 Redis 的 headless Service 与 PDB、
NetworkPolicy 的 nodeCIDR 钉子、三条 CronJob 的加密与双库、initdb 与 backup.sh
的副本与真源逐字节一致。

以下**必须**在真实集群上验收（对应设计文档 §8）：

1. `kubectl get pod -o wide`：gateway / frontend 的两副本在不同节点；
2. 从非 ingress-nginx 的 Pod 连 gateway:3000 与 postgres:5432，应被拒绝；
3. SSE 连续 5 分钟不断流（验证 LB 空闲超时与缓冲关闭）；
4. 日志里客户端 IP 是真实来源，不是控制器或节点地址；
5. 滚动更新期间持续压测，无 5xx；
6. `kubectl get pods -l app.kubernetes.io/name=worker` 始终 ≤ 1；
7. drain 时 PDB 生效（见 4.2）；
8. 备份产物能 `age -d` 后用 `pg_restore` 还原；
9. 抓包确认 6379 上没有任何明文（只有 TLS）。
