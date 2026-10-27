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

function model(
  modelID: string,
  displayName: string,
  vendor: string,
  endpoints: string[] = [],
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
    endpoints,
    group_price: null,
  };
}

const items = [
  model("openai/gpt-4o", "GPT-4o", "OpenAI"),
  model("deepseek/deepseek-chat", "DeepSeek-V3.2", "DeepSeek"),
  model("deepseek/deepseek-reasoner", "DeepSeek-R1", "DeepSeek"),
  model("anthropic/claude", "Claude Sonnet 4.5", "Anthropic"),
  model("google/gemini", "Gemini 2.5 Pro", "Google"),
  model("qwen/qwen3-max", "Qwen3-Max", "阿里通义"),
  model("openai/text-embedding-3", "text-embedding-3-large", "OpenAI", ["embeddings"]),
];

const catalog: PricingCatalog = {
  items,
  total: "7",
  page: 1,
  page_size: 100,
  facets: { vendors: [], capabilities: [], context: [], groups: [], endpoints: [], pricing_types: [] },
  meta,
};

function metricsWith(
  sampleSize: number,
  tps: number | null,
  options: {
    avgTTFTMs?: number | null;
    latestTest?: ModelMetrics["latest_test"] | undefined;
  } = {},
): ModelMetrics {
  return {
    range: "7d",
    window: { start: "2026-10-16T00:00:00Z", end: "2026-10-23T00:00:00Z" },
    summary: {
      avg_latency_ms: 820,
      avg_ttft_ms: options.avgTTFTMs === undefined ? 210 : options.avgTTFTMs,
      success_rate: 0.99,
      tps,
      failed_24h: 1,
      sample_size: sampleSize,
    },
    per_group: [],
    latest_test: options.latestTest ?? null,
  };
}

function latestTest(
  overrides: Partial<NonNullable<ModelMetrics["latest_test"]>> = {},
): NonNullable<ModelMetrics["latest_test"]> {
  return {
    ttft_ms_p50: 210,
    ttft_ms_p95: 480,
    latency_ms_p50: 900,
    latency_ms_p95: 1800,
    tps: 42.5,
    success_rate: 0.99,
    sample_size: 60,
    tested_at: new Date().toISOString(),
    ...overrides,
  };
}

function jsonResponse(body: unknown): Response {
  return { ok: true, status: 200, json: async () => body } as Response;
}

function stubFetch(
  options: {
    failAllMetrics?: boolean;
    metrics?: Record<string, ModelMetrics>;
  } = {},
) {
  const { failAllMetrics = false, metrics = {} } = options;
  const fetchMock = vi.fn((input: RequestInfo | URL) => {
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
      if (metrics[modelID]) {
        return Promise.resolve(jsonResponse(metrics[modelID]));
      }
      if (modelID === "google/gemini") {
        return Promise.resolve({ ok: false, status: 503, json: async () => ({}) } as Response);
      }
      return Promise.resolve(jsonResponse(metricsWith(1543, 42.5)));
    }
    return Promise.resolve({ ok: false, status: 404, json: async () => ({}) } as Response);
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function renderSection() {
  return render(
    <HomeDataProvider>
      <PerformanceSection copy={copy} />
    </HomeDataProvider>,
  );
}

async function findRow(name: string): Promise<HTMLTableRowElement> {
  const cell = await screen.findByText(name);
  const row = cell.closest("tr");
  if (!(row instanceof HTMLTableRowElement)) {
    throw new Error(`missing ${name} row`);
  }
  return row;
}

function requestedURLs(fetchMock: ReturnType<typeof stubFetch>): string[] {
  return fetchMock.mock.calls.map(([input]) =>
    typeof input === "string" ? input : input.toString(),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("PerformanceSection", () => {
  it("shows the latest probe values and ignores the usage summary", async () => {
    stubFetch({
      metrics: {
        "openai/gpt-4o": metricsWith(1543, 1.5, {
          avgTTFTMs: 999,
          latestTest: latestTest({ ttft_ms_p50: 210, tps: 42.5 }),
        }),
      },
    });
    renderSection();

    const row = await findRow("GPT-4o");
    await waitFor(() => {
      expect(within(row).getByText("210 ms")).toBeInTheDocument();
    });
    expect(within(row).getByText("42.5 tokens/s")).toBeInTheDocument();
    expect(within(row).queryByText("999 ms")).toBeNull();
    expect(within(row).queryByText("1.5 tokens/s")).toBeNull();
  });

  it("does not fall back to the usage summary", async () => {
    stubFetch({
      metrics: {
        "openai/gpt-4o": metricsWith(1543, 42.5, { avgTTFTMs: 210, latestTest: null }),
      },
    });
    renderSection();

    const row = await findRow("GPT-4o");
    await waitFor(() => {
      expect(within(row).getAllByText("—")).toHaveLength(2);
    });
    expect(within(row).queryByText("210 ms")).toBeNull();
    expect(within(row).queryByText("42.5 tokens/s")).toBeNull();
  });

  it("keeps probes from just under 36 hours", async () => {
    // 钉住阈值本身：只有 37h 与 0h 两个用例时，任何 (0h, 37h] 的常量都能通过。
    stubFetch({
      metrics: {
        "openai/gpt-4o": metricsWith(1543, 42.5, {
          latestTest: latestTest({
            ttft_ms_p50: 210,
            tps: 42.5,
            tested_at: new Date(Date.now() - 35 * 3600_000).toISOString(),
          }),
        }),
      },
    });
    renderSection();

    const row = await findRow("GPT-4o");
    await waitFor(() => {
      expect(within(row).getByText("210 ms")).toBeInTheDocument();
    });
    expect(within(row).getByText("42.5 tokens/s")).toBeInTheDocument();
  });

  it("renders a dash for a fresh probe field that is missing", async () => {
    stubFetch({
      metrics: {
        "openai/gpt-4o": metricsWith(1543, 42.5, {
          latestTest: latestTest({ ttft_ms_p50: null, tps: 42.5 }),
        }),
      },
    });
    renderSection();

    const row = await findRow("GPT-4o");
    await waitFor(() => {
      expect(within(row).getByText("42.5 tokens/s")).toBeInTheDocument();
    });
    // 缺失的单列显示 —，不能出现 "null" 或崩溃。
    expect(within(row).getByText("—")).toBeInTheDocument();
    expect(within(row).queryByText("null")).toBeNull();
  });

  it("hides probes older than 36 hours", async () => {
    stubFetch({
      metrics: {
        "openai/gpt-4o": metricsWith(1543, 42.5, {
          latestTest: latestTest({
            ttft_ms_p50: 210,
            tps: 42.5,
            tested_at: new Date(Date.now() - 37 * 3600_000).toISOString(),
          }),
        }),
      },
    });
    renderSection();

    const row = await findRow("GPT-4o");
    await waitFor(() => {
      expect(within(row).getAllByText("—")).toHaveLength(2);
    });
    expect(within(row).queryByText("210 ms")).toBeNull();
    expect(within(row).queryByText("42.5 tokens/s")).toBeNull();
  });

  it("keeps probes with a future timestamp", async () => {
    stubFetch({
      metrics: {
        "openai/gpt-4o": metricsWith(1543, 1.5, {
          avgTTFTMs: 999,
          latestTest: latestTest({
            ttft_ms_p50: 210,
            tps: 42.5,
            tested_at: new Date(Date.now() + 3600_000).toISOString(),
          }),
        }),
      },
    });
    renderSection();

    const row = await findRow("GPT-4o");
    await waitFor(() => {
      expect(within(row).getByText("210 ms")).toBeInTheDocument();
    });
    expect(within(row).getByText("42.5 tokens/s")).toBeInTheDocument();
    expect(within(row).queryByText("999 ms")).toBeNull();
  });

  it("degrades to dashes and a retry notice when the metrics endpoint is unavailable", async () => {
    stubFetch({ failAllMetrics: true });
    renderSection();

    expect(await screen.findByText("性能数据暂不可用")).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getAllByText("—").length).toBeGreaterThan(0);
    });
    expect(screen.getByRole("button", { name: "重试" })).toBeInTheDocument();
  });

  it("excludes non-chat models from the performance table", async () => {
    // 固定采样：否则回退到 featured（7 个模型取 6 个）时，嵌入模型只有 6/7
    // 的概率被抽中，回退能逃过断言。
    vi.spyOn(Math, "random").mockReturnValue(0);
    const fetchMock = stubFetch({
      metrics: {
        "openai/gpt-4o": metricsWith(1543, 1.5, {
          latestTest: latestTest({ ttft_ms_p50: 210, tps: 42.5 }),
        }),
      },
    });
    renderSection();

    const row = await findRow("GPT-4o");
    await waitFor(() => {
      expect(within(row).getByText("210 ms")).toBeInTheDocument();
    });

    expect(screen.queryByText("text-embedding-3-large")).toBeNull();
    const embeddingMetricsCalls = requestedURLs(fetchMock).filter((url) =>
      url.includes(encodeURIComponent("openai/text-embedding-3")),
    );
    expect(embeddingMetricsCalls).toHaveLength(0);
  });
});
