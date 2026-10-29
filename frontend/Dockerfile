# NEW TG 前端运行镜像：控制台 SPA（frontend/dist）+ 官网静态导出（website/out）。
#
# 构建上下文 = 仓库根（与 backend/Dockerfile 一致），由仓根 .dockerignore 裁剪：
# 它保留 frontend/、website/、packages/public-shell 的源码，排掉 node_modules 与
# 本地产物，同时整体排除 deploy/，所以 nginx.conf 放在 frontend/ 而不是 deploy/。
# 不要改用 Dockerfile.dockerignore：那需要 BuildKit，没有 buildx 组件的主机上
# 会被静默忽略，COPY 拿到的是空目录。
#
# 运行段用 nginx-unprivileged：PSA baseline 要求非 root，普通 nginx:alpine
# 绑 80 需要 root。容器监听 8080，Service 再映射成 80。

FROM oven/bun:1-alpine AS build
WORKDIR /src

# 共享外壳包先装：它的 devDependencies（@types/react）参与 next build 的类型检查。
# 这里的 bun.lock 与 package.json 不同步（仓库现状），只能非 frozen 安装。
COPY packages/public-shell /src/packages/public-shell
RUN cd /src/packages/public-shell && bun install

COPY frontend /src/frontend
# link:../packages/public-shell 在当前 bun（1.4.3）下会报
# "FileNotFound: failed linking dependency"，装不进 node_modules；显式建软链。
RUN cd /src/frontend && { bun install --frozen-lockfile || true; }
RUN cd /src/frontend && mkdir -p node_modules/@newtg \
    && rm -rf node_modules/@newtg/public-shell \
    && ln -sfn ../../../packages/public-shell node_modules/@newtg/public-shell \
    && bun run build

COPY website /src/website
RUN cd /src/website && { bun install --frozen-lockfile || true; }
RUN cd /src/website && mkdir -p node_modules/@newtg \
    && rm -rf node_modules/@newtg/public-shell \
    && ln -sfn ../../../packages/public-shell node_modules/@newtg/public-shell \
    && bun run build

FROM nginxinc/nginx-unprivileged:1.27-alpine
COPY frontend/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /src/frontend/dist /srv/web
COPY --from=build /src/website/out /srv/marketing
EXPOSE 8080
