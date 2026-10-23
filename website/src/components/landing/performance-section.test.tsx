import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { HomeDataProvider } from "@/components/landing/home-data";
import { copy } from "@/config/copy";
import { PerformanceSection } from "@/components/landing/performance-section";
import type { ModelMetrics, PricingCatalog, PricingModel } from "@/lib/pricing-api";

const meta = {
  credit_symbol: "¥",
  quota_per_unit: 100,
  usd_exchange_rate: 7.1,
  version: "test",
  generated_at: "2026-10-23T00:00:00Z",
};

function model(modelID: string, displayName: string, vendor: string): PricingModel {
  return {
    model_id: modelID,
    display_name: displayName,
    vendor,
    context_length: 128000,
    max_output: 16384,
    prompt_price: "250",
    completion_price: "1000",
    cache_read_price: null,
    cache_write_price: null,
    is_free: false,
    mode: "per_token",
    description: null,
    capabilities: [],
    tags: [],
  };
}

const items = [
  model("openai/gpt-4o", "GPT-4o", "OpenAI"),
  model("deepseek/deepseek-chat", "DeepSeek-V3.2", "DeepSeek"),
  model("deepseek/deepseek-reasoner", "DeepSeek-R1", "DeepSeek"),
  model("anthropic/claude", "Claude Sonnet 4.5", "Anthropic"),
  model("google/gemini", "Gemini 2.5 Pro", "Google"),
  model("qwen/qwen3-max", "Qwen3-Max", "阿里通义"),
];

const catalog: PricingCatalog = {
  items,
  total: "6",
  page: 1,
  page_size: 100,
  facets: { vendors: [] },
  meta,
};

function metricsWith(sampleSize: number, tps: number | null): ModelMetrics {
  return {
    range: "7d",
    window: { start: "2026-10-16T00:00:00Z", end: "2026-10-23T00:00:00Z" },
    summary: {
      avg_latency_ms: 820,
      avg_ttft_ms: 210,
      success_rate: 0.99,
      tps,
      failed_24h: 1,
      sample_size: sampleSize,
    },
    per_group: [],
  };
}

function jsonResponse(body: unknown): Response {
  return { ok: true, status: 200, json: async () => body } as Response;
}

function stubFetch(failAllMetrics = false) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.startsWith("/api/pricing?")) {
        return Promise.resolve(jsonResponse(catalog));
      }
      if (url.includes("/metrics")) {
        if (failAllMetrics) {
          return Promise.resolve({ ok: false, status: 503, json: async () => ({}) } as Response);
        }
        const encoded = url.split("/api/pricing/")[1].split("/metrics")[0];
        const modelID = decodeURIComponent(encoded);
        if (modelID === "google/gemini") {
          return Promise.resolve({ ok: false, status: 503, json: async () => ({}) } as Response);
        }
        return Promise.resolve(jsonResponse(metricsWith(modelID === "deepseek/deepseek-reasoner" ? 5 : 1543, 42.5)));
      }
      return Promise.resolve({ ok: false, status: 404, json: async () => ({}) } as Response);
    }),
  );
}

function renderSection() {
  return render(
    <HomeDataProvider>
      <PerformanceSection copy={copy} />
    </HomeDataProvider>,
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("PerformanceSection", () => {
  it("shows real TTFT and tokens/s, flags low samples, and leaves failed models at a dash", async () => {
    stubFetch();
    renderSection();

    expect(await screen.findByRole("heading", { name: "真实调用性能" })).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getAllByText("210 ms").length).toBeGreaterThan(0);
    });
    expect(screen.getAllByText("42.5 tokens/s").length).toBeGreaterThan(0);
    expect(screen.getByText("样本不足")).toBeInTheDocument();

    const geminiHeading = screen.getByText("Gemini 2.5 Pro");
    const geminiRow = geminiHeading.closest("tr");
    if (!(geminiRow instanceof HTMLTableRowElement)) {
      throw new Error("missing Gemini row");
    }
    expect(within(geminiRow).getAllByText("—")).toHaveLength(2);
  });

  it("degrades to dashes and a retry notice when the metrics endpoint is unavailable", async () => {
    stubFetch(true);
    renderSection();

    expect(await screen.findByText("性能数据暂不可用")).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getAllByText("—").length).toBeGreaterThan(0);
    });
    expect(screen.getByRole("button", { name: "重试" })).toBeInTheDocument();
  });
});
