import type { PricingMeta, PricingModel } from "@/lib/pricing-api";

const PLACEHOLDER = "—";

export function formatTokenCount(tokens: number | null): string {
  if (tokens === null || tokens <= 0) {
    return PLACEHOLDER;
  }
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

export function formatPrice(
  credits: string | null,
  meta: PricingMeta | null,
  mode: PricingModel["mode"],
): string {
  if (credits === null || meta === null) {
    return PLACEHOLDER;
  }
  const value = Number(credits);
  if (!Number.isFinite(value)) {
    return PLACEHOLDER;
  }
  if (value === 0) {
    return "免费";
  }
  const quotaPerUnit = meta.quota_per_unit > 0 ? meta.quota_per_unit : 100;
  const amount = value / quotaPerUnit;
  const formatted = amount.toLocaleString("zh-CN", {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  });
  const unit = mode === "per_request" ? "/ 次" : "/ 1M";
  return `${meta.credit_symbol}${formatted} ${unit}`;
}

/** 公开端点里能用流式 chat 请求探测的协议族。 */
export const CHAT_ENDPOINTS = ["chat.completions", "messages", "generateContent"] as const;

/**
 * 与后端 `store.ListPublicChatModels` 同口径（真正的规则在
 * `publicChatModelNames` / `hasChatEndpoint`）：没有端点元数据时保守保留
 * （无法证明它不是对话模型），有元数据时只保留含对话端点的模型。
 */
export function isChatCapable(model: PricingModel): boolean {
  const endpoints = model.endpoints ?? [];
  if (endpoints.length === 0) {
    return true;
  }
  return CHAT_ENDPOINTS.some((endpoint) => endpoints.includes(endpoint));
}
