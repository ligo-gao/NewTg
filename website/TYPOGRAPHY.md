# 营销站排版（website/）

> 本文件是**入口指针**，不是第二套标准。唯一权威口径是
> [`docs/typography-00-排版标准.md`](../docs/typography-00-排版标准.md)，
> 机器校验是 `frontend/src/styles/typography-standard.test.ts`（含 website 扫描）
> 与 `frontend/src/styles/spacing-standard.test.ts`（间距刻度与例外登记）。
>
> 本文件只覆盖**字阶**。中文混排（§11）、间距与垂直节奏（§12）、例外登记（§13）
> 在权威文档里，营销站同样适用：间距用 Tailwind 的 4px 网格（`p-N` = N×4px），
> 刻度变量 `--space-*` 定义在 `frontend/src/styles/tokens.css`。

## 1. 与主站的同一套尺度

`website/src/app/globals.css` 的 `@theme` 里声明了与
`frontend/src/styles/tailwind.css` **逐值相同**的 `--text-public-*`：

| 层级 | 字号 / 行高 | 字重 | Token | 用途 |
|---|---|---|---|---|
| display | `clamp(39px, 4.6vw, 49px)` / 1.1 | 600 | `text-public-display` | 首页 hero h1，全站唯一 |
| hero | `clamp(31px, 3.2vw, 39px)` / 1.18 | 600 | `text-public-hero` | 预留：独立落地页主视觉（当前无页面使用） |
| title | `clamp(25px, 2.2vw, 31px)` / 1.28 | 600 | `text-public-title` | 所有内页 h1（定价 / 模型库 / 排行 / 关于…） |
| section | 25 / 34 | 600 | `text-public-section` | 所有区块 h2（含收尾 CTA） |
| subhead | 20 / 28 | 600 | `text-public-subhead` | 小节 h3、卡片组标题 |
| figure | 25 / 34 | 600 | `text-public-figure` | 信任带三数字等统计值 |
| lead | 20 / 30 | 400 | `text-public-lead` | 页面导语 |
| body-strong | 16 / 28 | 600 | `text-public-body-strong` | 卡片标题、表格主列 |
| body | 16 / 28 | 400 | `text-public-body` | 正文、按钮、输入框 |
| label | 14 / 22 | 500 | `text-public-label` | 表头、标签、控件 |
| caption | 14 / 22 | 400 | `text-public-caption` | 辅助说明、元信息、表格次列 |

等价组件类（`globals.css` 的 `@layer components`）：`.heading-page` = hero、
`.heading-section` = section、`.heading-sub` = subhead、`.body-primary` = body、
`.body-secondary` / `.body-small` = caption。

## 2. 字体

- 西文：`Inter`（`next/font/google`，`--font-inter`），字重只有 400 / 500 / 600。
- 中文：`Noto Sans SC` 随站加载，运行时优先系统苹方 / 微软雅黑（顺序见 `--font-sans`）。
- 等宽：`JetBrains Mono`，只用于代码块、模型 ID、参数名、端点路径。
- 2026-10-28 起 Archivo 已移除：它的骨架与中文黑体不搭，且引入 700 字重。

## 3. 硬规则（测试会拦）

1. 不写 `text-[13px]`、`text-[clamp(...)]` 一类任意字号。
2. 不用 Tailwind 默认阶 `text-xs/sm/base/lg/xl`——它们不在这套比例里。
3. 不写 `font-bold`（700）；强调一律 600。
4. 可见文字 ≥ 14px；唯一例外是 `aria-hidden` 的装饰元素。
5. 正文段落 ≤ 40 个汉字（`max-w-[40em]`），导语 ≤ 34 字（`max-w-[34em]`）。
6. 数字用无衬线 + `tabular-nums`，不要用等宽体排版价格与分数。

## 4. 检查清单

- [ ] 改字号前先改 `docs/typography-00-排版标准.md` 与两个 Tailwind 入口，再改测试。
- [ ] `bun run typecheck && bun run test` 在 `website/` 与 `frontend/` 都通过。
- [ ] `bun run build` 后按 1440 / 768 / 375 三档目视验收（字号、行长、溢出）。

## 5. 历史文档

`typography-audit.md` 与 `CHANGELOG-typography.md` 记录的是 2026-10 之前的旧系统
（Archivo + 7 档任意 clamp），仅作沿革参考，**不得据其新增样式**。
