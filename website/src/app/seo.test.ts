import { describe, expect, it } from "vitest";
import { LOCALES } from "@/config/locales";
import { site } from "@/config/site";
import robots from "./robots";
import sitemap from "./sitemap";

describe("robots", () => {
  it("points crawlers at the sitemap and keeps machine routes out", () => {
    const result = robots();

    expect(result.sitemap).toBe(`${site.url}/sitemap.xml`);
    expect(result.rules).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ userAgent: "*", allow: "/" }),
      ]),
    );
    const disallow = (result.rules as Array<{ disallow?: string | string[] }>)[0]?.disallow;
    expect(disallow).toEqual(
      expect.arrayContaining(["/api/", "/v1/", "/v1beta/", "/pg/", "/readyz", "/healthz"]),
    );
  });
});

describe("sitemap", () => {
  it("lists every marketing page in every locale", () => {
    const entries = sitemap();

    expect(entries).toHaveLength(LOCALES.length * 2);
    const urls = entries.map((entry) => entry.url);
    expect(urls).toContain(`${site.url}/`);
    expect(urls).toContain(`${site.url}/pricing/`);
    expect(urls).toContain(`${site.url}/en/`);
    expect(urls).toContain(`${site.url}/ja/pricing/`);
  });

  it("declares the hreflang cluster on each entry", () => {
    const pricingEn = sitemap().find((entry) => entry.url === `${site.url}/en/pricing/`);
    const languages = pricingEn?.alternates?.languages;

    expect(Object.keys(languages ?? {}).sort()).toEqual(
      [...LOCALES.map((locale) => locale.code), "x-default"].sort(),
    );
    expect(languages?.["zh-CN"]).toBe(`${site.url}/pricing/`);
    expect(languages?.["x-default"]).toBe(`${site.url}/pricing/`);
    expect(languages?.vi).toBe(`${site.url}/vi/pricing/`);
  });
});
