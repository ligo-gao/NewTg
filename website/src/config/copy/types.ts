export type FeatureIconKey = "router" | "failover" | "plug" | "gauge";

export interface CopyLink {
  label: string;
  href: string;
}

export interface CopyStat {
  label: string;
  value: string;
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
  footer: {
    tagline: string;
    columns: Array<{
      title: string;
      links: CopyLink[];
    }>;
    status: CopyLink;
    copyright: string;
  };
}
