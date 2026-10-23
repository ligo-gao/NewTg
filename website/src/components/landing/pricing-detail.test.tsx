import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { PricingDetail } from "@/components/landing/pricing-detail";
import type { PricingDisplay } from "@/components/landing/pricing-view-types";
import { getCopy } from "@/config/copy";
import type { PricingMeta, PricingModel, PricingSchedule } from "@/lib/pricing-api";

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
    description: "测试用模型。",
    capabilities: ["vision", "tools"],
    tags: ["热门"],
    endpoints: ["chat"],
    group_price: null,
    ...overrides,
  };
}

function display(overrides: Partial<PricingDisplay> = {}): PricingDisplay {
  return { unit: "M", dual: false, group: "default", ...overrides };
}

const schedule: PricingSchedule = {
  kind: "peak_offpeak",
  timezone: "Asia/Shanghai",
  default_multiplier: 0.5,
  holiday_policy: "cn_offpeak",
  windows: [
    { name: "上午高峰", days: [1, 2, 3, 4, 5], start: "09:00", end: "12:00", multiplier: 1 },
    { name: "夜间低谷", days: [6], start: "22:00", end: "08:00", multiplier: 0.6 },
  ],
};

describe("PricingDetail", () => {
  it("renders the full register and the default cost estimate", () => {
    render(<PricingDetail model={model()} meta={meta} copy={copy} display={display()} />);

    // Register: input ¥2.50, output ¥10.00, cache read ¥1.25, cache write ¥2.50.
    expect(screen.getAllByText("¥2.50")).toHaveLength(2);
    expect(screen.getByText("¥10.00")).toBeInTheDocument();
    expect(screen.getByText("¥1.25")).toBeInTheDocument();

    // 1M input at 30% cache hit + 200K output:
    // 0.7 × ¥2.50 + 0.3 × ¥1.25 + 0.2 × ¥10.00 = ¥4.125 → ¥4.13.
    expect(screen.getByText("¥4.13")).toBeInTheDocument();
    expect(screen.getByText("≈ $0.58")).toBeInTheDocument();
    expect(screen.getByText("¥1.75")).toBeInTheDocument();
    expect(screen.getByText("¥0.38")).toBeInTheDocument();
    expect(screen.getByText("¥2.00")).toBeInTheDocument();

    // Model info.
    expect(screen.getByText("128K")).toBeInTheDocument();
    expect(screen.getByText("16.4K")).toBeInTheDocument();
    expect(screen.getByText(copy.pricingPage.modeToken)).toBeInTheDocument();
    expect(screen.getByText("OpenAI")).toBeInTheDocument();
    expect(screen.getByText("vision")).toBeInTheDocument();
    expect(screen.getByText("chat")).toBeInTheDocument();

    const detailLink = screen.getByRole("link", {
      name: copy.pricingPage.detail.viewDetail,
    });
    expect(detailLink).toHaveAttribute("href", "/pricing/openai%2Fgpt-4o");
  });

  it("recalculates the estimate when output tokens change", async () => {
    const user = userEvent.setup();
    render(<PricingDetail model={model()} meta={meta} copy={copy} display={display()} />);

    expect(screen.getByText("¥4.13")).toBeInTheDocument();

    const output = screen.getByLabelText(copy.pricingPage.detail.outputTokens);
    await user.clear(output);
    await user.type(output, "400000");

    // 0.7 × ¥2.50 + 0.3 × ¥1.25 + 0.4 × ¥10.00 = ¥6.125 → ¥6.13.
    expect(screen.getByText("¥6.13")).toBeInTheDocument();
    expect(screen.queryByText("¥4.13")).not.toBeInTheDocument();
  });

  it("shows the server group price and strikes the base price", () => {
    const grouped = model({
      group_price: {
        group: "vip",
        ratio: 120,
        prompt: "300",
        completion: "1200",
        cache_read: "150",
        cache_write: "300",
        source: "ratio",
      },
    });

    render(
      <PricingDetail
        model={grouped}
        meta={meta}
        copy={copy}
        display={display({ group: "vip" })}
      />,
    );

    // Group values: input / cache write ¥3.00, output ¥12.00, cache read ¥1.50.
    expect(screen.getAllByText("¥3.00")).toHaveLength(2);
    expect(screen.getByText("¥12.00")).toBeInTheDocument();
    expect(screen.getByText("¥1.50")).toBeInTheDocument();

    // The base value is rendered struck through next to the group value.
    const struckBase = screen
      .getAllByText("¥2.50")
      .filter((node) => node.closest("s") !== null);
    expect(struckBase.length).toBeGreaterThan(0);
    const struckOutput = screen
      .getAllByText("¥10.00")
      .filter((node) => node.closest("s") !== null);
    expect(struckOutput.length).toBeGreaterThan(0);

    expect(screen.getAllByText("分组价 ×1.20")).toHaveLength(2);
    // Group estimator total: 2.1 + 0.45 + 2.4 = ¥4.95.
    expect(screen.getByText("¥4.95")).toBeInTheDocument();
  });

  it("renders the published time-of-day schedule with localized labels", () => {
    render(
      <PricingDetail
        model={model({ pricing_schedule: schedule })}
        meta={meta}
        copy={copy}
        display={display()}
      />,
    );

    const section = screen.getByTestId("pricing-schedule");
    expect(within(section).getByText(copy.pricingPage.schedule.title)).toBeInTheDocument();
    expect(within(section).getByText("Asia/Shanghai")).toBeInTheDocument();

    // Window 1: weekday peak, ×1.00.
    expect(within(section).getByText("上午高峰")).toBeInTheDocument();
    expect(within(section).getByText(/周一–周五/)).toBeInTheDocument();
    expect(within(section).getByText(/09:00–12:00/)).toBeInTheDocument();
    expect(within(section).getByText("×1.00")).toBeInTheDocument();

    // Window 2: overnight on Saturday, flagged as ending the next day.
    expect(within(section).getByText("夜间低谷")).toBeInTheDocument();
    const overnight = within(section).getByText(/22:00–08:00/);
    expect(overnight).toHaveTextContent(copy.pricingPage.schedule.overnight);
    expect(within(section).getByText("×0.60")).toBeInTheDocument();

    // Off-peak default and the cn_offpeak holiday policy.
    expect(within(section).getByText("×0.50")).toBeInTheDocument();
    expect(
      within(section).getByText(copy.pricingPage.schedule.holidayCnOffpeak),
    ).toBeInTheDocument();
  });

  it("leaves the detail unchanged when no schedule is published", () => {
    render(<PricingDetail model={model()} meta={meta} copy={copy} display={display()} />);

    expect(screen.queryByTestId("pricing-schedule")).not.toBeInTheDocument();
    expect(screen.queryByText(copy.pricingPage.schedule.badge)).not.toBeInTheDocument();
    // The existing register and estimator are still intact.
    expect(screen.getByText("¥4.13")).toBeInTheDocument();
  });

  it("estimates per-request models by number of calls", () => {    const perRequest = model({
      model_id: "openai/dall-e-3",
      display_name: "DALL·E 3",
      mode: "per_request",
      prompt_price: "4",
      completion_price: null,
      cache_read_price: null,
      cache_write_price: null,
      context_length: null,
      max_output: null,
    });

    render(<PricingDetail model={perRequest} meta={meta} copy={copy} display={display()} />);

    expect(screen.getAllByText("¥0.04 / 次")).toHaveLength(2);
    // 1000 default requests × ¥0.04 = ¥40.00.
    expect(screen.getByText("¥40.00")).toBeInTheDocument();
    expect(screen.getByLabelText(copy.pricingPage.detail.requests)).toBeInTheDocument();
  });
});
