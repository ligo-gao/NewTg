# 排版系统统一方案

## 当前问题

### 标题不一致
- Hero 一级标题: `clamp(2.5rem,4.6vw,4rem)` (40-64px)
- 定价页一级标题: `clamp(2.1rem,3.6vw,3rem)` (33.6-48px)
- 模型库二级标题: `clamp(1.7rem,3vw,2.4rem)` (27.2-38.4px)
- 性能区二级标题: `clamp(1.7rem,3vw,2.4rem)` (27.2-38.4px)
- Closing CTA 标题: `clamp(2rem,4.2vw,3.3rem)` (32-52.8px)

### 正文不一致
- Hero 副标题: `text-base` (15px base)
- 模型库副标题: `text-[14.5px]`
- 定价页副标题: `text-[15px]`
- 各种小文本: `text-[13px]`, `text-[12.5px]`, `text-[11.5px]`

## 统一方案

### 页面标题层级（Page Title Hierarchy）
针对**模型库、定价、关于**三个公共页面使用统一的标题系统：

- **页面主标题（Page Hero）**: `clamp(2.1rem,3.8vw,3.2rem)` → 33.6-51.2px
  - font-bold, leading-[1.12], tracking-[-0.035em]
  - 用于：定价页顶部、模型库如单独成页、关于页顶部
  
- **区块标题（Section Heading）**: `clamp(1.75rem,3vw,2.4rem)` → 28-38.4px
  - font-semibold, leading-tight, tracking-[-0.03em]
  - 用于：首页中的模型库区块、性能区块、特性网格
  
- **次级标题（Sub-heading）**: `text-[1.375rem]` → 22px
  - font-semibold, leading-[1.3], tracking-[-0.02em]
  - 用于：卡片组标题、表格区域标题

### 正文层级（Body Text Hierarchy）

- **主正文（Primary Body）**: `text-[15px]` → 15px
  - leading-[1.8], text-ink-2
  - 用于：页面副标题、主要段落
  
- **次正文（Secondary Body）**: `text-[13.5px]` → 13.5px
  - leading-[1.7], text-muted
  - 用于：卡片描述、辅助说明
  
- **小文本（Small Text）**: `text-[12px]` → 12px
  - leading-[1.6], text-faint
  - 用于：脚注、图例、元数据

### 字体使用
- **标题**: font-sans (Archivo + Noto Sans SC)
- **正文**: font-sans
- **代码/数据**: font-mono (JetBrains Mono)

## 实施清单

- [ ] 统一定价工作台主标题为 `clamp(2.1rem,3.8vw,3.2rem)`
- [ ] 统一模型库区块标题为 `clamp(1.75rem,3vw,2.4rem)`
- [ ] 统一所有页面副标题为 `text-[15px]`
- [ ] 修复语言切换器以支持所有页面路由
- [ ] 更新全局样式文档
