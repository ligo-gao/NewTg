# 备份用的工具镜像：postgres 客户端 + age + aws-cli。
#
# 为什么需要单独一个镜像：应用镜像是 distroless 的 Go 二进制，没有 pg_dump；
# postgres:16-alpine 有 pg_dump 但没有 age/aws。备份 Pod 以非 root 运行且
# 不在启动时联网装包（每小时一次的 Job 装包既慢又依赖外网）。
#
# 构建（上下文就是这个目录，不读仓根 .dockerignore）：
#   docker build -t registry.example.com/newtg/backup:1 \
#       -f deploy/ops/backup/Dockerfile deploy/ops/backup
FROM postgres:16-alpine

RUN apk add --no-cache age python3 aws-cli \
    && rm -rf /var/cache/apk/*

# 与 postgres 镜像的 postgres 用户一致；PSA baseline 要求非 root。
USER 70:70
ENTRYPOINT ["/bin/sh"]
