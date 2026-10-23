import { describe, expect, it } from "vitest";
import type { PricingModel } from "@/lib/pricing-api";
import { filterPricingModels } from "@/lib/pricing-filter";

function model(overrides: Partial<PricingModel> & Pick<PricingModel, "model_id">): PricingModel {
  return {
    display_name: overrides.model_id,
    vendor: "OpenAI",
    context_length: null,
    max_output: null,
    prompt_price: null,
    completion_price: null,
    cache_read_price: null,
    cache_write_price: null,
    is_free: false,
    mode: "per_token",
    description: null,
    capabilities: null,
    tags: null,
    endpoints: null,
    group_price: null,
    ...overrides,
  };
}

const list: PricingModel[] = [
  model({ model_id: "openai/gpt-4o", display_name: "GPT-4o", vendor: "OpenAI" }),
  model({ model_id: "deepseek/deepseek-chat", display_name: "DeepSeek-V3.2", vendor: "DeepSeek" }),
  model({ model_id: "deepseek/deepseek-reasoner", display_name: "DeepSeek-R1", vendor: "DeepSeek" }),
  model({ model_id: "anthropic/claude", display_name: "Claude Sonnet 4.5", vendor: "Anthropic" }),
];

describe("filterPricingModels", () => {
  it("returns everything with an empty query and the all vendor", () => {
    expect(filterPricingModels(list, { query: "", vendor: "all" })).toHaveLength(4);
  });

  it("matches display name, model id, and vendor case-insensitively", () => {
    expect(filterPricingModels(list, { query: "deepseek", vendor: "all" })).toHaveLength(2);
    expect(filterPricingModels(list, { query: "ANTHROPIC", vendor: "all" })).toHaveLength(1);
    expect(filterPricingModels(list, { query: "gpt-4o", vendor: "all" })).toHaveLength(1);
  });

  it("applies the vendor filter and query together", () => {
    expect(filterPricingModels(list, { query: "deepseek", vendor: "Anthropic" })).toEqual([]);
    expect(filterPricingModels(list, { query: "", vendor: "DeepSeek" })).toHaveLength(2);
  });

  it("accepts the lowercase facet values returned by the backend", () => {
    expect(filterPricingModels(list, { query: "", vendor: "deepseek" })).toHaveLength(2);
    expect(filterPricingModels(list, { query: "", vendor: "anthropic" })).toHaveLength(1);
  });
});
