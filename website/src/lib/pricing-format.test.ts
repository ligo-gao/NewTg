import { describe, expect, it } from "vitest";
import type { PricingMeta, PricingModel } from "@/lib/pricing-api";
import {
  formatPrice,
  formatTokenCount,
  isChatCapable,
} from "@/lib/pricing-format";

const meta: PricingMeta = {
  credit_symbol: "¥",
  quota_per_unit: 100,
  usd_exchange_rate: 7.1,
  version: "v1",
  generated_at: "2026-10-23T00:00:00Z",
};

describe("formatTokenCount", () => {
  it("abbreviates thousands and millions", () => {
    expect(formatTokenCount(128000)).toBe("128K");
    expect(formatTokenCount(1000000)).toBe("1M");
    expect(formatTokenCount(256000)).toBe("256K");
    expect(formatTokenCount(2000000)).toBe("2M");
    expect(formatTokenCount(512)).toBe("512");
  });

  it("uses a dash for missing or invalid values", () => {
    expect(formatTokenCount(null)).toBe("—");
    expect(formatTokenCount(0)).toBe("—");
  });
});

describe("formatPrice", () => {
  it("converts credits to the display currency per 1M tokens", () => {
    expect(formatPrice("250", meta, "per_token")).toBe("¥2.50 / 1M");
  });

  it("uses the per-request unit for per_request models", () => {
    expect(formatPrice("200", meta, "per_request")).toBe("¥2.00 / 次");
  });

  it("shows free and missing prices without inventing numbers", () => {
    expect(formatPrice("0", meta, "per_token")).toBe("免费");
    expect(formatPrice(null, meta, "per_token")).toBe("—");
    expect(formatPrice("250", null, "per_token")).toBe("—");
  });

  it("falls back to 100 credits per unit when meta is malformed", () => {
    expect(formatPrice("250", { ...meta, quota_per_unit: 0 }, "per_token")).toBe("¥2.50 / 1M");
  });
});

describe("isChatCapable", () => {
  const base = { endpoints: [] as string[] } as unknown as PricingModel;

  it("keeps models without endpoint metadata (conservative)", () => {
    expect(isChatCapable({ ...base, endpoints: [] })).toBe(true);
    expect(isChatCapable({ ...base, endpoints: null })).toBe(true);
    // Go 侧恒初始化为空切片，字段缺失只是防御路径；保守保留与后端一致。
    expect(isChatCapable({ ...base, endpoints: undefined } as unknown as PricingModel)).toBe(true);
  });

  it("keeps chat-capable models", () => {
    expect(isChatCapable({ ...base, endpoints: ["chat.completions"] })).toBe(true);
    expect(isChatCapable({ ...base, endpoints: ["messages"] })).toBe(true);
    expect(isChatCapable({ ...base, endpoints: ["generateContent"] })).toBe(true);
    // 混合端点里只要有一个对话端点就算：挡住「只看第一个端点」或 every 的漂移。
    expect(isChatCapable({ ...base, endpoints: ["embeddings", "chat.completions"] })).toBe(true);
  });

  it("drops models that only expose non-chat endpoints", () => {
    expect(isChatCapable({ ...base, endpoints: ["embeddings"] })).toBe(false);
    expect(isChatCapable({ ...base, endpoints: ["images.generations", "rerank"] })).toBe(false);
  });
});
