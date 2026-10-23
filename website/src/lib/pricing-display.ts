import type { PricingMeta, PricingModel } from "@/lib/pricing-api";

export type TokenUnit = "M" | "K";

export interface EffectivePrices {
  input: string | null;
  output: string | null;
  cacheRead: string | null;
  cacheWrite: string | null;
  /** Integer percentage when a non-default group is active; null otherwise. */
  ratio: number | null;
}

/** Use the server-computed group price when available; never multiply client-side. */
export function effectivePrices(model: PricingModel, group: string): EffectivePrices {
  if (group !== "default" && model.group_price) {
    return {
      input: model.group_price.prompt,
      output: model.group_price.completion,
      cacheRead: model.group_price.cache_read,
      cacheWrite: model.group_price.cache_write,
      ratio: model.group_price.ratio,
    };
  }
  return {
    input: model.prompt_price,
    output: model.completion_price,
    cacheRead: model.cache_read_price,
    cacheWrite: model.cache_write_price,
    ratio: null,
  };
}

export function creditsValue(credits: string | null): number | null {
  if (credits === null) return null;
  const value = Number(credits);
  return Number.isFinite(value) ? value : null;
}

/** Credits → display currency at 100 credits per unit (from meta). */
export function currencyValue(credits: string | null, meta: PricingMeta | null): number | null {
  const value = creditsValue(credits);
  if (value === null || meta === null || meta.quota_per_unit <= 0) return null;
  return value / meta.quota_per_unit;
}

function decimals(value: number, precise: boolean): string {
  if (value === 0) return "0.00";
  return precise || value < 0.01 ? value.toFixed(4) : value.toFixed(2);
}

/** CNY per 1M tokens (or per call) → display string for the chosen token unit. */
export function formatCredits(
  credits: string | null,
  meta: PricingMeta | null,
  unit: TokenUnit,
): string {
  const value = currencyValue(credits, meta);
  if (value === null) return "—";
  const shown = unit === "K" ? value / 1000 : value;
  const symbol = meta?.credit_symbol ?? "¥";
  return `${symbol}${decimals(shown, unit === "K")}`;
}

export function formatUSD(
  credits: string | null,
  meta: PricingMeta | null,
  unit: TokenUnit,
): string {
  const value = currencyValue(credits, meta);
  if (value === null || meta === null || meta.usd_exchange_rate <= 0) return "";
  const shown = unit === "K" ? value / 1000 : value;
  const usdValue = shown / meta.usd_exchange_rate;
  return `$${decimals(usdValue, true)}`;
}

export function formatPerRequest(credits: string | null, meta: PricingMeta | null): string {
  const value = currencyValue(credits, meta);
  if (value === null) return "—";
  const symbol = meta?.credit_symbol ?? "¥";
  return `${symbol}${decimals(value, false)}`;
}

export function formatContext(tokens: number | null): string {
  if (tokens === null || tokens <= 0) return "—";
  if (tokens >= 1_000_000) {
    const millions = tokens / 1_000_000;
    return `${Number.isInteger(millions) ? millions : millions.toFixed(1)}M`;
  }
  if (tokens >= 1_000) {
    const thousands = tokens / 1_000;
    return `${Number.isInteger(thousands) ? thousands : thousands.toFixed(1)}K`;
  }
  return String(tokens);
}

export function formatRatio(ratio: number | null): string {
  if (ratio === null || ratio <= 0) return "";
  return `×${(ratio / 100).toFixed(2)}`;
}

export interface CostEstimate {
  total: number;
  inputCost: number;
  cacheCost: number;
  outputCost: number;
  unit: "token" | "request";
}

export interface EstimateInput {
  group: string;
  unit: TokenUnit;
  inputTokens: number;
  outputTokens: number;
  cacheHitPercent: number;
  requests: number;
}

/** Estimate in display currency using the effective (group) prices. */
export function estimateCost(
  model: PricingModel,
  meta: PricingMeta | null,
  input: EstimateInput,
): CostEstimate {
  const prices = effectivePrices(model, input.group);
  if (model.mode === "per_request") {
    const perCall = currencyValue(prices.input, meta) ?? 0;
    const requests = Math.max(0, input.requests);
    return {
      total: perCall * requests,
      inputCost: 0,
      cacheCost: 0,
      outputCost: 0,
      unit: "request",
    };
  }

  const inputPrice = currencyValue(prices.input, meta) ?? 0;
  const outputPrice = currencyValue(prices.output, meta) ?? 0;
  const cachePrice = currencyValue(prices.cacheRead, meta) ?? inputPrice;
  const cacheRate = Math.min(100, Math.max(0, input.cacheHitPercent)) / 100;
  const inputTokens = Math.max(0, input.inputTokens);
  const outputTokens = Math.max(0, input.outputTokens);

  const inputCost = (inputTokens * (1 - cacheRate) * inputPrice) / 1_000_000;
  const cacheCost = (inputTokens * cacheRate * cachePrice) / 1_000_000;
  const outputCost = (outputTokens * outputPrice) / 1_000_000;

  return {
    total: inputCost + cacheCost + outputCost,
    inputCost,
    cacheCost,
    outputCost,
    unit: "token",
  };
}

export function formatMoneyValue(value: number): string {
  return `${value < 0.01 ? value.toFixed(4) : value.toFixed(2)}`;
}
