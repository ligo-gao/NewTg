import { describe, expect, it } from "vitest";
import type { PricingMeta } from "@/lib/pricing-api";
import {
  formatPrice,
  formatTTFT,
  formatTPS,
  formatTokenCount,
  isLowSample,
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

describe("formatTTFT", () => {
  it("shows milliseconds below one second and seconds above", () => {
    expect(formatTTFT(412)).toBe("412 ms");
    expect(formatTTFT(1250)).toBe("1.3 s");
  });

  it("shows a dash when there are no streaming samples", () => {
    expect(formatTTFT(null)).toBe("—");
  });
});

describe("formatTPS", () => {
  it("formats tokens per second", () => {
    expect(formatTPS(42.55)).toBe("42.5 tokens/s");
    expect(formatTPS(120.4)).toBe("120 tokens/s");
  });

  it("shows a dash without samples", () => {
    expect(formatTPS(null)).toBe("—");
  });
});

describe("isLowSample", () => {
  it("flags samples below 20 but not zero", () => {
    expect(isLowSample(19)).toBe(true);
    expect(isLowSample(20)).toBe(false);
    expect(isLowSample(0)).toBe(false);
  });
});
