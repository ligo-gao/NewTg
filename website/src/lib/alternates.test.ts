import { describe, expect, it } from "vitest";
import { LOCALES } from "@/config/locales";
import { languageAlternates } from "./alternates";

describe("languageAlternates", () => {
  it("lists every locale for the home page plus x-default", () => {
    const alternates = languageAlternates("");
    const expected = [...LOCALES.map((locale) => locale.code), "x-default"].sort();

    expect(Object.keys(alternates).sort()).toEqual(expected);
    expect(alternates["zh-CN"]).toBe("/");
    expect(alternates.en).toBe("/en/");
    expect(alternates["zh-TW"]).toBe("/zh-TW/");
    expect(alternates.ja).toBe("/ja/");
    expect(alternates["x-default"]).toBe("/");
  });

  it("keeps the sub-page under every locale prefix", () => {
    const alternates = languageAlternates("pricing");

    expect(alternates["zh-CN"]).toBe("/pricing/");
    expect(alternates.en).toBe("/en/pricing/");
    expect(alternates.vi).toBe("/vi/pricing/");
    expect(alternates["x-default"]).toBe("/pricing/");
  });
});
