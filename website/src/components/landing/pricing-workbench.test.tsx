import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { PricingWorkbench } from "@/components/landing/pricing-workbench";
import { getCopy } from "@/config/copy";
import {
  fetchPricingPage,
  type PricingCatalog,
  type PricingModel,
} from "@/lib/pricing-api";

vi.mock("@/lib/pricing-api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/pricing-api")>();
  return { ...actual, fetchPricingPage: vi.fn() };
});

const fetchMock = vi.mocked(fetchPricingPage);

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
    cache_read_price: "125",
    cache_write_price: "250",
    is_free: false,
    mode: "per_token",
    description: null,
    capabilities: ["vision", "tools"],
    tags: [],
    endpoints: ["chat"],
    group_price: null,
    ...overrides,
  };
}

const catalog: PricingCatalog = {
  items: [
    model("openai/gpt-4o", "GPT-4o", "OpenAI", { tags: ["热门"] }),
    model("anthropic/claude-sonnet-4-5", "Claude Sonnet 4.5", "Anthropic", {
      prompt_price: "300",
      completion_price: "1500",
      cache_read_price: "30",
      cache_write_price: "75",
      context_length: 200000,
      capabilities: ["vision"],
    }),
    model("deepseek/deepseek-chat", "DeepSeek-V3.2", "DeepSeek", {
      prompt_price: "28",
      completion_price: "42",
      cache_read_price: null,
      cache_write_price: null,
      capabilities: ["tools"],
    }),
    model("openai/dall-e-3", "DALL·E 3", "OpenAI", {
      mode: "per_request",
      prompt_price: "4",
      completion_price: null,
      cache_read_price: null,
      cache_write_price: null,
      context_length: null,
      max_output: null,
      capabilities: ["image"],
      endpoints: ["images"],
      tags: ["按次"],
    }),
  ],
  total: "30",
  page: 1,
  page_size: 24,
  facets: {
    vendors: [
      { value: "openai", label: "OpenAI", count: 2 },
      { value: "anthropic", label: "Anthropic", count: 1 },
      { value: "deepseek", label: "DeepSeek", count: 1 },
    ],
    capabilities: [
      { value: "vision", label: "vision", count: 2 },
      { value: "tools", label: "tools", count: 3 },
      { value: "image", label: "image", count: 1 },
    ],
    context: [
      { value: "128", label: "≤ 128K", count: 2 },
      { value: "200", label: "200K", count: 1 },
      { value: "1m", label: "≥ 1M", count: 1 },
    ],
    groups: [
      { value: "default", label: "default", ratio: 100, count: 4 },
      { value: "vip", label: "vip", ratio: 120, count: 4 },
    ],
    endpoints: [
      { value: "chat", label: "Chat Completions", count: 3 },
      { value: "images", label: "Images", count: 1 },
    ],
    pricing_types: [
      { value: "per_token", count: 3 },
      { value: "per_request", count: 1 },
    ],
  },
  meta,
};

function renderWorkbench() {
  return render(<PricingWorkbench copy={getCopy("zh-CN")} locale="zh-CN" />);
}

beforeEach(() => {
  window.history.replaceState(null, "", "/");
  fetchMock.mockReset();
  fetchMock.mockResolvedValue(catalog);
});

describe("PricingWorkbench", () => {
  it("renders the live catalog table after loading", async () => {
    renderWorkbench();

    expect(await screen.findByText("GPT-4o")).toBeInTheDocument();
    expect(screen.getByText("Claude Sonnet 4.5")).toBeInTheDocument();
    expect(screen.getByText("DeepSeek-V3.2")).toBeInTheDocument();
    expect(screen.getByText("DALL·E 3")).toBeInTheDocument();
    expect(screen.getAllByText("¥2.50").length).toBeGreaterThan(0);
    expect(screen.getByText("个模型")).toBeInTheDocument();
    expect(screen.getByText("60s")).toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("collapses the secondary pricing rules behind a closed summary", async () => {
    renderWorkbench();
    await screen.findByText("GPT-4o");

    const primaryLine = "价格为每 1M tokens，可切换 1K 视图";
    expect(screen.getByText(primaryLine)).toBeInTheDocument();

    const summary = screen.getByText("计价说明");
    const details = summary.closest("details");
    expect(details).not.toBeNull();
    if (!details) throw new Error("the pricing legend disclosure did not render");

    expect(details).not.toHaveAttribute("open");
    expect(within(details).queryByText(primaryLine)).not.toBeInTheDocument();
    expect(within(details).getByText("100 积分 = ¥1 · 美元参考按汇率换算")).toBeInTheDocument();
    expect(within(details).getByText("null 显示 —，0 显示「免费」")).toBeInTheDocument();
    expect(within(details).getByText("分组价 = 基准价 × 分组倍率")).toBeInTheDocument();
  });

  it("debounces search and sends the term to the API", async () => {
    const user = userEvent.setup();
    renderWorkbench();
    await screen.findByText("GPT-4o");

    await user.type(screen.getByRole("searchbox"), "gpt");

    await waitFor(
      () => {
        expect(fetchMock).toHaveBeenLastCalledWith(
          expect.objectContaining({ search: "gpt", page: 1 }),
          expect.anything(),
        );
      },
      { timeout: 2000 },
    );
    expect(window.location.search).toContain("search=gpt");
  });

  it("filters by vendor from the sticky rail", async () => {
    const user = userEvent.setup();
    renderWorkbench();
    await screen.findByText("GPT-4o");

    await user.click(screen.getByRole("checkbox", { name: "OpenAI" }));

    await waitFor(() => {
      expect(fetchMock).toHaveBeenLastCalledWith(
        expect.objectContaining({ vendors: ["openai"], page: 1 }),
        expect.anything(),
      );
    });
    expect(window.location.search).toContain("vendor=openai");
  });

  it("switches the displayed prices to per-1K without refetching", async () => {
    const user = userEvent.setup();
    renderWorkbench();
    await screen.findByText("GPT-4o");
    expect(screen.getAllByText("¥2.50").length).toBeGreaterThan(0);

    await user.click(screen.getByRole("button", { name: "/ 1K" }));

    expect(screen.getAllByText("¥0.0025").length).toBeGreaterThan(0);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(window.location.search).toContain("tokenUnit=K");
  });

  it("expands a row into the shared PricingDetail panel", async () => {
    const user = userEvent.setup();
    renderWorkbench();

    expect(screen.queryByTestId("pricing-detail")).not.toBeInTheDocument();
    const expandButtons = await screen.findAllByRole("button", { name: "展开完整价目" });
    await user.click(expandButtons[0]);

    expect(await screen.findByTestId("pricing-detail")).toBeInTheDocument();
    expect(expandButtons[0]).toHaveAttribute("aria-expanded", "true");

    await user.click(expandButtons[0]);
    expect(screen.queryByTestId("pricing-detail")).not.toBeInTheDocument();
  });

  it("shows the compare bar once two models are selected", async () => {
    const user = userEvent.setup();
    renderWorkbench();
    await screen.findByText("GPT-4o");

    await user.click(screen.getByRole("checkbox", { name: "对比 GPT-4o" }));
    await user.click(screen.getByRole("checkbox", { name: "对比 Claude Sonnet 4.5" }));

    const bar = screen.getByRole("region", { name: "模型对比" });
    expect(bar).toHaveTextContent("已选");
    expect(bar).toHaveTextContent("2");
    expect(bar).toHaveTextContent("最多 3 个");
  });

  it("pages through results using the server total", async () => {
    const user = userEvent.setup();
    renderWorkbench();
    await screen.findByText("GPT-4o");
    expect(screen.getByText("1–24 / 30")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "下一页" }));

    await waitFor(() => {
      expect(fetchMock).toHaveBeenLastCalledWith(
        expect.objectContaining({ page: 2 }),
        expect.anything(),
      );
    });
    expect(window.location.search).toContain("p=2");
  });

  it("hydrates filters and display options from the URL", async () => {
    window.history.replaceState(
      null,
      "",
      "/pricing?search=claude&vendor=openai&p=2&tokenUnit=K&dualCurrency=1",
    );
    renderWorkbench();

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        expect.objectContaining({
          search: "claude",
          vendors: ["openai"],
          page: 2,
        }),
        expect.anything(),
      );
    });
    expect(screen.getByRole("checkbox", { name: "OpenAI" })).toBeChecked();
    expect(screen.getByRole("button", { name: "/ 1K" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
  });
});
