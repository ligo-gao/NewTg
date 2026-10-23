import type { Copy } from "./types";

export const zhCN: Copy = {
  brand: "NEWTG",
  skipLink: "跳到主要内容",

  meta: {
    title: "NEWTG — 一个 API，调用所有主流模型",
    description:
      "统一路由 GPT、Claude、Gemini、DeepSeek 等主流模型，支持智能路由、自动降级与统一计费，一套代码随时切换，成本透明可控。",
    ogAlt: "NEWTG 首页：一个 API，调用所有主流模型",
  },

  language: {
    label: "语言",
  },

  theme: {
    label: "主题",
    toggle: "切换主题",
  },

  nav: {
    links: [
      { label: "模型库", href: "#models" },
      { label: "定价", href: "/pricing" },
      { label: "文档", href: "/docs" },
      { label: "控制台", href: "/dashboard" },
    ],
    login: "登录",
    cta: "免费开始使用",
    openMenu: "打开导航菜单",
    closeMenu: "关闭导航菜单",
  },

  hero: {
    titleLines: ["一个 API，", "调用所有主流模型"],
    subtitle:
      "统一路由 GPT、Claude、Gemini、DeepSeek 等主流模型，支持智能路由、自动降级与统一计费，一套代码随时切换，成本透明可控。",
    primaryCta: "免费开始使用",
    secondaryCta: "查看文档",
    compatLabel: "兼容",
    compat: ["OpenAI SDK", "LangChain", "Vercel AI SDK"],
    method: "POST",
    endpoint: "api.newtg.ai/v1/chat/completions",
    copyLabel: "复制",
    copiedLabel: "已复制",
    copyFailedLabel: "复制失败",
    copySuccessAnnouncement: "代码已复制到剪贴板",
    copyFailureAnnouncement: "复制失败，请手动选择代码",
    code: `curl https://api.newtg.ai/v1/chat/completions \\
  -H "Authorization: Bearer $NEWTG_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "gpt-4o",
    "messages": [
      { "role": "user", "content": "你好，介绍一下你自己" }
    ]
  }'`,
    requestLabel: "最近一次请求",
    okLabel: "200 OK",
    trace: [
      { label: "路由", value: "openai / gpt-4o" },
      { label: "首字延迟", value: "412 ms" },
      { label: "本次费用", value: "$0.0024" },
    ],
  },

  trust: {
    ariaLabel: "平台规模",
    items: [
      { value: "100+", label: "模型，覆盖 6 大厂商" },
      { value: "99.9%", label: "服务可用性" },
      { value: "10ms", label: "智能路由" },
      { value: "10,000+", label: "开发者" },
    ],
    note: "统计口径与数值以上线版本为准。",
  },

  models: {
    title: "支持你需要的每一个模型",
    subtitle: "实时比价、一键切换，价格与额度透明可查",
    searchLabel: "搜索模型",
    searchPlaceholder: "试试搜索：GPT-4o / Claude / DeepSeek",
    vendorFilterLabel: "按厂商筛选",
    allVendors: "全部",
    countUnit: "个模型",
    compare: "对比",
    call: "调用",
    callHref: "/dashboard",
    emptyTitle: "没有匹配的模型。",
    emptyAction: "清除搜索与筛选",
    emptyCatalog: "后台还没有可公示的模型。",
    loadError: "模型数据暂时无法加载。",
    retry: "重试",
    truncatedNote: "仅显示前 24 个匹配结果。",
    pricingLink: "查看完整定价",
    footnote: "模型、上下文与价格来自后台配置，实时同步；价格按每 1M tokens 展示。",
    badgeFree: "免费",
    badgePerRequest: "按次",
    stats: {
      context: "上下文",
      maxOutput: "最大输出",
      input: "输入 / 1M",
      output: "输出 / 1M",
    },
    compareBar: {
      ariaLabel: "模型对比",
      selected: "已选",
      unit: "个模型",
      open: "对比",
      clear: "清除",
    },
    dialog: {
      title: "模型对比",
      close: "关闭对比",
      indicator: "指标",
      rows: {
        vendor: "厂商",
        context: "上下文",
        maxOutput: "最大输出",
        input: "输入 / 1M",
        output: "输出 / 1M",
      },
    },
  },

  features: {
    title: "把模型接入的麻烦，交给网关",
    subtitle: "路由、容灾、计费都在同一条请求链路里完成。",
    items: [
      {
        icon: "router",
        title: "聪明的模型选择",
        description: "根据任务类型与成本预算，自动选最优模型",
      },
      {
        icon: "failover",
        title: "故障自动降级",
        description: "模型不可用时秒切备用，你的业务不中断",
      },
      {
        icon: "plug",
        title: "零改造接入",
        description: "OpenAI 兼容接口，改一行配置即切换模型",
      },
      {
        icon: "gauge",
        title: "用量精确可控",
        description: "实时用量监控、预算告警、按 token 计费",
      },
    ],
  },

  performance: {
    title: "真实调用性能",
    subtitle: "基于近 7 天真实调用，按模型统计首字延迟与输出速度",
    windowLabel: "近 7 天",
    columns: {
      model: "模型",
      vendor: "提供商",
      ttft: "连接时延",
      tps: "token 输出速度",
    },
    lowSample: "样本不足",
    unavailable: "性能数据暂不可用",
    retry: "重试",
    empty: "暂无可展示的模型。",
    footnote:
      "数据为近 7 天真实调用的平均值；连接时延取流式调用的首字时间（TTFT），输出速度为总输出 token 与总耗时的估算值。样本少于 20 次的模型标注「样本不足」，无数据显示 —。",
  },

  closing: {
    title: "准备好接入所有模型了吗？",
    cta: "免费开始",
    note: "无需信用卡，注册即送测试额度",
  },

  footer: {
    tagline: "一个 API，调用所有主流模型。统一接入、智能路由、自动降级与统一计费。",
    columns: [
      {
        title: "产品",
        links: [
          { label: "模型库", href: "#models" },
          { label: "定价", href: "/pricing" },
          { label: "控制台", href: "/dashboard" },
        ],
      },
      {
        title: "文档",
        links: [
          { label: "快速开始", href: "/docs" },
          { label: "API 参考", href: "/docs" },
          { label: "SDK 与示例", href: "/docs" },
          { label: "更新日志", href: "/docs" },
        ],
      },
      {
        title: "社区",
        links: [
          { label: "GitHub", href: "#community" },
          { label: "Discord", href: "#community" },
          { label: "X", href: "#community" },
          { label: "联系我们", href: "#community" },
        ],
      },
      {
        title: "法律",
        links: [
          { label: "服务条款", href: "/legal/terms" },
          { label: "隐私政策", href: "/legal/privacy" },
        ],
      },
    ],
    status: { label: "状态页", href: "#status" },
    copyright: "© 2026 NEWTG",
  },
};
