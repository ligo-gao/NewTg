# 排版系统文档

本文档定义了 NEWTG 网站的统一排版系统，适用于所有公共页面（首页、模型库、定价、关于等）。

## 设计原则

- **一致性（Consistency）**：所有页面使用统一的标题层级和字号体系
- **协调性（Harmony）**：标题、正文、小字的比例关系经过精心设计
- **响应式（Responsive）**：使用 clamp() 实现流畅的响应式字号
- **易读性（Readability）**：合理的行高和字间距确保舒适的阅读体验
- **大气感（Elegance）**：字号比例和留白营造出专业、大气的视觉效果

## 标题层级

### 页面主标题（Page Hero Title）
用于页面顶部的主标题，例如定价页、关于页的顶部标题。

```tsx
<h1 className="text-[clamp(2.1rem,3.8vw,3.2rem)] font-bold leading-[1.12] tracking-[-0.035em]">
  {title}
</h1>
```

**或使用工具类：**
```tsx
<h1 className="heading-page">{title}</h1>
```

- **字号范围**：33.6px (mobile) → 51.2px (desktop)
- **字重**：700 (bold)
- **行高**：1.12
- **字间距**：-0.035em

**适用场景：**
- 定价页面主标题
- 关于页面主标题
- 独立的模型库页面主标题
- Closing CTA 标题

---

### 区块标题（Section Heading）
用于首页各个区块的标题。

```tsx
<h2 className="text-[clamp(1.75rem,3vw,2.4rem)] font-semibold leading-tight tracking-[-0.03em]">
  {title}
</h2>
```

**或使用工具类：**
```tsx
<h2 className="heading-section">{title}</h2>
```

- **字号范围**：28px (mobile) → 38.4px (desktop)
- **字重**：600 (semibold)
- **行高**：1.2 (tight)
- **字间距**：-0.03em

**适用场景：**
- 首页模型库区块标题
- 首页性能区块标题
- 首页特性网格标题
- 各种页面内的主要区块标题

---

### 次级标题（Sub-heading）
用于卡片组、表格区域等次级内容的标题。

```tsx
<h3 className="text-[1.375rem] font-semibold leading-[1.3] tracking-[-0.02em]">
  {title}
</h3>
```

**或使用工具类：**
```tsx
<h3 className="heading-sub">{title}</h3>
```

- **字号**：22px (固定)
- **字重**：600 (semibold)
- **行高**：1.3
- **字间距**：-0.02em

**适用场景：**
- 卡片组标题
- 表格区域标题
- 对话框标题

---

## 正文层级

### 主正文（Primary Body）
用于页面副标题和主要段落文本。

```tsx
<p className="text-[15px] leading-[1.8] text-ink-2">
  {content}
</p>
```

**或使用工具类：**
```tsx
<p className="body-primary">{content}</p>
```

- **字号**：15px
- **行高**：1.8
- **颜色**：`text-ink-2` (次要墨色)

**适用场景：**
- 页面副标题
- 主要段落文本
- 按钮文本

---

### 次正文（Secondary Body）
用于卡片描述和辅助说明。

```tsx
<p className="text-[13.5px] leading-[1.7] text-muted">
  {content}
</p>
```

**或使用工具类：**
```tsx
<p className="body-secondary">{content}</p>
```

- **字号**：13.5px
- **行高**：1.7
- **颜色**：`text-muted` (柔和色)

**适用场景：**
- 卡片描述
- 辅助说明文本
- 表单帮助文本

---

### 小文本（Small Text）
用于脚注、图例、元数据等。

```tsx
<p className="text-[12px] leading-[1.6] text-faint">
  {content}
</p>
```

**或使用工具类：**
```tsx
<p className="body-small">{content}</p>
```

- **字号**：12px
- **行高**：1.6
- **颜色**：`text-faint` (浅灰色)

**适用场景：**
- 页面脚注
- 图例说明
- 时间戳、元数据
- 版权信息

---

## 字体家族

### 无衬线字体（Sans-serif）
用于所有标题和正文。

```css
font-family: var(--font-sans);
/* Archivo, Noto Sans SC, PingFang SC, Microsoft YaHei, system-ui, sans-serif */
```

- **西文**：Archivo (现代、几何、友好)
- **中文**：Noto Sans SC (Google 开源，优雅清晰)
- **回退**：PingFang SC, Microsoft YaHei, system-ui

---

### 等宽字体（Monospace）
用于代码、数据、模型 ID 等。

```css
font-family: var(--font-mono);
/* JetBrains Mono, ui-monospace, SFMono-Regular, Menlo, Consolas, monospace */
```

- **主字体**：JetBrains Mono (开发者友好)
- **回退**：系统等宽字体

---

## 使用示例

### 定价页面

```tsx
<section>
  <h1 className="heading-page">
    {copy.pricingPage.title}
  </h1>
  <p className="body-primary mt-3.5 max-w-[46em]">
    {copy.pricingPage.subtitle}
  </p>
</section>
```

### 首页模型库区块

```tsx
<section>
  <h2 className="heading-section">
    {copy.models.title}
  </h2>
  <p className="body-primary mt-3 max-w-[34em]">
    {copy.models.subtitle}
  </p>
</section>
```

### 卡片组件

```tsx
<div className="card">
  <h3 className="heading-sub">
    {card.title}
  </h3>
  <p className="body-secondary mt-2">
    {card.description}
  </p>
  <p className="body-small mt-4">
    {card.metadata}
  </p>
</div>
```

---

## 实施的更改

### 已统一的组件

- ✅ `pricing-workbench.tsx` - 定价页主标题和副标题
- ✅ `model-explorer.tsx` - 模型库区块标题和副标题
- ✅ `performance-section.tsx` - 性能区块标题
- ✅ `feature-grid.tsx` - 特性网格标题
- ✅ `closing-cta.tsx` - 结尾 CTA 标题

### CSS 工具类

在 `globals.css` 中添加了以下工具类：

- `.heading-page` - 页面主标题
- `.heading-section` - 区块标题
- `.heading-sub` - 次级标题
- `.body-primary` - 主正文
- `.body-secondary` - 次正文
- `.body-small` - 小文本

---

## 迁移指南

如果要为新页面或组件应用此排版系统：

1. **确定内容层级**：页面主标题 > 区块标题 > 次级标题
2. **选择合适的类**：使用 `.heading-*` 或直接使用 Tailwind 类
3. **保持一致性**：同级标题使用相同的样式
4. **验证响应式**：在移动端和桌面端都要检查视觉效果
5. **测试可访问性**：确保标题层级语义正确（h1 > h2 > h3）

---

## 注意事项

### 响应式断点

- **移动端**：< 621px
- **平板**：621px - 1080px
- **桌面**：> 1080px
- **宽屏**：> 1280px
- **超宽屏**：> 1360px

### 不要做

❌ 在同一页面混用不同的标题字号体系  
❌ 随意调整 clamp() 值破坏一致性  
❌ 在标题上使用 `text-balance` 之外的文本工具  
❌ 混用 `text-muted` 和 `text-ink-2` 作为正文颜色

### 应该做

✅ 优先使用工具类 `.heading-*` 和 `.body-*`  
✅ 标题使用语义化的 HTML 标签（h1, h2, h3）  
✅ 为长文本设置合理的 `max-w-*` 限制行长  
✅ 在标题和正文之间保持合适的间距（mt-3, mt-3.5）

---

## 相关文件

- `src/app/globals.css` - 排版工具类定义
- `src/components/landing/*.tsx` - 应用排版系统的组件
- `typography-audit.md` - 排版审计和变更记录
