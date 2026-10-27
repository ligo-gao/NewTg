export type FeatureIconKey = "router" | "failover" | "plug" | "gauge";

export interface CopyLink {
  label: string;
  href: string;
}

export interface CopyStat {
  label: string;
  value: string;
}


export interface PricingPageCopy {
  meta: { title: string; description: string; ogAlt: string };
  title: string;
  subtitle: string;
  stats: { models: string; vendors: string; lowest: string; sync: string };
  legend: string[];
  /** Disclosure label that wraps the secondary pricing rules. */
  legendSummary: string;
  filters: {
    title: string;
    reset: string;
    close: string;
    vendors: string;
    capabilities: string;
    context: string;
    endpoint: string;
    mode: string;
    all: string;
    perToken: string;
    perRequest: string;
  };
  toolbar: {
    searchPlaceholder: string;
    sortName: string;
    sortPriceAsc: string;
    sortPriceDesc: string;
    groupPrefix: string;
    unitM: string;
    unitK: string;
    dual: string;
    table: string;
    cards: string;
    filters: string;
  };
  table: {
    model: string;
    vendor: string;
    context: string;
    input: string;
    output: string;
    cacheRead: string;
    cacheWrite: string;
    perRequest: string;
    group: string;
    actions: string;
    compare: string;
    expand: string;
    perCall: string;
    free: string;
    dash: string;
    groupRatio: string;
  };
  cards: {
    detail: string;
    context: string;
    maxOutput: string;
  };
  detail: {
    register: string;
    base: string;
    group: string;
    input: string;
    output: string;
    cacheRead: string;
    cacheWrite: string;
    perRequest: string;
    rechargeNote: string;
    estimator: string;
    inputTokens: string;
    outputTokens: string;
    cacheHit: string;
    requests: string;
    estimated: string;
    inputCost: string;
    cacheCost: string;
    outputCost: string;
    perRequestCost: string;
    estimatorNote: string;
    info: string;
    mode: string;
    copyId: string;
    copied: string;
    viewDetail: string;
  };
  compare: {
    title: string;
    selected: string;
    unit: string;
    max: string;
    open: string;
    clear: string;
    close: string;
    indicator: string;
    context: string;
    maxOutput: string;
    mode: string;
    input: string;
    output: string;
    cacheRead: string;
    cacheWrite: string;
    perRequest: string;
    group: string;
  };
  /** Peak/off-peak (time-of-day) pricing labels. */
  schedule: {
    badge: string;
    title: string;
    timezone: string;
    defaultMultiplier: string;
    windowFallback: string;
    /** Template with `{value}`, e.g. "×{value}". */
    multiplier: string;
    holidayLabel: string;
    holidayNone: string;
    holidayCnOffpeak: string;
    everyDay: string;
    /** Monday first; index 0 = Monday. */
    weekdays: [string, string, string, string, string, string, string];
    /** Appended to windows whose `end` is before or equal to `start`. */
    overnight: string;
    /** Template with `{timezone}`. */
    footnote: string;
  };
  states: {
    loading: string;
    error: string;
    retry: string;
    empty: string;
    emptyAction: string;
    results: string;
    perPage: string;
    previous: string;
    next: string;
  };
  modeToken: string;
  modeRequest: string;
  footnote: string;
}

export interface Copy {
  brand: string;
  skipLink: string;
  meta: {
    title: string;
    description: string;
    ogAlt: string;
  };
  language: {
    label: string;
  };
  theme: {
    label: string;
    toggle: string;
  };
  nav: {
    links: CopyLink[];
    login: string;
    cta: string;
    openMenu: string;
    closeMenu: string;
    /** Accessible name for the primary and mobile navigation landmarks. */
    ariaLabel: string;
  };
  hero: {
    titleLines: string[];
    subtitle: string;
    primaryCta: string;
    secondaryCta: string;
    compatLabel: string;
    compat: string[];
    method: string;
    endpoint: string;
    copyLabel: string;
    copiedLabel: string;
    copyFailedLabel: string;
    copySuccessAnnouncement: string;
    copyFailureAnnouncement: string;
    code: string;
    requestLabel: string;
    okLabel: string;
    trace: CopyStat[];
  };
  trust: {
    ariaLabel: string;
    items: CopyStat[];
    note: string;
  };
  models: {
    title: string;
    subtitle: string;
    searchLabel: string;
    searchPlaceholder: string;
    vendorFilterLabel: string;
    allVendors: string;
    countUnit: string;
    compare: string;
    call: string;
    callHref: string;
    emptyTitle: string;
    emptyAction: string;
    emptyCatalog: string;
    loadError: string;
    retry: string;
    truncatedNote: string;
    pricingLink: string;
    footnote: string;
    badgeFree: string;
    badgePerRequest: string;
    stats: {
      context: string;
      maxOutput: string;
      input: string;
      output: string;
    };
    compareBar: {
      ariaLabel: string;
      selected: string;
      unit: string;
      open: string;
      clear: string;
    };
    dialog: {
      title: string;
      close: string;
      indicator: string;
      rows: {
        vendor: string;
        context: string;
        maxOutput: string;
        input: string;
        output: string;
      };
    };
  };
  features: {
    title: string;
    subtitle: string;
    items: Array<{
      icon: FeatureIconKey;
      title: string;
      description: string;
    }>;
  };
  performance: {
    title: string;
    subtitle: string;
    windowLabel: string;
    columns: {
      model: string;
      vendor: string;
      ttft: string;
      tps: string;
    };
    lowSample: string;
    probe: string;
    unavailable: string;
    retry: string;
    empty: string;
    footnote: string;
  };
  closing: {
    title: string;
    cta: string;
    note: string;
  };
  pricingPage: PricingPageCopy;
  footer: {
    tagline: string;
    /** Accessible name for the footer logo link back to the home page. */
    homeAria: string;
    columns: Array<{
      title: string;
      links: CopyLink[];
    }>;
    status: CopyLink;
    copyright: string;
  };
}
