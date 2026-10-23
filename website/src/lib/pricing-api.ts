export interface PricingVendorFacet {
  value: string;
  label: string;
  count: number;
}

export interface PricingFacetOption {
  value: string;
  label: string;
  count: number;
}

export interface PricingGroupFacet {
  value: string;
  label: string;
  ratio: number | null;
  count: number;
}

export interface PricingFacets {
  vendors: PricingVendorFacet[];
  capabilities: PricingFacetOption[];
  context: PricingFacetOption[];
  groups: PricingGroupFacet[];
  endpoints: PricingFacetOption[];
  pricing_types: Array<{ value: string; count: number }>;
}

export interface PricingMeta {
  credit_symbol: string;
  quota_per_unit: number;
  usd_exchange_rate: number;
  version: string;
  generated_at: string;
}

export type PricingScheduleKind = "flat" | "peak_offpeak";
export type PricingHolidayPolicy = "none" | "cn_offpeak";

export interface PricingScheduleWindow {
  /** Admin-provided label; may be an empty string. */
  name: string;
  /** 1 = Monday … 7 = Sunday. For overnight windows this is the start day. */
  days: number[];
  /** HH:MM, left-closed right-open. `end <= start` means the window crosses midnight. */
  start: string;
  end: string;
  /** Decimal multiplier: 0.5 = half price. */
  multiplier: number;
}

/**
 * Time-of-day pricing published on the public catalog. `null` (or absent on
 * older payloads) means the model bills at its flat token price.
 */
export interface PricingSchedule {
  kind: PricingScheduleKind;
  /** IANA zone owned by the strategy; not the site timezone. */
  timezone: string;
  /** Multiplier used outside every window (and on statutory holidays for `cn_offpeak`). */
  default_multiplier: number;
  holiday_policy: PricingHolidayPolicy;
  /** When several windows match, the last entry wins. */
  windows: PricingScheduleWindow[];
}

export interface PricingGroupPrice {
  group: string;
  /** Integer percent: 120 = 1.20x. */
  ratio: number | null;
  prompt: string | null;
  completion: string | null;
  cache_read: string | null;
  cache_write: string | null;
  source: "override" | "ratio";
}

export interface PricingModel {
  model_id: string;
  display_name: string;
  vendor: string;
  context_length: number | null;
  max_output: number | null;
  prompt_price: string | null;
  completion_price: string | null;
  cache_read_price: string | null;
  cache_write_price: string | null;
  is_free: boolean;
  mode: "per_token" | "per_request";
  description: string | null;
  capabilities: string[] | null;
  tags: string[] | null;
  endpoints: string[] | null;
  group_price: PricingGroupPrice | null;
  /**
   * Peak/off-peak schedule; `null` (or absent on older payloads) when the
   * backend has not enabled time-of-day pricing for this model.
   */
  pricing_schedule?: PricingSchedule | null;
}

export interface PricingCatalog {
  items: PricingModel[];
  total: string;
  page: number;
  page_size: number;
  facets: PricingFacets;
  meta: PricingMeta;
}

export interface PricingQuery {
  page: number;
  pageSize: number;
  search: string;
  sort: "name" | "price_asc" | "price_desc";
  vendors: string[];
  capabilities: string[];
  contexts: string[];
  endpoints: string[];
  group: string;
  quotaType: "all" | "token" | "request";
}

export const DEFAULT_PRICING_QUERY: PricingQuery = {
  page: 1,
  pageSize: 24,
  search: "",
  sort: "name",
  vendors: [],
  capabilities: [],
  contexts: [],
  endpoints: [],
  group: "default",
  quotaType: "all",
};

export function pricingQueryString(query: PricingQuery): string {
  const params = new URLSearchParams();
  params.set("p", String(query.page));
  params.set("page_size", String(query.pageSize));
  if (query.search.trim() !== "") {
    params.set("search", query.search.trim());
  }
  params.set("sort", query.sort);
  query.vendors.forEach((value) => params.append("vendor", value));
  query.capabilities.forEach((value) => params.append("capability", value));
  query.contexts.forEach((value) => params.append("context", value));
  query.endpoints.forEach((value) => params.append("endpointType", value));
  if (query.group !== "") {
    params.set("group", query.group);
  }
  params.set("quotaType", query.quotaType);
  return params.toString();
}

export async function fetchPricingPage(
  query: PricingQuery,
  signal?: AbortSignal,
): Promise<PricingCatalog> {
  const response = await fetch(`/api/pricing?${pricingQueryString(query)}`, {
    signal,
    headers: { Accept: "application/json" },
    credentials: "omit",
  });
  if (!response.ok) {
    throw new Error(`pricing request failed: ${response.status}`);
  }
  return (await response.json()) as PricingCatalog;
}

export async function fetchPricingCatalog(signal?: AbortSignal): Promise<PricingCatalog> {
  const response = await fetch("/api/pricing?page_size=100&sort=name", {
    signal,
    headers: { Accept: "application/json" },
    credentials: "omit",
  });
  if (!response.ok) {
    throw new Error(`pricing catalog request failed: ${response.status}`);
  }
  return (await response.json()) as PricingCatalog;
}

export interface ModelMetricsSummary {
  avg_latency_ms: number | null;
  avg_ttft_ms: number | null;
  success_rate: number | null;
  tps: number | null;
  failed_24h: number | null;
  sample_size: number;
}

export interface ModelMetricsLatestTest {
  ttft_ms_p50: number | null;
  ttft_ms_p95: number | null;
  latency_ms_p50: number | null;
  latency_ms_p95: number | null;
  tps: number | null;
  success_rate: number;
  sample_size: number;
  tested_at: string;
}

export interface ModelMetrics {
  range: "24h" | "7d";
  window: {
    start: string;
    end: string;
  };
  summary: ModelMetricsSummary;
  per_group: Array<{
    group: string;
    avg_ttft_ms: number | null;
    avg_latency_ms: number | null;
    success_rate: number | null;
  }>;
  /**
   * Latest admin-initiated probe. Independent of the usage_logs corpus; null
   * when no probe exists or the perf store is unavailable (fail-open).
   */
  latest_test: ModelMetricsLatestTest | null;
}

export async function fetchModelMetrics(
  modelID: string,
  signal?: AbortSignal,
): Promise<ModelMetrics> {
  const response = await fetch(
    `/api/pricing/${encodeURIComponent(modelID)}/metrics?range=7d`,
    {
      signal,
      headers: { Accept: "application/json" },
      credentials: "omit",
    },
  );
  if (!response.ok) {
    throw new Error(`model metrics request failed: ${response.status}`);
  }
  return (await response.json()) as ModelMetrics;
}
