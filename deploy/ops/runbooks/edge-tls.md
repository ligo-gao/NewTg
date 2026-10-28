# Edge TLS Runbook（内部 CA、证书续期与 HTTPS 强制）

> 依据：`deploy/edge/Caddyfile`（:80 跳转 + :8443 文件证书）、
> `deploy/edge/gen-internal-ca.sh`（签发脚本）、`deploy/ops/runbooks/go-live-checklist.md`
> 4.7（上线检查的证书项）。
> 本 runbook 只给执行步骤；证书参数（SAN、有效期）以脚本为准，不在文档里重复定义。

## 0. 适用范围与前提

- edge 使用 **内部 CA + 分发给客户端的根证书**：Let's Encrypt 无法为内网 IP
  （`10.211.55.5`）签发证书。
- 证书是 **文件证书**（`deploy/edge/certs/newtg.crt` / `newtg.key`），
  **Caddy 不会自动续期**。叶证书有效期 365 天，根 CA 3650 天。
- Caddyfile 里 `admin off`，**没有 reload 通道**：换证书只能用重启容器生效。
- P2-8 之后 `:80` 不再明文服务：除 `/healthz`（容器自探活）外，任何 Host 的
  `:80` 请求都 **301 到 `https://<同一 Host>:8443<uri>`**。

## 1. 证书清单

| 文件 | 位置 | 用途 | 分发范围 |
|---|---|---|---|
| `newtg-root-ca.crt` | `deploy/edge/certs-internal-ca/` | 根证书（客户端信任） | **每个访问客户端**必须安装 |
| `newtg-root-ca.key` | `deploy/edge/certs-internal-ca/` | 根 CA 私钥 | 仅签发机（offline 保管，不上 edge） |
| `newtg.crt` / `newtg.key` | `deploy/edge/certs/`（线上，只读挂载进容器） | 叶证书（SAN 见脚本 `SAN=`） | 仅 edge |

staging 目录 `certs-internal-ca/` 与线上目录 `certs/` 分离是脚本的安全闸门：
输出目录落在线上目录内时脚本直接拒绝运行。

## 2. 巡检（每月一次，或 go-live-checklist 4.7）

```bash
# 剩余天数（< 30 天必须安排续期）
openssl x509 -in deploy/edge/certs/newtg.crt -noout -enddate -subject -ext subjectAltName

# 端到端：链是否被根证书验证通过、TLS 是否可握手
curl --cacert deploy/edge/certs-internal-ca/newtg-root-ca.crt \
  -sS -o /dev/null -w '%{http_code}\n' https://10.211.55.5:8443/readyz

# HTTPS 强制：任何 Host 的 :80 都应 301 到 :8443（不是 200 明文页面）
curl -sS -o /dev/null -w '%{http_code} %{redirect_url}\n' -H 'Host: any.example' http://10.211.55.5:8080/
```

通过标准：链验证通过、`/readyz` 返回 200、`:80` 返回 `301 https://…:8443/…`。

## 3. 续期步骤（叶证书）

```bash
# 1) 在 staging 目录签发新叶证书（复用已有根 CA，不改变客户端信任）
deploy/edge/gen-internal-ca.sh

# 2) 核对 staging 产物的 SAN 与有效期（SAN 需覆盖实际访问用的 IP/域名）
openssl x509 -in deploy/edge/certs-internal-ca/newtg.crt -noout -dates -ext subjectAltName

# 3) 原子替换线上证书（脚本会先备份到 certs/backup-<UTC 时间戳>/）
deploy/edge/gen-internal-ca.sh --apply

# 4) 重启 edge 让新证书生效（admin off，没有 reload）
docker compose -f deploy/docker-compose.yml restart edge

# 5) 校验（同 §2 的三条命令）——全部通过才算完成
```

要点：

- **不要**加 `--force`：它会轮换根 CA，已安装旧根证书的客户端会全部失效。
  只有根私钥泄漏或到达 3650 天上限时才轮换，且必须重新分发根证书。
- 新增访问用的域名/IP 时，改脚本里的 `SAN=` 再执行 §3；否则跳转后的
  HTTPS 会因证书不匹配被浏览器拦截（`:80` 的 301 本身不校验证书）。
- 续期不改变 `deploy/edge/certs/` 的挂载方式，不需要改 compose。

## 4. 回滚

`--apply` 会把旧证书备份到 `deploy/edge/certs/backup-<UTC 时间戳>/`：

```bash
cp -a deploy/edge/certs/backup-<时间戳>/newtg.crt \
      deploy/edge/certs/backup-<时间戳>/newtg.key deploy/edge/certs/
docker compose -f deploy/docker-compose.yml restart edge
```

回滚后按 §2 校验。备份目录不要删除：客户端仍信任的根证书与旧叶证书需要能
在事故复盘时对得上。

## 5. HTTPS 强制与 edge 自探活（P2-8）

- `:80` 的两个入口：
  - `GET|HEAD /healthz` → 反向代理到 `gateway:3000`（**不**跳转），
    供 compose 的 edge healthcheck 使用：它同时证明「Caddy 在听」与
    「Caddy 能到达网关」。只探端口开放会把「网关全挂」误报成健康。
  - 其余一切 → `301 https://{http.request.host}:8443{uri}`。
- edge healthcheck 的探针命令用 **busybox `nc`**：`caddy:2-alpine` 里没有
  curl/openssl，仓库规则 R-07 也禁止 `wget`。探针期望首行状态码为 `200`。
- 若 edge 显示 `unhealthy` 而容器在跑：
  1. `docker compose logs edge --tail 50` 看 Caddy 是否启动失败（配置/证书）；
  2. 容器内执行一次探针命令（`docker compose exec edge sh -c "<healthcheck test>"`）；
  3. 网关不可用时 `:80 /healthz` 返回 503 —— 这是「edge 正常、上游异常」，
     按网关侧 runbook（canary-rollback §4）处理，不要重启 edge。

## 6. 已知边界

- 客户端必须预先安装根证书；未安装时会看到证书警告（预期行为，不是故障）。
- `:80` 的 301 会把**任意** Host 跳到 `https://<host>:8443`；用未列入 SAN 的
  主机名访问时，跳转后由浏览器拦下。需要支持新主机名时按 §3 重签叶证书。
- 本 runbook 不覆盖 K8s Ingress 的证书管理：生产灰度的 TLS 由集群 Ingress
  负责，见 `docs/platform-07-发布与灰度-设计.md`。
