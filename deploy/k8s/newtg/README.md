# NEW TG 多机分离部署（Helm chart）

一套 chart 部署五个角色，彼此通过节点池与 NetworkPolicy 隔离：

| 角色 | 资源 | 落在哪类节点 |
|---|---|---|
| gateway（2→8 副本） | Deployment + Service + PDB + HPA | 应用节点池 |
| frontend（2 副本） | Deployment + Service + PDB | 应用节点池 |
| worker（1 副本） | Deployment（Recreate）+ 指标 Service | 应用节点池 |
| web 前端站点 | 与 frontend 同一个镜像（nginx 双根） | 同上 |
| PostgreSQL 16（TLS） | StatefulSet + headless Service + PDB | 数据节点池（PG） |
| Redis 7（TLS + AOF） | StatefulSet + headless Service + PDB | 数据节点池（Redis） |
| migrate | Job（Helm hook，post-install / pre-upgrade） | 任意 |
| 备份 | 3 个 CronJob（hourly / daily / monthly） | 任意 |

设计与决策依据见 `docs/superpowers/specs/2026-10-29-k8s-separated-deployment-design.md`。

---

## 1. 集群前置检查清单

部署前逐条确认，缺一条就会以很难定位的形态失败（例如「部署成功但整站 503」）。

- [ ] **CNI 支持 NetworkPolicy**（Calico / Cilium / kube-router 等）。flannel 默认不支持，
      chart 里 7 条策略会全部静默失效，数据层直接裸奔。
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
- [ ] **节点池与污点**：数据节点池建议打污点独占，用
      `postgres.nodeSelector` / `postgres.tolerations` 和
      `redis.nodeSelector` / `redis.tolerations` 把 PG / Redis 钉到各自的数据节点。
      gateway 与 frontend 的两副本靠 `topologySpreadConstraints`（`DoNotSchedule`）
      保证落在两台机器上——**少于两台应用节点时 Pod 会一直 Pending，这是有意为之**。
- [ ] **监控**：装了 Prometheus 就把 `networkPolicy.monitoringNamespace` 设成它的
      命名空间，否则默认拒绝会让所有 target 变 down（指标看起来像「应用没暴露」）。

## 2. 预创建 Secret（chart 不渲染任何 Secret）

所有口令与证书在集群外生成，chart 只引用名字。**不要**把它们写进 values 文件。

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

DSN 格式（主机名就是 Service 名，CA 由 chart 挂进各 Pod）：

```
postgres://<角色>:<口令>@postgres:5432/newtg?sslmode=verify-full&sslrootcert=/etc/ssl/postgres/ca.crt
postgres://<角色>:<口令>@postgres:5432/newtg_logs?sslmode=verify-full&sslrootcert=/etc/ssl/postgres/ca.crt
postgres://<角色>:<口令>@postgres:5432/newtg_metrics?sslmode=verify-full&sslrootcert=/etc/ssl/postgres/ca.crt
```

> `sslmode=verify-full` 会校验证书里的主机名——服务端证书的 SAN 必须含
> `DNS:postgres`，否则连不上（这是刻意的：能连的只有真的连到了那个库）。
> 库名与 `values.postgres.database/logsDatabase/metricsDatabase` 必须一致。

### 2.2 `newtg-postgres-tls`（`postgres.tls.existingSecret`）

三个键：`ca.crt`、`tls.crt`、`tls.key`。服务端证书 **SAN 必须含 `DNS:postgres`**。

```sh
openssl req -x509 -newkey rsa:4096 -nodes -days 3650 \
  -keyout pg-ca.key -out pg-ca.crt -subj "/CN=newtg-postgres-ca"
openssl req -newkey rsa:2048 -nodes -keyout tls.key -out tls.csr -subj "/CN=postgres"
printf "subjectAltName=DNS:postgres\n" > san.cnf
openssl x509 -req -in tls.csr -CA pg-ca.crt -CAkey pg-ca.key -CAcreateserial \
  -out tls.crt -days 825 -extfile san.cnf
```

gateway / worker / migrate / backup 都把 `ca.crt` 挂到 `/etc/ssl/postgres/ca.crt`；
PG 侧以 0640 + `fsGroup: 70` 读取私钥（PG 拒绝 group/other 可读的 key）。

### 2.3 `newtg-redis-tls`（`redis.tls.existingSecret`）

三个键：`ca.crt`（私有 CA）、`tls.crt`、`tls.key`（服务端证书，SAN 必须含 `redis`）。

```sh
openssl req -x509 -newkey rsa:4096 -nodes -days 3650 \
  -keyout ca.key -out ca.crt -subj "/CN=newtg-redis-ca"
openssl req -newkey rsa:2048 -nodes -keyout tls.key -out tls.csr -subj "/CN=redis"
printf "subjectAltName=DNS:redis\n" > san.cnf
openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out tls.crt -days 825 -extfile san.cnf
```

Redis 侧 CA 挂到 gateway / worker 的 `/etc/ssl/redis/ca.crt`，
由 `REDIS_TLS_CA` 指给 Go 进程（见 `backend/internal/platform/redis.go`）。

### 2.4 备份凭据 `newtg-backup-s3`（`backup.s3.credentialsSecret`）

| 键 | 说明 |
|---|---|
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | **只授 PutObject**（外加 ListMultipartUpload 若用分片上传） |

桶侧要求：

- 开启版本控制与对象锁（防勒索删除）；
- 配三条生命周期规则，按前缀执行保留期：`hourly/` 2 天、`daily/` 30 天、
  `monthly/` 12 个月。**保留期只在桶上实现**——备份凭据没有 DeleteObject，
  Job 自身删不了任何对象；
- 加密用 age：`backup.ageRecipient` 是接收者公钥（可入库），私钥（L1 材料）
  **绝不入库**，离线保管。

创建示例：

```sh
kubectl apply -f deploy/k8s/newtg/namespace.yaml   # 命名空间与 PSA 标签（不归 chart 管）
kubectl -n newtg create secret generic newtg-secrets --from-env-file=secrets.env
kubectl -n newtg create secret generic newtg-postgres-tls \
  --from-file=ca.crt=pg-ca.crt --from-file=tls.crt --from-file=tls.key
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
  monitoringNamespace: monitoring  # 没用 Prometheus 就留空
redis:
  nodeSelector:
    newtg.io/pool: data-redis
  tolerations:
    - key: newtg.io/data
      operator: Exists
      effect: NoSchedule
postgres:
  nodeSelector:
    newtg.io/pool: data-pg
  tolerations:
    - key: newtg.io/data
      operator: Exists
      effect: NoSchedule
EOF

# 2) 渲染自检（不需要集群）
make chart-lint

# 3) 安装 / 升级
helm upgrade --install newtg deploy/k8s/newtg -n newtg -f my-values.yaml --timeout 30m
```

**不要加 `--create-namespace`**：chart 不含 Namespace 对象，命名空间由第 2 节最后
那条 `kubectl apply` 建好（PSA 标签也就在那里）。加了会与归属检查冲突。

`--timeout` 必须大于 migrate Job 的 `activeDeadlineSeconds`（900s）：Helm 默认 300s，
首次安装时镜像拉取 + initdb 就可能超过它，那时 release 被判失败而 Job 还在跑。

### 3.1 首次上线顺序

1. `helm upgrade --install` → 提交全部资源 → `newtg-migrate` Job（post-install hook）
   用一个 `pg_isready` 等待容器等 PG 起来，再跑迁移；失败则 release 不会完成；
2. `kubectl -n newtg get pods` 等 postgres / redis 变成 Ready；
3. gateway / frontend 两副本跨机器就绪，`curl -I https://app.example.com/healthz` 返回 200；
4. 打开 `https://app.example.com/` 走 setup 向导创建管理员，**随后**立即确认
   设置里的站点地址与 `PUBLIC_BASE_URL` 一致；
5. `kubectl -n newtg create job --from=cronjob/newtg-backup-hourly backup-once`，
   **看日志里主库与日志库两份 dump 都成功**，再确认对象落到 `s3://<bucket>/hourly/...`，
   最后做一次 `age -d` + `pg_restore` 演练并记录结论。

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

### 4.3 数据层证书轮换（PG / Redis）

1. 用同一 CA 签一张新证书（SAN 仍然是 `postgres` / `redis`），或先换 CA；
2. 更新对应的 Secret（`newtg-postgres-tls` / `newtg-redis-tls`）；
3. `kubectl -n newtg rollout restart statefulset/postgres`（或 `redis`）；
4. `kubectl -n newtg rollout restart deploy/gateway deploy/worker`
   （客户端要重读 CA；migrate/backup 每次新建 Pod，自然重读）；
5. 验证（以 Redis 为例）：
   ```sh
   kubectl -n newtg exec statefulset/redis -- redis-cli --tls \
     --cacert /etc/redis/tls/ca.crt -a "$REDIS_PASSWORD" --no-auth-warning ping   # → PONG
   ```

换 CA 时必须**先**把新 CA 同时放进 `ca.crt`（追加而不是替换），重启客户端，
再重启服务端，最后移除旧 CA——否则中间一定有一段连接失败。

## 5. 本地校验与「必须上集群验证」的清单

本地只能验证到结构与渲染：

```sh
bash scripts/install-helm.sh   # 装 helm 到 .tools/bin
make chart-lint                # = bash deploy/k8s/newtg/verify.sh
```

`verify.sh` 断言了：副本数、探针路径、terminationGracePeriodSeconds、
连接预算（所有池之和 × 副本数）、TRUSTED_PROXIES 不是 `0.0.0.0/0`、
SSE 注解、frontend 8080/80 映射、PG 与 Redis 的 headless Service 与 PDB 及 TLS、
migrate 的 hook 时机与等库容器、NetworkPolicy 的来源集合（含 backup 与监控）、
三条 CronJob 的加密与双库、initdb 与 backup.sh 的副本与真源逐字节一致。

以下**必须**在真实集群上验收（对应设计文档 §8）：

1. **空集群、只有 values 文件，按本文原样执行一次成功**——上面所有「首次安装」
   类问题只有真装才会暴露；
2. `kubectl get pod -o wide`：gateway / frontend 的两副本在不同节点，PG / Redis 在数据节点；
3. 从非 ingress-nginx 的 Pod 连 gateway:3000 与 postgres:5432，应被拒绝；
4. SSE 连续 5 分钟不断流（验证 LB 空闲超时与缓冲关闭）；
5. 日志里客户端 IP 是真实来源，不是控制器或节点地址；
6. 滚动更新期间持续压测，无 5xx；
7. `kubectl get pods -l app.kubernetes.io/name=worker` 始终 ≤ 1；
8. drain 时 PDB 生效（见 4.2）；
9. 备份产物能 `age -d` 后用 `pg_restore` 还原；
10. 抓包确认 5432 与 6379 上都没有明文（只有 TLS）。
