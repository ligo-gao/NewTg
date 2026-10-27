import { describe, expect, it } from "vitest";
import {
  formatLatencyMs,
  formatSampleSize,
  formatSuccessRate,
  formatTestedAt,
  formatTps,
  successTone,
} from "@/lib/perf-format";

const NOW = Date.parse("2026-10-28T12:00:00Z");

describe("formatLatencyMs", () => {
  it("shows grouped whole milliseconds", () => {
    expect(formatLatencyMs(412, "zh-CN")).toBe("412");
    expect(formatLatencyMs(3240, "zh-CN")).toBe("3,240");
  });

  it("shows a dash for missing or invalid values", () => {
    expect(formatLatencyMs(null, "zh-CN")).toBe("—");
    expect(formatLatencyMs(-1, "zh-CN")).toBe("—");
  });
});

describe("formatTps", () => {
  it("keeps one decimal", () => {
    expect(formatTps(91.64, "zh-CN")).toBe("91.6");
    expect(formatTps(120, "zh-CN")).toBe("120.0");
  });

  it("shows a dash for missing values", () => {
    expect(formatTps(null, "zh-CN")).toBe("—");
  });
});

describe("formatSuccessRate", () => {
  it("formats a fraction as a localized percentage", () => {
    expect(formatSuccessRate(0.992, "zh-CN")).toBe("99.2%");
    expect(formatSuccessRate(1, "en")).toBe("100.0%");
  });

  it("shows a dash for missing values", () => {
    expect(formatSuccessRate(null, "zh-CN")).toBe("—");
    expect(formatSuccessRate(undefined, "zh-CN")).toBe("—");
  });
});

describe("formatSampleSize", () => {
  it("groups large samples per locale", () => {
    expect(formatSampleSize(1543, "zh-CN")).toBe("1,543");
    expect(formatSampleSize(0, "zh-CN")).toBe("0");
  });

  it("shows a dash for missing values", () => {
    expect(formatSampleSize(null, "zh-CN")).toBe("—");
  });
});

describe("formatTestedAt", () => {
  it("uses relative units, matching the largest sensible one", () => {
    expect(formatTestedAt("2026-10-28T11:59:30Z", "zh-CN", NOW)).toBe("此刻");
    expect(formatTestedAt("2026-10-28T11:30:00Z", "zh-CN", NOW)).toBe("30分钟前");
    expect(formatTestedAt("2026-10-28T09:00:00Z", "zh-CN", NOW)).toBe("3小时前");
    expect(formatTestedAt("2026-10-27T12:00:00Z", "zh-CN", NOW)).toBe("昨天");
    expect(formatTestedAt("2026-10-26T12:00:00Z", "zh-CN", NOW)).toBe("前天");
  });

  it("formats in the requested language", () => {
    expect(formatTestedAt("2026-10-28T09:00:00Z", "en", NOW)).toBe("3 hours ago");
  });

  it("shows a dash for missing or invalid timestamps", () => {
    expect(formatTestedAt(null, "zh-CN", NOW)).toBe("—");
    expect(formatTestedAt("not-a-date", "zh-CN", NOW)).toBe("—");
  });
});

describe("successTone", () => {
  it("flags healthy and degraded rates", () => {
    expect(successTone(0.995)).toBe("good");
    expect(successTone(0.97)).toBe("plain");
    expect(successTone(0.9)).toBe("warn");
    expect(successTone(null)).toBe("missing");
  });
});
