import type { Copy } from "./types";

export const zhTW: Copy = {
  brand: "NEWTG",
  skipLink: "跳至主要內容",

  meta: {
    title: "NEWTG — 一個 API，呼叫所有主流模型",
    description:
      "統一路由 GPT、Claude、Gemini、DeepSeek 等主流模型，支援智慧路由、自動降級與統一計費，一套程式碼隨時切換，成本透明可控。",
    ogAlt: "NEWTG 首頁：一個 API，呼叫所有主流模型",
  },

  language: {
    label: "語言",
  },

  theme: {
    label: "主題",
    toggle: "切換主題",
  },

  nav: {
    links: [
      { label: "模型庫", href: "/models" },
      { label: "定價", href: "/pricing" },
      { label: "文件", href: "/docs" },
      { label: "控制台", href: "/dashboard" },
    ],
    login: "登入",
    cta: "免費開始使用",
    openMenu: "開啟導覽選單",
    closeMenu: "關閉導覽選單",
  },

  hero: {
    titleLines: ["一個 API，", "呼叫所有主流模型"],
    subtitle:
      "統一路由 GPT、Claude、Gemini、DeepSeek 等主流模型，支援智慧路由、自動降級與統一計費，一套程式碼隨時切換，成本透明可控。",
    primaryCta: "免費開始使用",
    secondaryCta: "查看文件",
    compatLabel: "相容",
    compat: ["OpenAI SDK", "LangChain", "Vercel AI SDK"],
    method: "POST",
    endpoint: "api.newtg.ai/v1/chat/completions",
    copyLabel: "複製",
    copiedLabel: "已複製",
    copyFailedLabel: "複製失敗",
    copySuccessAnnouncement: "程式碼已複製到剪貼簿",
    copyFailureAnnouncement: "複製失敗，請手動選取程式碼",
    code: `curl https://api.newtg.ai/v1/chat/completions \\
  -H "Authorization: Bearer $NEWTG_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "gpt-4o",
    "messages": [
      { "role": "user", "content": "你好，介紹一下你自己" }
    ]
  }'`,
    requestLabel: "最近一次請求",
    okLabel: "200 OK",
    trace: [
      { label: "路由", value: "openai / gpt-4o" },
      { label: "首字延遲", value: "412 ms" },
      { label: "本次費用", value: "$0.0024" },
    ],
  },

  trust: {
    ariaLabel: "平台能力",
    items: [
      { value: "OpenAI", label: "相容主流 SDK" },
      { value: "統一計費", label: "按 token 透明計量" },
      { value: "智慧路由", label: "健康檢查與自動降級" },
      { value: "7 語言", label: "介面開箱即用" },
    ],
    note: "功能與資料以實際接入為準。",
  },

  models: {
    title: "支援你需要的每一個模型",
    subtitle: "即時比價、一鍵切換，價格與額度透明可查",
    searchLabel: "搜尋模型",
    searchPlaceholder: "試試搜尋：GPT-4o / Claude / DeepSeek",
    vendorFilterLabel: "依廠商篩選",
    allVendors: "全部",
    countUnit: "個模型",
    compare: "比較",
    call: "呼叫",
    callHref: "/dashboard",
    emptyTitle: "沒有符合的模型。",
    emptyAction: "清除搜尋與篩選",
    emptyCatalog: "後台還沒有可公開的模型。",
    loadError: "模型資料暫時無法載入。",
    retry: "重試",
    truncatedNote: "僅顯示前 24 個符合結果。",
    pricingLink: "查看完整定價",
    footnote: "模型、上下文與價格來自後台設定，即時同步；價格以每 1M tokens 顯示。",
    badgeFree: "免費",
    badgePerRequest: "按次",
    stats: {
      context: "上下文",
      maxOutput: "最大輸出",
      input: "輸入 / 1M",
      output: "輸出 / 1M",
    },
    compareBar: {
      ariaLabel: "模型比較",
      selected: "已選",
      unit: "個模型",
      open: "比較",
      clear: "清除",
    },
    dialog: {
      title: "模型比較",
      close: "關閉比較",
      indicator: "指標",
      rows: {
        vendor: "廠商",
        context: "上下文",
        maxOutput: "最大輸出",
        input: "輸入 / 1M",
        output: "輸出 / 1M",
      },
    },
  },

  features: {
    title: "把模型接入的麻煩，交給閘道",
    subtitle: "路由、容錯、計費都在同一條請求鏈路裡完成。",
    items: [
      {
        icon: "router",
        title: "聰明的模型選擇",
        description: "依任務類型與成本預算，自動選出最佳模型",
      },
      {
        icon: "failover",
        title: "故障自動降級",
        description: "模型不可用時秒切備援，你的業務不中斷",
      },
      {
        icon: "plug",
        title: "零改造接入",
        description: "OpenAI 相容介面，改一行設定即可切換模型",
      },
      {
        icon: "gauge",
        title: "用量精確可控",
        description: "即時用量監控、預算告警、按 token 計費",
      },
    ],
  },

  performance: {
    title: "真實呼叫效能",
    subtitle: "依據近 7 天真實呼叫，按模型統計首字延遲與輸出速度",
    windowLabel: "近 7 天",
    columns: {
      model: "模型",
      vendor: "供應商",
      ttft: "連線延遲",
      tps: "token 輸出速度",
    },
    lowSample: "樣本不足",
    probe: "主動測試",
    unavailable: "效能資料暫時無法使用",
    retry: "重試",
    empty: "暫無可顯示的模型。",
    footnote:
      "資料為近 7 天真實呼叫的平均值；連線延遲取串流呼叫的首字時間（TTFT），輸出速度為總輸出 token 與總耗時的估算值。樣本少於 20 次的模型標示「樣本不足」，無資料顯示 —。",
  },

  closing: {
    title: "準備好接入所有模型了嗎？",
    cta: "免費開始",
    note: "無需信用卡，註冊即可開始",
  },

  footer: {
    tagline: "一個 API，呼叫所有主流模型。統一接入、智慧路由、自動降級與統一計費。",
    columns: [
      {
        title: "產品",
        links: [
          { label: "模型庫", href: "/models" },
          { label: "定價", href: "/pricing" },
          { label: "控制台", href: "/dashboard" },
        ],
      },
      {
        title: "文件",
        links: [
          { label: "快速開始", href: "/docs" },
          { label: "API 參考", href: "/docs" },
          { label: "SDK 與範例", href: "/docs" },
          { label: "更新日誌", href: "/docs" },
        ],
      },
      {
        title: "社群",
        links: [
          { label: "GitHub", href: "#community" },
          { label: "Discord", href: "#community" },
          { label: "X", href: "#community" },
          { label: "聯絡我們", href: "#community" },
        ],
      },
      {
        title: "法律",
        links: [
          { label: "服務條款", href: "/legal/terms" },
          { label: "隱私政策", href: "/legal/privacy" },
        ],
      },
    ],
    status: { label: "狀態頁", href: "#status" },
    copyright: "© 2026 NEWTG",
  },
  pricingPage: {
    meta: {
      title: "模型定價 — NEWTG",
      description:
        "輸入、輸出、快取與按次呼叫的完整價目，支援分組價、幣別與單位切換，價格來自後台設定並即時同步。",
      ogAlt: "NEWTG 模型定價頁",
    },
    title: "模型定價",
    subtitle:
      "輸入、輸出、快取與按次呼叫的完整價目。價格來自後台設定，依目前分組即時換算；展開任一列即可看到完整價籤與費用估算。",
    stats: {
      models: "個模型",
      vendors: "家廠商",
      lowest: "最低付費輸入 / 1M",
      sync: "價格同步週期",
    },
    legend: [
      "價格為每 1M tokens，可切換 1K 檢視",
      "100 點數 = ¥1 · 美元參考依匯率換算",
      "null 顯示 —，0 顯示「免費」",
      "分組價 = 基準價 × 分組倍率",
    ],
    legendSummary: "計價說明",
    filters: {
      title: "篩選",
      reset: "重設",
      close: "收合",
      vendors: "廠商",
      capabilities: "能力",
      context: "上下文",
      endpoint: "端點",
      mode: "計費方式",
      all: "全部",
      perToken: "按 token",
      perRequest: "按次",
    },
    toolbar: {
      searchPlaceholder: "搜尋模型名稱、ID 或廠商，例如 GPT-4o / Claude",
      sortName: "依名稱",
      sortPriceAsc: "輸入價由低到高",
      sortPriceDesc: "輸入價由高到低",
      groupPrefix: "分組：",
      unitM: "/ 1M",
      unitK: "/ 1K",
      dual: "顯示 $ 參考",
      table: "表格",
      cards: "卡片",
      filters: "篩選",
    },
    table: {
      model: "模型",
      vendor: "廠商",
      context: "上下文",
      input: "輸入",
      output: "輸出",
      cacheRead: "快取讀取",
      cacheWrite: "快取寫入",
      perRequest: "按次",
      group: "分組價",
      actions: "比較 / 詳情",
      compare: "比較",
      expand: "展開完整價目",
      perCall: "/ 次",
      free: "免費",
      dash: "—",
      groupRatio: "×{ratio}",
    },
    cards: {
      detail: "完整價目",
      context: "上下文",
      maxOutput: "最大輸出",
    },
    detail: {
      register: "完整價籤",
      base: "基準價",
      group: "分組價",
      input: "輸入",
      output: "輸出",
      cacheRead: "快取讀取",
      cacheWrite: "快取寫入",
      perRequest: "按次",
      rechargeNote:
        "儲值參考：¥1 = 100 點數。快取讀取/寫入未設定時顯示 —；「免費」僅在有效價格全為 0 時顯示。",
      estimator: "費用估算",
      inputTokens: "輸入 tokens",
      outputTokens: "輸出 tokens",
      cacheHit: "快取命中率 %",
      requests: "請求次數",
      estimated: "預計費用",
      inputCost: "輸入（未命中）",
      cacheCost: "快取讀取",
      outputCost: "輸出",
      perRequestCost: "按次單價",
      estimatorNote:
        "估算使用目前分組價；快取讀取未設定時依輸入價計算。",
      info: "模型資訊",
      mode: "計費方式",
      copyId: "複製模型 ID",
      copied: "已複製",
      viewDetail: "查看完整詳情",
    },
    schedule: {
      badge: "峰谷計價",
      title: "時段計價",
      timezone: "時區",
      defaultMultiplier: "非高峰時段",
      windowFallback: "高峰時段",
      multiplier: "×{value}",
      holidayLabel: "節假日",
      holidayNone: "不區分節假日",
      holidayCnOffpeak: "中國法定節假日按低谷計費",
      everyDay: "每天",
      weekdays: ["週一", "週二", "週三", "週四", "週五", "週六", "週日"],
      overnight: "次日結束",
      footnote: "時間依 {timezone} 計算；請求依開始時的價格計費。",
    },
    compare: {
      title: "模型比較",
      selected: "已選",
      unit: "個模型",
      max: "最多 {count} 個",
      open: "比較",
      clear: "清除",
      close: "關閉比較",
      indicator: "指標",
      context: "上下文",
      maxOutput: "最大輸出",
      mode: "計費方式",
      input: "輸入",
      output: "輸出",
      cacheRead: "快取讀取",
      cacheWrite: "快取寫入",
      perRequest: "按次",
      group: "分組價",
    },
    states: {
      loading: "正在載入價目…",
      error: "價目暫時無法載入。",
      retry: "重試",
      empty: "沒有符合的模型。",
      emptyAction: "清除篩選",
      results: "共 {count} 個模型",
      perPage: "每頁",
      previous: "上一頁",
      next: "下一頁",
    },
    modeToken: "按 token",
    modeRequest: "按次",
    footnote:
      "價格每 60 秒同步；null 顯示 —；「免費」僅在有效價格全為 0 時顯示。",
  },
};
