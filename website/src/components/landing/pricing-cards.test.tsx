import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { PricingCard } from "@/components/landing/pricing-cards";
import type { PricingDisplay } from "@/components/landing/pricing-view-types";
import { getCopy } from "@/config/copy";
import type { PricingMeta, PricingModel } from "@/lib/pricing-api";

const copy = getCopy("zh-CN");

const meta: PricingMeta = {
  credit_symbol: "¥",
  quota_per_unit: 100,
  usd_exchange_rate: 7.1,
  version: "test",
  generated_at: "2026-10-23T00:00:00Z",
};

function model(overrides: Partial<PricingModel> = {}): PricingModel {
  return {
    model_id: "openai/gpt-4o",
    display_name: "GPT-4o",
    vendor: "OpenAI",
    context_length: 128000,
    max_output: 16384,
    prompt_price: "250",
    completion_price: "1000",
    cache_read_price: "125",
    cache_write_price: "250",
    is_free: false,
    mode: "per_token",
    description: null,
    capabilities: ["vision"],
    tags: ["热门"],
    endpoints: ["chat"],
    group_price: null,
    ...overrides,
  };
}

function display(overrides: Partial<PricingDisplay> = {}): PricingDisplay {
  return { unit: "M", dual: false, group: "default", ...overrides };
}

function renderCard(
  target: PricingModel,
  options: {
    display?: PricingDisplay;
    selected?: boolean;
    compareDisabled?: boolean;
    onToggleCompare?: (id: string, checked: boolean) => void;
    onOpenDetail?: (model: PricingModel) => void;
  } = {},
) {
  return render(
    <PricingCard
      model={target}
      meta={meta}
      copy={copy}
      display={options.display ?? display()}
      selected={options.selected ?? false}
      compareDisabled={options.compareDisabled ?? false}
      onToggleCompare={options.onToggleCompare ?? vi.fn()}
      onOpenDetail={options.onOpenDetail ?? vi.fn()}
    />,
  );
}

describe("PricingCard", () => {
  it("marks a free model as free in the price grid", () => {
    renderCard(
      model({
        model_id: "meta/llama-3-2-1b",
        display_name: "Llama 3.2 1B",
        prompt_price: "0",
        completion_price: "0",
        cache_read_price: null,
        cache_write_price: null,
        is_free: true,
        tags: [copy.pricingPage.table.free],
      }),
    );

    const card = screen.getByTestId("pricing-card");
    expect(within(card).getAllByText(copy.pricingPage.table.free).length).toBeGreaterThan(0);
  });

  it("renders per-request pricing with a per-call suffix", () => {
    renderCard(
      model({
        model_id: "openai/dall-e-3",
        display_name: "DALL·E 3",
        mode: "per_request",
        prompt_price: "4",
        completion_price: null,
        cache_read_price: null,
        cache_write_price: null,
      }),
    );

    const card = screen.getByTestId("pricing-card");
    expect(within(card).getByText("¥0.04 / 次")).toBeInTheDocument();
    expect(card).toHaveTextContent(copy.pricingPage.filters.perRequest);
  });

  it("shows a group line when a non-default group is selected", () => {
    renderCard(
      model({
        group_price: {
          group: "vip",
          ratio: 120,
          prompt: "300",
          completion: "1200",
          cache_read: "150",
          cache_write: "300",
          source: "ratio",
        },
      }),
      { display: display({ group: "vip" }) },
    );

    const card = screen.getByTestId("pricing-card");
    expect(card).toHaveTextContent(copy.pricingPage.detail.group);
    expect(card).toHaveTextContent("¥3.00");
    expect(card).toHaveTextContent("¥12.00");
  });

  it("wires the compare checkbox and the detail button", async () => {
    const user = userEvent.setup();
    const onToggleCompare = vi.fn();
    const onOpenDetail = vi.fn();
    const target = model();

    renderCard(target, { onToggleCompare, onOpenDetail });

    const card = screen.getByTestId("pricing-card");
    await user.click(within(card).getByRole("checkbox", { name: "对比 GPT-4o" }));
    expect(onToggleCompare).toHaveBeenCalledWith("openai/gpt-4o", true);

    await user.click(within(card).getByRole("button", { name: copy.pricingPage.cards.detail }));
    expect(onOpenDetail).toHaveBeenCalledWith(target);
  });

  it("disables the compare checkbox when the limit is reached", () => {
    renderCard(model(), { compareDisabled: true });
    expect(screen.getByRole("checkbox", { name: "对比 GPT-4o" })).toBeDisabled();
  });
});
