import { describe, expect, it } from "vitest";
import { models } from "@/config/models";
import { filterModels } from "@/lib/filter-models";

describe("filterModels", () => {
  it("returns every model when the query is empty and the vendor is all", () => {
    expect(filterModels(models, { query: "", vendor: "all" })).toHaveLength(6);
  });

  it("matches a model name", () => {
    const result = filterModels(models, { query: "deepseek", vendor: "all" });
    expect(result.map((model) => model.name)).toEqual(["DeepSeek-V3.2"]);
  });

  it("filters by vendor", () => {
    const result = filterModels(models, { query: "", vendor: "google" });
    expect(result.map((model) => model.name)).toEqual(["Gemini 2.5 Pro"]);
  });

  it("applies the query and the vendor together", () => {
    expect(filterModels(models, { query: "gemini", vendor: "deepseek" })).toEqual([]);
  });

  it("is case insensitive and matches the vendor name", () => {
    const result = filterModels(models, { query: "ANTHROPIC", vendor: "all" });
    expect(result.map((model) => model.name)).toEqual(["Claude Sonnet 4.5"]);
  });
});
