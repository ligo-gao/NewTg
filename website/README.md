# NEWTG 官网

NEWTG 的营销首页，Next.js 16（App Router）+ React 19 + TypeScript（strict）+ Tailwind CSS 4，代码示例由 shiki 在服务端高亮。视觉与交互以 `../demos/landing-page/index.html` 为已确认的参考稿。

## 运行

```bash
cd website
bun install
bun run dev --port 3100      # 3000 端口可能被网关占用
```

- 开发：http://localhost:3100
- 构建：`bun run build`（`output: "export"`，产物在 `out/`）
- 本地预览构建产物：`bunx serve out`
- 测试：`bun run test`（Vitest + Testing Library）
- 类型检查：`bun run typecheck`

## 部署

官网以静态导出接入现有 NEWTG 栈，由 Caddy 直接托管：

- `website/out/` 挂载到 edge 容器的 `/srv/marketing`
- Caddy 用 `@marketing_home` 接管 `/`、`/en/`、`/zh-TW/`、`/fr/`、`/ru/`、`/ja/`、`/vi/`，并用 `@marketing_assets` 接管 `/_next/*`、`/icon.svg`、`/og.png`
- `out/robots.txt` 与 `out/sitemap.xml` 由 `src/app/robots.ts`、`src/app/sitemap.ts` 生成；edge 的 `@marketing_assets` 需把这两个路径一并纳入才会对外生效（当前 Caddyfile 尚未包含）
- 其余路径继续落到控制台 SPA（`/srv/web`），所以 `/sign-in`、`/dashboard`、`/pricing`、`/docs` 等保持原路径不变
- 官网里的「登录 / 免费开始使用 / 控制台 / 文档」分别指向 `/sign-in`、`/sign-up`、`/dashboard`、`/docs`

重新部署：

```bash
cd website && bun run build
cd ../deploy && docker compose up -d --force-recreate --no-deps edge
```

必须带上 `--force-recreate`：Next 每次构建会重建 `out/` 目录，容器里的 bind mount 仍指向旧 inode，只替换文件不会生效。

后端性能接口改动需要重建网关镜像并重建 gateway / worker：

```bash
docker build --platform linux/arm64 \
  --build-arg GOPROXY=https://goproxy.cn,direct \
  -f backend/Dockerfile -t newtg/gateway:dev .
cd deploy && docker compose up -d --force-recreate --no-deps gateway worker
```

## 多语言

- 7 种语言，独立静态路由：`/`（简体中文）、`/en/`、`/zh-TW/`、`/fr/`、`/ru/`、`/ja/`、`/vi/`
- 导航右侧（移动端为顶栏紧凑码 + 抽屉内完整选择器）的语言选择器跳转到对应路由，并把选择写入 `localStorage` 的 `newtg-locale`
- 文案在 `src/config/copy/<locale>.ts`；`Copy` 接口强制所有语言的字段一致，漏翻会编译失败
- `next.config.ts` 开启 `trailingSlash`，每种语言导出为 `out/<locale>/index.html`
- `<html lang>` 由路由的根布局在构建期写入：`(default)/layout.tsx` 用默认语言，`(localized)/[locale]/layout.tsx` 用路由参数；不再依赖水合后的脚本
- `alternates.languages` 为每个页面输出完整 hreflang（含 `x-default`）；`/robots.txt` 与 `/sitemap.xml` 由 `src/app/robots.ts`、`src/app/sitemap.ts` 生成
- 当前 7 套文案由本次实现提供；上线前建议由母语者校对 fr / ru / ja / vi / zh-TW

## 定价页

- 路由：`/pricing/`（简体中文）与 `/<locale>/pricing/`（其余 6 种语言），全部静态导出。
- 页面不显示「首页 / 定价」面包屑，直接从标题开始。
- 数据来自公开的 `GET /api/pricing`：分页、搜索、排序、厂商 / 能力 / 上下文 / 端点 / 计费方式筛选、分组、`quotaType` 都走服务端查询参数；展示状态（表格/卡片、1M/1K、$ 参考）只改 URL、不重新请求。
- 表格列出输入 / 输出 / 缓存读 / 缓存写 / 按次 / 分组价；基准价固定，分组价使用服务端返回的 `group_price`（不在前端乘倍率）。
- 展开行：完整价签（基准 vs 分组、充值参考）、费用估算（输入/输出 tokens + 缓存命中率，或按次请求数）、模型信息与复制/详情入口。
- 卡片视图与移动端（≤760px 强制卡片）、最多 3 个模型对比、分页。
- Caddy 只把精确的 `/pricing` 与 `/<locale>/pricing` 交给官网；`/pricing/{model_id}` 继续由控制台详情页处理。

## 明暗主题

- 首次访问跟随系统 `prefers-color-scheme`；导航栏右侧（移动端在汉堡旁）的月亮/太阳按钮手动切换。
- 手动选择写入 `localStorage` 的 `newtg-theme`，之后优先于系统偏好；再次切换即覆盖。
- `layout.tsx` 的 `<head>` 内联脚本在首帧前写入 `data-theme`，避免亮暗闪烁；`suppressHydrationWarning` 避免水合告警。
- 所有颜色收敛为 `globals.css` 中的语义 token（`:root` 亮色 / `[data-theme="dark"]` 暗色），组件只使用 `bg-*` / `text-*` token。
- 代码块用 shiki 双主题（`github-light` / `github-dark-default`），通过 `--shiki-light` / `--shiki-dark` 变量随主题切换。
- 亮色主色使用更深的青色 `#077486`（浅底对比度 5.0+）；主按钮仍用亮青底 + 深色文字。

访问：`http://10.211.55.5:8080/`（会 301 到 `https://10.211.55.5:8443/`，自签证书需手动信任）。

## 结构

```
src/
  app/
    (default)/layout.tsx        # 默认语言根布局：字体、metadata、OG、`html lang`
    (default)/page.tsx          # 首页
    (default)/pricing/page.tsx  # /pricing
    (localized)/[locale]/layout.tsx          # 按路由参数写 `html lang`
    (localized)/[locale]/page.tsx            # /<locale>/
    (localized)/[locale]/pricing/page.tsx    # /<locale>/pricing/
    robots.ts           # /robots.txt
    sitemap.ts          # /sitemap.xml（含 hreflang）
    globals.css         # 设计 token、按钮、shiki 覆盖、动效
  components/landing/
    site-header.tsx     # 固定毛玻璃导航（客户端）
    hero.tsx            # 首屏（服务端，shiki）
    code-card.tsx       # curl 卡片 + 复制（客户端）
    trust-band.tsx
    landing-page.tsx    # 7 种语言共用的页面组合
    home-data.tsx       # 拉取目录、随机 6 个、拉取性能指标（客户端）
    model-explorer.tsx  # 搜索 / 筛选 / 对比（客户端）
    model-card.tsx
    feature-grid.tsx
    performance-section.tsx  # 近 7 天真实性能表（客户端）
    pricing-page.tsx    # 定价页外壳（服务端）
    pricing-workbench.tsx  # 定价工作台：筛选/表格/卡片/分页（客户端）
    pricing-detail.tsx  # 完整价签 + 费用估算
    pricing-cards.tsx   # 定价卡片
    pricing-compare.tsx # 对比弹窗
    closing-cta.tsx
    site-footer.tsx
  components/
    language-switcher.tsx  # 7 语言原生选择器（客户端）
  config/
    locales.ts          # 语言代码、路由、endonym、OG locale
    copy/
      types.ts          # Copy 接口（字段一致性由类型强制）
      zh-CN.ts          # 简体中文（默认 /）
      en.ts zh-TW.ts fr.ts ru.ts ja.ts vi.ts
    site.ts             # 域名与链接
  lib/
    pricing-api.ts      # /api/pricing 与 metrics 的类型和请求
    pricing-filter.ts   # 搜索 + 厂商筛选
    pricing-format.ts   # 上下文/价格/TTFT/TPS 格式化
    pick-random.ts      # Fisher–Yates 随机抽样
    highlight.ts        # shiki
```

## 实时数据

- 模型目录来自 `GET /api/pricing`（匿名，`page_size=100`）。默认从后台可见模型中随机取 8 个；搜索与厂商筛选作用于完整目录（最多渲染前 24 个结果）。
- 卡片字段全部来自接口：上下文、最大输出、输入/输出价。价格由积分数按 `meta.quota_per_unit` 换算成 `meta.credit_symbol` 货币，按每 1M tokens（或按次）展示；`null` 显示 —，`0` 显示免费。
- 性能表调用 `GET /api/pricing/{model_id}/metrics?range=7d`，展示最近一次定时探测的 `latest_test`：首字时延与总时延的 P50/P95、输出速度、成功率、样本数与探测时间。超过 36 小时的探测整行显示 —，不回退到 7 天汇总值。
- 性能表随机取 8 个可对话模型，与模型卡片一样由 `HomeDataProvider` 统一加载。
- 后端实现：`backend/internal/httpapi/handlers_public_pricing.go` 与 `backend/internal/store/public_model_metrics_repo.go`；OpenAPI 已同步。
- 当前部署的 `model_pricing` 与 `usage_logs` 均为空，所以首页显示空态；在后台添加模型并产生调用后会自动出现真实数据。

## 其他占位

- 信任数字（100+ / 99.9% / 10ms / 10,000+）来自需求文案，页面注明「以上线版本为准」；上线前请替换为真实统计。
- `#community`、`#status` 仍为页内占位；`https://newtg.ai`、`https://api.newtg.ai` 为占位域名。
- `public/og.png` 由参考稿渲染生成（1200×630）。

## 验证

- `bun run test`：14 个测试文件、57 个用例通过（实时目录、搜索/筛选、空态、对比弹窗、性能表、定价工作台/详情估算/卡片/对比、主题、导航、复制、格式化、随机抽取、语言切换）。
- `bun run typecheck` / `bun run build`：通过（`/` + 6 个语言路由的静态预渲染）。
- 多语言实测：7 个路由均 200，标题随语言变化；在浏览器里把选择器切到 `ja` 后跳转 `/ja/`、`html lang=ja`、`newtg-locale=ja`、导航与正文均为日文。
- 定价页实测（浏览器注入示例目录，不写数据库）：7 个模型渲染、展开估算 ¥4.13、分组 vip ×1.20、1K + $ 参考、卡片视图、390px 无横向溢出、控制台 0 报错；`/pricing/`、`/en/pricing/`、`/ja/pricing/` 标题与 h1 正确；`/pricing/{id}` 仍由控制台承载。
- 后端：`go test ./internal/httpapi/ ./internal/store/` 通过；公开指标单测覆盖 200 / 404 / 400 / 503 / 304、无样本 null、低样本与 TPS 计算。
- 接口实测（临时探针，验证后已清理）：近 7 天 4 次调用 → `avg_latency_ms=875`、`avg_ttft_ms=200`、`tps≈85.7`、`sample_size=4`、`success_rate=0.75`。
- 浏览器 QA：1920 / 1440 / 390 / 375 宽度；375 无横向溢出；移动端汉堡菜单、对比栏、弹窗正常；空库下模型区与性能区显示确定性空态；控制台无报错。
- 主题 QA：系统亮色 → 亮色；切换后写入 localStorage 并跨刷新保持；系统暗色且无手动选择 → 暗色；两种主题下均无横向溢出与控制台报错。
