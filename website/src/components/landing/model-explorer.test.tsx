import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { HomeDataProvider } from "@/components/landing/home-data";
import { copy } from "@/config/copy";
import { ModelExplorer } from "@/components/landing/model-explorer";
import type { ModelMetrics, PricingCatalog, PricingModel } from "@/lib/pricing-api";

const meta = {
  credit_symbol: "¥",
  quota_per_unit: 100,
  usd_exchange_rate: 7.1,
  version: "test",
  generated_at: "2026-10-23T00:00:00Z",
};

function model(
  modelID: string,
  displayName: string,
  vendor: string,
  overrides: Partial<PricingModel> = {},
): PricingModel {
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
    endpoints: [],
    group_price: null,
    ...overrides,
  };
}

const catalog: PricingCatalog = {
  items: [
    model("openai/gpt-4o", "GPT-4o", "OpenAI"),
    model("deepseek/deepseek-chat", "DeepSeek-V3.2", "DeepSeek", {
      prompt_price: "28",
      completion_price: "42",
      pricing_schedule: {
        kind: "peak_offpeak",
        timezone: "Asia/Shanghai",
        default_multiplier: 0.5,
        holiday_policy: "cn_offpeak",
        windows: [
          { name: "上午高峰", days: [1, 2, 3, 4, 5], start: "09:00", end: "12:00", multiplier: 1 },
        ],
      },
    }),
    model("deepseek/deepseek-reasoner", "DeepSeek-R1", "DeepSeek"),
    model("anthropic/claude", "Claude Sonnet 4.5", "Anthropic"),
    model("google/gemini", "Gemini 2.5 Pro", "Google", { is_free: false }),
    model("qwen/qwen3-max", "Qwen3-Max", "阿里通义"),
  ],
  total: "6",
  page: 1,
  page_size: 100,
  facets: {
    capabilities: [],
    context: [],
    groups: [],
    endpoints: [],
    pricing_types: [],
    vendors: [
      { value: "openai", label: "OpenAI", count: 1 },
      { value: "deepseek", label: "DeepSeek", count: 2 },
      { value: "anthropic", label: "Anthropic", count: 1 },
      { value: "google", label: "Google", count: 1 },
      { value: "阿里通义", label: "阿里通义", count: 1 },
    ],
  },
  meta,
};

function metricsFor(modelID: string): ModelMetrics {
  const lowSample = modelID === "deepseek/deepseek-reasoner";
  return {
    range: "7d",
    window: { start: "2026-10-16T00:00:00Z", end: "2026-10-23T00:00:00Z" },
    summary: {
      avg_latency_ms: 820,
      avg_ttft_ms: 210,
      success_rate: 0.99,
      tps: 42.5,
      failed_24h: 1,
      sample_size: lowSample ? 5 : 1543,
    },
    per_group: [],
    latest_test: null,
  };
}

function jsonResponse(body: unknown): Response {
  return {
    ok: true,
    status: 200,
    json: async () => body,
  } as Response;
}

function stubFetch(failMetricsFor?: string) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.startsWith("/api/pricing?")) {
        return Promise.resolve(jsonResponse(catalog));
      }
      if (url.includes("/metrics")) {
        const encoded = url.split("/api/pricing/")[1].split("/metrics")[0];
        const modelID = decodeURIComponent(encoded);
        if (modelID === failMetricsFor) {
          return Promise.resolve({ ok: false, status: 503, json: async () => ({}) } as Response);
        }
        return Promise.resolve(jsonResponse(metricsFor(modelID)));
      }
      return Promise.resolve({ ok: false, status: 404, json: async () => ({}) } as Response);
    }),
  );
}

function renderExplorer() {
  return render(
    <HomeDataProvider>
      <ModelExplorer copy={copy} />
    </HomeDataProvider>,
  );
}

beforeEach(() => {
  stubFetch();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("ModelExplorer live data", () => {
  it("renders the six models returned by the backend with real prices", async () => {
    renderExplorer();

    await waitFor(() => {
      expect(screen.getAllByRole("article")).toHaveLength(6);
    });
    expect(screen.getByRole("heading", { name: "GPT-4o" })).toBeInTheDocument();
    expect(screen.getAllByText("¥2.50 / 1M").length).toBeGreaterThan(0);
    expect(document.querySelector("#models [aria-live]")?.textContent).toContain("6");
  });

  it("shows the peak/off-peak badge only for models with a published schedule", async () => {
    renderExplorer();

    const scheduledHeading = await screen.findByRole("heading", { name: "DeepSeek-V3.2" });
    const scheduledCard = scheduledHeading.closest("article");
    if (!(scheduledCard instanceof HTMLElement)) {
      throw new Error("missing scheduled model card");
    }
    expect(within(scheduledCard).getByTestId("schedule-badge")).toHaveTextContent(
      copy.pricingPage.schedule.badge,
    );

    const flatHeading = screen.getByRole("heading", { name: "GPT-4o" });
    const flatCard = flatHeading.closest("article");
    if (!(flatCard instanceof HTMLElement)) {
      throw new Error("missing flat model card");
    }
    expect(within(flatCard).queryByTestId("schedule-badge")).not.toBeInTheDocument();
  });

  it("searches the full catalog, not just the six featured models", async () => {    const user = userEvent.setup();
    renderExplorer();
    await screen.findByRole("heading", { name: "GPT-4o" });

    await user.type(screen.getByLabelText("搜索模型"), "deepseek");

    await waitFor(() => {
      expect(screen.getAllByRole("article")).toHaveLength(2);
    });
    expect(screen.getByRole("heading", { name: "DeepSeek-V3.2" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "DeepSeek-R1" })).toBeInTheDocument();
    expect(document.querySelector("#models [aria-live]")?.textContent).toContain("2");
  });

  it("filters by vendor and resets from the empty state", async () => {
    const user = userEvent.setup();
    renderExplorer();
    await screen.findByRole("heading", { name: "GPT-4o" });

    await user.click(screen.getByRole("button", { name: "Google" }));
    await waitFor(() => {
      expect(screen.getAllByRole("article")).toHaveLength(1);
    });

    await user.type(screen.getByLabelText("搜索模型"), "zzz");
    expect(await screen.findByText("没有匹配的模型。")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "清除搜索与筛选" }));
    await waitFor(() => {
      expect(screen.getAllByRole("article")).toHaveLength(6);
    });
    expect(screen.getByRole("button", { name: "全部" })).toHaveAttribute("aria-pressed", "true");
  });

  it("compares two live models and clears the selection", async () => {
    const user = userEvent.setup();
    renderExplorer();
    await screen.findByRole("heading", { name: "GPT-4o" });

    const gptCard = screen.getByRole("heading", { name: "GPT-4o" }).closest("article");
    const claudeCard = screen.getByRole("heading", { name: "Claude Sonnet 4.5" }).closest("article");
    if (!(gptCard instanceof HTMLElement) || !(claudeCard instanceof HTMLElement)) {
      throw new Error("missing model cards");
    }

    await user.click(within(gptCard).getByRole("checkbox", { name: "对比 GPT-4o" }));
    await user.click(
      within(claudeCard).getByRole("checkbox", { name: "对比 Claude Sonnet 4.5" }),
    );

    const bar = screen.getByRole("region", { name: "模型对比" });
    expect(bar).toHaveTextContent("已选 2 个模型");

    await user.click(within(bar).getByRole("button", { name: "对比" }));
    const dialog = screen.getByRole("dialog", { name: "模型对比" });
    expect((dialog as HTMLDialogElement).open).toBe(true);
    expect(within(dialog).getByRole("columnheader", { name: "GPT-4o" })).toBeInTheDocument();
    expect(
      within(dialog).getByRole("columnheader", { name: "Claude Sonnet 4.5" }),
    ).toBeInTheDocument();

    await user.click(within(bar).getByRole("button", { name: "清除" }));
    expect(screen.queryByRole("region", { name: "模型对比" })).not.toBeInTheDocument();
  });
});
