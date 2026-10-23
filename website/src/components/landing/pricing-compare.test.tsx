import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { PricingCompareDialog } from "@/components/landing/pricing-compare";
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
    capabilities: [],
    tags: [],
    endpoints: [],
    group_price: null,
    ...overrides,
  };
}

const display: PricingDisplay = { unit: "M", dual: false, group: "default" };

const gpt = model();
const claude = model({
  model_id: "anthropic/claude-sonnet-4-5",
  display_name: "Claude Sonnet 4.5",
  vendor: "Anthropic",
  context_length: 200000,
  max_output: 64000,
  prompt_price: "300",
  completion_price: "1500",
  cache_read_price: "30",
  cache_write_price: "75",
});

function renderDialog(
  models: PricingModel[],
  options: {
    open?: boolean;
    onOpenChange?: (open: boolean) => void;
    onClear?: () => void;
  } = {},
) {
  return render(
    <PricingCompareDialog
      models={models}
      meta={meta}
      copy={copy}
      display={display}
      open={options.open ?? true}
      onOpenChange={options.onOpenChange ?? vi.fn()}
      onClear={options.onClear ?? vi.fn()}
    />,
  );
}

describe("PricingCompareDialog", () => {
  it("opens with the selected models and every comparison row", () => {
    renderDialog([gpt, claude]);

    const dialog = screen.getByRole("dialog", { name: copy.pricingPage.compare.title });
    expect((dialog as HTMLDialogElement).open).toBe(true);

    expect(
      within(dialog).getByRole("columnheader", { name: "GPT-4o" }),
    ).toBeInTheDocument();
    expect(
      within(dialog).getByRole("columnheader", { name: "Claude Sonnet 4.5" }),
    ).toBeInTheDocument();
    expect(
      within(dialog).getByRole("columnheader", { name: copy.pricingPage.compare.indicator }),
    ).toBeInTheDocument();

    const rows = [
      copy.pricingPage.compare.context,
      copy.pricingPage.compare.maxOutput,
      copy.pricingPage.compare.mode,
      copy.pricingPage.compare.input,
      copy.pricingPage.compare.output,
      copy.pricingPage.compare.cacheRead,
      copy.pricingPage.compare.cacheWrite,
      copy.pricingPage.compare.perRequest,
      copy.pricingPage.compare.group,
    ];
    rows.forEach((label) => {
      expect(within(dialog).getByRole("rowheader", { name: label })).toBeInTheDocument();
    });

    // Current group (default) prices are used: GPT-4o input ¥2.50, Claude input ¥3.00.
    expect(dialog).toHaveTextContent("¥2.50");
    expect(dialog).toHaveTextContent("¥10.00");
    expect(dialog).toHaveTextContent("¥3.00");
    expect(dialog).toHaveTextContent("128K");
    expect(dialog).toHaveTextContent("200K");
  });

  it("closes through the close button", async () => {
    const user = userEvent.setup();
    const onOpenChange = vi.fn();
    renderDialog([gpt, claude], { onOpenChange });

    await user.click(
      screen.getByRole("button", { name: copy.pricingPage.compare.close }),
    );
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("clears the selection and closes", async () => {
    const user = userEvent.setup();
    const onOpenChange = vi.fn();
    const onClear = vi.fn();
    renderDialog([gpt, claude], { onOpenChange, onClear });

    await user.click(screen.getByRole("button", { name: copy.pricingPage.compare.clear }));
    expect(onClear).toHaveBeenCalledTimes(1);
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("renders nothing with fewer than two models", () => {
    renderDialog([gpt]);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("uses the selected group prices and the group ratio row", () => {
    const grouped = {
      ...gpt,
      group_price: {
        group: "vip" as const,
        ratio: 120,
        prompt: "300",
        completion: "1200",
        cache_read: "150",
        cache_write: "300",
        source: "ratio" as const,
      },
    };

    render(
      <PricingCompareDialog
        models={[grouped, claude]}
        meta={meta}
        copy={copy}
        display={{ unit: "M", dual: false, group: "vip" }}
        open
        onOpenChange={vi.fn()}
        onClear={vi.fn()}
      />,
    );

    const dialog = screen.getByRole("dialog", { name: copy.pricingPage.compare.title });
    expect(dialog).toHaveTextContent("¥3.00");
    expect(dialog).toHaveTextContent("¥12.00");
    expect(dialog).toHaveTextContent("×1.20");
  });
});
