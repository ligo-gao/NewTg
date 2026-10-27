import { describe, expect, it } from "vitest";
import { isMarketingPath, localeForPath, localizePath } from "./locale-path";

describe("localeForPath", () => {
  it("maps the default-locale marketing routes to zh-CN", () => {
    expect(localeForPath("/")).toBe("zh-CN");
    expect(localeForPath("/pricing/")).toBe("zh-CN");
  });

  it("reads the locale prefix of a localized marketing route", () => {
    expect(localeForPath("/en/")).toBe("en");
    expect(localeForPath("/fr/pricing/")).toBe("fr");
  });

  it("returns null outside the marketing pages", () => {
    expect(localeForPath("/dashboard")).toBeNull();
    expect(localeForPath("/en/models/")).toBeNull();
  });
});

describe("isMarketingPath", () => {
  it("accepts the home and pricing pages in every locale", () => {
    for (const pathname of ["/", "/pricing/", "/en/", "/fr/pricing/", "/ja/", "/vi/pricing/"]) {
      expect(isMarketingPath(pathname)).toBe(true);
    }
  });

  it("rejects the console and unknown marketing paths", () => {
    expect(isMarketingPath("/dashboard")).toBe(false);
    // localizePath 只对营销站页面有意义：静态导出没有 /<locale>/models 路由，
    // 把 /models 改写成 /fr/models/ 只会得到一个 404，不是可预期的行为。
    expect(isMarketingPath("/models")).toBe(false);
    expect(isMarketingPath("/en/models/")).toBe(false);
    expect(isMarketingPath("/en/rankings/")).toBe(false);
  });
});

describe("localizePath", () => {
  it("keeps the page under the new locale prefix", () => {
    expect(localizePath("/pricing/", "fr")).toBe("/fr/pricing/");
    expect(localizePath("/en/", "ja")).toBe("/ja/");
    expect(localizePath("/", "zh-TW")).toBe("/zh-TW/");
  });

  it("drops the prefix for the default locale", () => {
    expect(localizePath("/en/", "zh-CN")).toBe("/");
    expect(localizePath("/fr/pricing/", "zh-CN")).toBe("/pricing/");
  });
});
