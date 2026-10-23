import type { Copy } from "./types";

export const en: Copy = {
  brand: "NEWTG",
  skipLink: "Skip to main content",

  meta: {
    title: "NEWTG — One API for every major model",
    description:
      "Route GPT, Claude, Gemini, DeepSeek and other major models through one API, with smart routing, automatic failover and unified billing. Switch models with one line of code.",
    ogAlt: "NEWTG homepage: one API for every major model",
  },

  language: {
    label: "Language",
  },

  theme: {
    label: "Theme",
    toggle: "Toggle theme",
  },

  nav: {
    links: [
      { label: "Models", href: "#models" },
      { label: "Pricing", href: "/pricing" },
      { label: "Docs", href: "/docs" },
      { label: "Console", href: "/dashboard" },
    ],
    login: "Sign in",
    cta: "Get started free",
    openMenu: "Open navigation menu",
    closeMenu: "Close navigation menu",
  },

  hero: {
    titleLines: ["One API,", "every major model"],
    subtitle:
      "Route GPT, Claude, Gemini, DeepSeek and other major models through one endpoint. Smart routing, automatic failover and unified billing — switch models with one line of code, with transparent costs.",
    primaryCta: "Get started free",
    secondaryCta: "Read the docs",
    compatLabel: "Works with",
    compat: ["OpenAI SDK", "LangChain", "Vercel AI SDK"],
    method: "POST",
    endpoint: "api.newtg.ai/v1/chat/completions",
    copyLabel: "Copy",
    copiedLabel: "Copied",
    copyFailedLabel: "Copy failed",
    copySuccessAnnouncement: "Code copied to clipboard",
    copyFailureAnnouncement: "Copy failed; select the code manually",
    code: `curl https://api.newtg.ai/v1/chat/completions \\
  -H "Authorization: Bearer $NEWTG_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "gpt-4o",
    "messages": [
      { "role": "user", "content": "Hello, introduce yourself" }
    ]
  }'`,
    requestLabel: "Last request",
    okLabel: "200 OK",
    trace: [
      { label: "Route", value: "openai / gpt-4o" },
      { label: "First token", value: "412 ms" },
      { label: "Cost", value: "$0.0024" },
    ],
  },

  trust: {
    ariaLabel: "Platform scale",
    items: [
      { value: "100+", label: "models across 6 providers" },
      { value: "99.9%", label: "service availability" },
      { value: "10ms", label: "smart routing" },
      { value: "10,000+", label: "developers" },
    ],
    note: "Figures are subject to the production release.",
  },

  models: {
    title: "Every model you need",
    subtitle: "Compare prices and switch instantly — pricing and quotas stay visible",
    searchLabel: "Search models",
    searchPlaceholder: "Try GPT-4o / Claude / DeepSeek",
    vendorFilterLabel: "Filter by provider",
    allVendors: "All",
    countUnit: "models",
    compare: "Compare",
    call: "Use",
    callHref: "/dashboard",
    emptyTitle: "No matching models.",
    emptyAction: "Clear search and filters",
    emptyCatalog: "No public models have been configured yet.",
    loadError: "Model data is temporarily unavailable.",
    retry: "Retry",
    truncatedNote: "Showing the first 24 matches.",
    pricingLink: "View full pricing",
    footnote:
      "Models, context and prices come from the backend and sync live; prices are shown per 1M tokens.",
    badgeFree: "Free",
    badgePerRequest: "Per request",
    stats: {
      context: "Context",
      maxOutput: "Max output",
      input: "Input / 1M",
      output: "Output / 1M",
    },
    compareBar: {
      ariaLabel: "Model comparison",
      selected: "Selected",
      unit: "models",
      open: "Compare",
      clear: "Clear",
    },
    dialog: {
      title: "Model comparison",
      close: "Close comparison",
      indicator: "Metric",
      rows: {
        vendor: "Provider",
        context: "Context",
        maxOutput: "Max output",
        input: "Input / 1M",
        output: "Output / 1M",
      },
    },
  },

  features: {
    title: "Leave model plumbing to the gateway",
    subtitle: "Routing, failover and billing happen in the same request path.",
    items: [
      {
        icon: "router",
        title: "Smart model selection",
        description: "Picks the best model for the task and budget automatically",
      },
      {
        icon: "failover",
        title: "Automatic failover",
        description: "Switches to a backup the moment a model is unavailable",
      },
      {
        icon: "plug",
        title: "Drop-in integration",
        description: "OpenAI-compatible API — change one line of config to switch models",
      },
      {
        icon: "gauge",
        title: "Precise usage control",
        description: "Live usage, budget alerts and token-level billing",
      },
    ],
  },

  performance: {
    title: "Real call performance",
    subtitle: "Per-model first-token latency and output speed from the last 7 days of real calls",
    windowLabel: "Last 7 days",
    columns: {
      model: "Model",
      vendor: "Provider",
      ttft: "First-token latency",
      tps: "Output speed",
    },
    lowSample: "Low sample",
    unavailable: "Performance data is temporarily unavailable",
    retry: "Retry",
    empty: "No models to show yet.",
    footnote:
      "Averages from real calls over the last 7 days. Latency is time to first token (TTFT) for streaming calls; output speed is estimated from total output tokens over total latency. Models with fewer than 20 samples are marked low sample. Missing data is shown as —.",
  },

  closing: {
    title: "Ready to connect every model?",
    cta: "Get started free",
    note: "No credit card required. Test credits on sign-up.",
  },

  footer: {
    tagline:
      "One API for every major model. Unified access, smart routing, automatic failover and billing.",
    columns: [
      {
        title: "Product",
        links: [
          { label: "Models", href: "#models" },
          { label: "Pricing", href: "/pricing" },
          { label: "Console", href: "/dashboard" },
        ],
      },
      {
        title: "Docs",
        links: [
          { label: "Quick start", href: "/docs" },
          { label: "API reference", href: "/docs" },
          { label: "SDKs & examples", href: "/docs" },
          { label: "Changelog", href: "/docs" },
        ],
      },
      {
        title: "Community",
        links: [
          { label: "GitHub", href: "#community" },
          { label: "Discord", href: "#community" },
          { label: "X", href: "#community" },
          { label: "Contact", href: "#community" },
        ],
      },
      {
        title: "Legal",
        links: [
          { label: "Terms of service", href: "/legal/terms" },
          { label: "Privacy policy", href: "/legal/privacy" },
        ],
      },
    ],
    status: { label: "Status", href: "#status" },
    copyright: "© 2026 NEWTG",
  },
};
