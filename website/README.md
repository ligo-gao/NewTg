# NEWTG 官网

NEWTG 的营销首页，Next.js 16（App Router）+ React 19 + TypeScript（strict）+ Tailwind CSS 4，代码示例由 shiki 在服务端高亮。视觉与交互以 `../demos/landing-page/index.html` 为已确认的参考稿。

## 运行

```bash
cd website
bun install
bun run dev --port 3100      # 3000 端口可能被网关占用
```

- 开发：http://localhost:3100
- 构建：`bun run build`
- 生产：`PORT=3100 bun run start`
- 测试：`bun run test`（Vitest + Testing Library）
- 类型检查：`bun run typecheck`

## 结构

```
src/
  app/
    layout.tsx          # 字体、metadata、OG
    page.tsx            # 按区块组装
    globals.css         # 设计 token、按钮、shiki 覆盖、动效
  components/landing/
    site-header.tsx     # 固定毛玻璃导航（客户端）
    hero.tsx            # 首屏（服务端，shiki）
    code-card.tsx       # curl 卡片 + 复制（客户端）
    trust-band.tsx
    model-explorer.tsx  # 搜索 / 筛选 / 对比（客户端）
    model-card.tsx
    feature-grid.tsx
    pricing-section.tsx
    closing-cta.tsx
    site-footer.tsx
  config/
    copy.ts             # 全站文案（唯一来源）
    models.ts           # 6 个模型 + 6 家厂商
    site.ts             # 域名与链接
  lib/
    filter-models.ts    # 纯筛选函数
    highlight.ts        # shiki
```

## 数据与占位

- 模型：每个厂商各 1 个，共 6 个；搜索、筛选、对比均基于 `src/config/models.ts`。
- 价格与延迟为演示数据，页面已注明上线后由后台同步。
- 信任数字（100+ / 99.9% / 10ms / 10,000+）来自需求文案，页面注明「以上线版本为准」；上线前请替换为真实统计。
- `#console`、`#docs`、`#community`、`#status`、`#terms`、`#privacy` 为页内占位链接。
- `https://newtg.ai`、`https://api.newtg.ai` 为占位域名，需替换为实际域名。
- `public/og.png` 由参考稿渲染生成（1200×630）。

## 验证

- `bun run test`：4 个测试文件、12 个用例通过（筛选纯函数、导航开关、复制成功/失败、模型搜索/筛选/空状态/对比弹窗）。
- `bun run build`：通过（Next 16.4.2，静态预渲染）。
- 浏览器 QA：1920 / 1440 / 390 / 375 宽度；375 无横向溢出；桌面导航距左 40px；移动端汉堡菜单、对比栏、弹窗均正常；控制台无报错。
