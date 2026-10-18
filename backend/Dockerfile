# backend/Dockerfile —— NewTG 单二进制镜像（docs/platform-02 §5）
#
# 构建上下文 = 仓库根（不是 backend/）：运行镜像必须随附 THIRD_PARTY_NOTICES.md
# （SecLists 前 3000 行编译进二进制，MIT 要求许可文本随副本分发）。把上下文
# 上提到仓根后 notices 有唯一真源，Dockerfile / Makefile / 两个 workflow 不再
# 各自维护一份拷贝。构建上下文由仓根 .dockerignore 裁剪，frontend/node_modules、
# deploy/.env 等不会被送进 daemon。
#
# 一个镜像内含单个多角色二进制 newtg，运行期由 RUN_MODE=gateway|worker|migrate
# 分派，不需要为角色单独构建镜像。

# ---- 阶段 1：构建 ----
FROM golang:1.26-alpine AS builder

# 版本信息真源为仓库根 VERSION，由 make image 通过 --build-arg 传入。
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

# 模块代理可由构建参数覆盖（受限网络环境用 --build-arg 指定可达镜像）。
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

# 静态链接，避免运行阶段依赖 libc。
ENV CGO_ENABLED=0

WORKDIR /src

# 依赖层缓存：源码变化时无需重新下载模块（路径相对仓库根上下文）。
COPY backend/go.mod backend/go.sum ./
RUN go mod download

# 只拷 Go 模块目录（cmd/internal 与 go.mod 都在其中）；仓根其它目录不进构建层。
COPY backend/ ./
RUN go build -trimpath \
      -ldflags "-s -w \
        -X newtg/gateway/internal/version.Version=${VERSION} \
        -X newtg/gateway/internal/version.Commit=${COMMIT} \
        -X newtg/gateway/internal/version.BuildTime=${BUILD_TIME}" \
      -o /out/newtg ./cmd/newtg

# ---- 阶段 2：运行 ----
FROM alpine:3.21

# TLS 根证书与时区数据；非 root 运行（uid 冻结为 65532）。
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S -g 65532 newtg \
 && adduser -S -D -H -u 65532 -G newtg -s /sbin/nologin newtg \
 && mkdir -p /data \
 && chown 65532:65532 /data

COPY --from=builder /out/newtg /newtg

# 迁移 SQL：RUN_MODE=migrate 经内嵌文件系统执行。
COPY backend/migrations /migrations
# 日志库迁移单独成目录（Task 8）。
COPY backend/migrations-logs /migrations-logs

# 第三方许可文本随镜像分发：MIT/ISC 要求许可与版权声明随软件副本保留，
# 而内嵌口令表已经编译进 /newtg——源码仓库里的 notices 对镜像使用者不可见。
# 固定路径 /usr/share/licenses/newtg/ 是发行版惯例位置，root 可读、非 root 可读。
COPY THIRD_PARTY_NOTICES.md /usr/share/licenses/newtg/THIRD_PARTY_NOTICES.md

EXPOSE 3000
WORKDIR /data

USER newtg

# 探针使用二进制内置子命令，镜像无需 wget/curl。
HEALTHCHECK --interval=10s --timeout=3s --retries=5 --start-period=30s \
  CMD ["/newtg","healthcheck","--url","http://127.0.0.1:3000/readyz"]

ENTRYPOINT ["/newtg"]
