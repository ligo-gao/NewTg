export interface PricingVendorFacet {
  value: string;
  label: string;
  count: number;
}

export interface PricingMeta {
  credit_symbol: string;
  quota_per_unit: number;
  usd_exchange_rate: number;
  version: string;
  generated_at: string;
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
}

export interface PricingCatalog {
  items: PricingModel[];
  total: string;
  page: number;
  page_size: number;
  facets: {
    vendors: PricingVendorFacet[];
  };
  meta: PricingMeta;
}

export interface ModelMetricsSummary {
  avg_latency_ms: number | null;
  avg_ttft_ms: number | null;
  success_rate: number | null;
  tps: number | null;
  failed_24h: number | null;
  sample_size: number;
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
