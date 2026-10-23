"use client";

import { useState } from "react";
import type { PricingDetailProps } from "@/components/landing/pricing-view-types";
import type { Copy } from "@/config/copy";
import type { PricingSchedule, PricingScheduleWindow } from "@/lib/pricing-api";
import {
  effectivePrices,
  estimateCost,
  formatContext,
  formatCredits,
  formatMoneyValue,
  formatPerRequest,
  formatRatio,
  formatUSD,
} from "@/lib/pricing-display";

interface RegisterEntry {
  key: string;
  label: string;
  baseCredits: string | null;
  groupCredits: string | null;
  request: boolean;
}

/** Decimal multiplier with two fixed decimals, matching the admin editor. */
function formatMultiplier(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "—";
  return value.toFixed(2);
}

/**
 * "1=Monday" list → localized label, compressing consecutive days into a
 * range ("Mon–Fri"). A cross-night window keeps the day label of its start.
 */
function formatDays(days: number[], weekdays: readonly string[], everyDay: string): string {
  const unique = Array.from(
    new Set(days.filter((day) => Number.isInteger(day) && day >= 1 && day <= 7)),
  ).sort((a, b) => a - b);
  if (unique.length === 0) return "—";
  if (unique.length === 7) return everyDay;
  const runs: Array<[number, number]> = [];
  unique.forEach((day) => {
    const last = runs[runs.length - 1];
    if (last && day === last[1] + 1) {
      last[1] = day;
    } else {
      runs.push([day, day]);
    }
  });
  return runs
    .map(([start, end]) =>
      start === end ? weekdays[start - 1] : `${weekdays[start - 1]}–${weekdays[end - 1]}`,
    )
    .join(", ");
}

/** `end <= start` in zero-padded HH:MM means the window crosses midnight. */
function isOvernight(window: PricingScheduleWindow): boolean {
  return window.end <= window.start;
}

function ScheduleSection({
  schedule,
  scheduleCopy,
}: {
  schedule: PricingSchedule;
  scheduleCopy: Copy["pricingPage"]["schedule"];
}) {
  return (
    <section
      aria-labelledby="pricing-detail-schedule"
      data-testid="pricing-schedule"
      className="border-t border-line pt-4 md:col-span-2 xl:col-span-3"
    >
      <h3
        id="pricing-detail-schedule"
        className="mb-2.5 flex items-center gap-2 text-[12px] font-semibold text-ink"
      >
        {scheduleCopy.title}
        <span className="rounded-[3px] border border-cyan/30 px-1.5 py-px font-mono text-[10px] font-normal text-cyan">
          {scheduleCopy.badge}
        </span>
      </h3>
      <p className="text-[11px] text-faint">
        {scheduleCopy.timezone}:{" "}
        <span className="font-mono text-ink-2">{schedule.timezone}</span>
      </p>
      <ul className="mt-1.5 space-y-1">
        {schedule.windows.map((window, index) => (
          <li
            key={`${window.name}-${index}`}
            data-testid="pricing-schedule-window"
            className="grid grid-cols-[minmax(0,1fr)_auto_auto] items-baseline gap-x-3 border-b border-dashed border-line py-1 text-[11.5px] last:border-b-0"
          >
            <span className="truncate text-ink-2">
              {window.name || scheduleCopy.windowFallback}
            </span>
            <span className="whitespace-nowrap font-mono text-[11px] text-faint">
              {formatDays(window.days, scheduleCopy.weekdays, scheduleCopy.everyDay)}{" "}
              {window.start}–{window.end}
              {isOvernight(window) ? ` (${scheduleCopy.overnight})` : ""}
            </span>
            <span className="font-mono text-[12px] tabular-nums text-ink">
              {scheduleCopy.multiplier.replace("{value}", formatMultiplier(window.multiplier))}
            </span>
          </li>
        ))}
      </ul>
      <dl className="mt-2 grid gap-x-6 gap-y-1 text-[11.5px] sm:grid-cols-2">
        <div className="flex justify-between gap-3">
          <dt className="text-muted">{scheduleCopy.defaultMultiplier}</dt>
          <dd className="font-mono text-ink-2">
            {scheduleCopy.multiplier.replace(
              "{value}",
              formatMultiplier(schedule.default_multiplier),
            )}
          </dd>
        </div>
        <div className="flex justify-between gap-3">
          <dt className="text-muted">{scheduleCopy.holidayLabel}</dt>
          <dd className="text-ink-2">
            {schedule.holiday_policy === "cn_offpeak"
              ? scheduleCopy.holidayCnOffpeak
              : scheduleCopy.holidayNone}
          </dd>
        </div>
      </dl>
      <p className="mt-2 text-[10.5px] leading-relaxed text-faint">
        {scheduleCopy.footnote.replace("{timezone}", schedule.timezone)}
      </p>
    </section>
  );
}

export function PricingDetail({ model, meta, copy, display }: PricingDetailProps) {
  const [inputTokens, setInputTokens] = useState(1_000_000);
  const [outputTokens, setOutputTokens] = useState(200_000);
  const [cacheHitPercent, setCacheHitPercent] = useState(30);
  const [requests, setRequests] = useState(1000);
  const [copied, setCopied] = useState(false);

  const detailCopy = copy.pricingPage.detail;
  const scheduleCopy = copy.pricingPage.schedule;
  const schedule = model.pricing_schedule ?? null;
  const base = effectivePrices(model, "default");
  const group = effectivePrices(model, display.group);
  const showGroup = display.group !== "default";
  const unitLabel = display.unit === "K" ? copy.pricingPage.toolbar.unitK : copy.pricingPage.toolbar.unitM;
  const freeLabel = copy.pricingPage.table.free;
  const dash = copy.pricingPage.table.dash;
  const currency = meta.credit_symbol;

  const isPerRequest = model.mode === "per_request";
  const registerEntries: RegisterEntry[] = isPerRequest
    ? [
        {
          key: "per_request",
          label: detailCopy.perRequest,
          baseCredits: base.input,
          groupCredits: group.input,
          request: true,
        },
      ]
    : [
        { key: "input", label: detailCopy.input, baseCredits: base.input, groupCredits: group.input, request: false },
        {
          key: "output",
          label: detailCopy.output,
          baseCredits: base.output,
          groupCredits: group.output,
          request: false,
        },
        {
          key: "cache_read",
          label: detailCopy.cacheRead,
          baseCredits: base.cacheRead,
          groupCredits: group.cacheRead,
          request: false,
        },
        {
          key: "cache_write",
          label: detailCopy.cacheWrite,
          baseCredits: base.cacheWrite,
          groupCredits: group.cacheWrite,
          request: false,
        },
      ];

  const estimate = estimateCost(model, meta, {
    group: display.group,
    unit: display.unit,
    inputTokens,
    outputTokens,
    cacheHitPercent,
    requests,
  });
  const estimateUSD =
    meta.usd_exchange_rate > 0 ? estimate.total / meta.usd_exchange_rate : null;
  const perCallPrice = isPerRequest ? group.input : null;

  const groupTag =
    showGroup && group.ratio !== null
      ? `${detailCopy.group} ${formatRatio(group.ratio)}`
      : showGroup
        ? detailCopy.group
        : detailCopy.base;

  function registerValue(credits: string | null, request: boolean): string {
    if (model.is_free) return freeLabel;
    if (credits === null) return dash;
    return request
      ? `${formatPerRequest(credits, meta)} ${copy.pricingPage.table.perCall}`
      : formatCredits(credits, meta, display.unit);
  }

  function usdReference(credits: string | null, request: boolean): string {
    if (model.is_free || credits === null) return "";
    return formatUSD(credits, meta, request ? "M" : display.unit);
  }

  async function copyModelID() {
    try {
      await navigator.clipboard?.writeText(model.model_id);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  }

  return (
    <div
      data-testid="pricing-detail"
      className="grid grid-cols-1 gap-x-7 gap-y-6 border-t border-line px-6 pb-6 pt-5 md:grid-cols-2 xl:grid-cols-[minmax(0,1.05fr)_minmax(0,1fr)_minmax(0,0.95fr)]"
    >
      <section aria-labelledby="pricing-detail-register">
        <h3 id="pricing-detail-register" className="mb-3 flex items-center gap-2 text-[12px] font-semibold text-ink">
          {detailCopy.register}
          <span className="font-mono text-[10px] font-normal text-faint">{groupTag}</span>
        </h3>
        <dl className="space-y-0">
          {registerEntries.map((entry) => {
            const shownCredits = showGroup ? entry.groupCredits : entry.baseCredits;
            return (
              <div
                key={entry.key}
                className="grid grid-cols-[72px_minmax(0,1fr)_auto] items-baseline gap-2.5 border-b border-dashed border-line py-1.5 text-[12px] last:border-b-0"
              >
                <dt className="text-muted">{entry.label}</dt>
                <dd className="text-right font-mono text-[13px] text-ink">
                  {showGroup && !model.is_free ? (
                    <>
                      <s className="mr-1.5 text-[10.5px] text-faint">
                        {registerValue(entry.baseCredits, entry.request)}
                      </s>
                      <span className="text-cyan">{registerValue(entry.groupCredits, entry.request)}</span>
                    </>
                  ) : (
                    registerValue(shownCredits, entry.request)
                  )}
                  {display.dual && usdReference(shownCredits, entry.request) !== "" ? (
                    <span className="ml-1.5 text-[10px] text-faint">
                      {usdReference(shownCredits, entry.request)}
                    </span>
                  ) : null}
                </dd>
                <span className="text-[10px] text-faint">
                  {entry.request ? copy.pricingPage.table.perCall : unitLabel}
                </span>
              </div>
            );
          })}
        </dl>
        <p className="mt-2.5 text-[10.5px] leading-relaxed text-faint">{detailCopy.rechargeNote}</p>
      </section>

      <section aria-labelledby="pricing-detail-estimator" className="flex flex-col gap-2.5">
        <h3
          id="pricing-detail-estimator"
          className="mb-0.5 flex items-center gap-2 text-[12px] font-semibold text-ink"
        >
          {detailCopy.estimator}
          <span className="font-mono text-[10px] font-normal text-faint">{groupTag}</span>
        </h3>
        <div className="grid grid-cols-2 gap-2">
          {isPerRequest ? (
            <label className="flex flex-col gap-1 text-[10.5px] text-muted">
              {detailCopy.requests}
              <input
                type="number"
                min={0}
                step={1}
                value={requests}
                onChange={(event) => setRequests(Math.max(0, Number(event.target.value) || 0))}
                className="h-8 rounded-[5px] border border-line-2 bg-panel px-2 font-mono text-[12px] text-ink"
              />
            </label>
          ) : (
            <>
              <label className="flex flex-col gap-1 text-[10.5px] text-muted">
                {detailCopy.inputTokens}
                <input
                  type="number"
                  min={0}
                  step={1000}
                  value={inputTokens}
                  onChange={(event) =>
                    setInputTokens(Math.max(0, Number(event.target.value) || 0))
                  }
                  className="h-8 rounded-[5px] border border-line-2 bg-panel px-2 font-mono text-[12px] text-ink"
                />
              </label>
              <label className="flex flex-col gap-1 text-[10.5px] text-muted">
                {detailCopy.outputTokens}
                <input
                  type="number"
                  min={0}
                  step={1000}
                  value={outputTokens}
                  onChange={(event) =>
                    setOutputTokens(Math.max(0, Number(event.target.value) || 0))
                  }
                  className="h-8 rounded-[5px] border border-line-2 bg-panel px-2 font-mono text-[12px] text-ink"
                />
              </label>
              <label className="flex flex-col gap-1 text-[10.5px] text-muted">
                {detailCopy.cacheHit}
                <input
                  type="number"
                  min={0}
                  max={100}
                  step={1}
                  value={cacheHitPercent}
                  onChange={(event) =>
                    setCacheHitPercent(
                      Math.min(100, Math.max(0, Number(event.target.value) || 0)),
                    )
                  }
                  className="h-8 rounded-[5px] border border-line-2 bg-panel px-2 font-mono text-[12px] text-ink"
                />
              </label>
            </>
          )}
        </div>

        <div className="rounded-[7px] border border-line-2 bg-panel px-3.5 py-3">
          <div className="flex items-baseline justify-between gap-3">
            <span className="text-[11.5px] text-muted">{detailCopy.estimated}</span>
            <strong className="font-mono text-[22px] font-medium tracking-[-0.02em] text-ink">
              {currency}
              {formatMoneyValue(estimate.total)}
            </strong>
          </div>
          {estimateUSD !== null ? (
            <p className="mt-0.5 text-right font-mono text-[11px] text-faint">
              ≈ ${formatMoneyValue(estimateUSD)}
            </p>
          ) : null}
          <dl className="mt-2 grid gap-0.5 text-[11px]">
            {isPerRequest ? (
              <>
                <div className="flex justify-between gap-3">
                  <dt className="text-muted">{detailCopy.perRequestCost}</dt>
                  <dd className="font-mono text-ink-2">
                    {perCallPrice === null
                      ? dash
                      : `${formatPerRequest(perCallPrice, meta)} ${copy.pricingPage.table.perCall}`}
                  </dd>
                </div>
                <div className="flex justify-between gap-3">
                  <dt className="text-muted">{detailCopy.requests}</dt>
                  <dd className="font-mono text-ink-2">{requests.toLocaleString()}</dd>
                </div>
              </>
            ) : (
              <>
                <div className="flex justify-between gap-3">
                  <dt className="text-muted">{detailCopy.inputCost}</dt>
                  <dd className="font-mono text-ink-2">
                    {currency}
                    {formatMoneyValue(estimate.inputCost)}
                  </dd>
                </div>
                <div className="flex justify-between gap-3">
                  <dt className="text-muted">{detailCopy.cacheCost}</dt>
                  <dd className="font-mono text-ink-2">
                    {currency}
                    {formatMoneyValue(estimate.cacheCost)}
                  </dd>
                </div>
                <div className="flex justify-between gap-3">
                  <dt className="text-muted">{detailCopy.outputCost}</dt>
                  <dd className="font-mono text-ink-2">
                    {currency}
                    {formatMoneyValue(estimate.outputCost)}
                  </dd>
                </div>
              </>
            )}
          </dl>
        </div>
        <p className="text-[10.5px] leading-relaxed text-faint">{detailCopy.estimatorNote}</p>
      </section>

      <section aria-labelledby="pricing-detail-info" className="md:col-span-2 xl:col-span-1">
        <h3 id="pricing-detail-info" className="mb-3 text-[12px] font-semibold text-ink">
          {detailCopy.info}
        </h3>
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-[12px]">
          <dt className="text-muted">{copy.pricingPage.table.context}</dt>
          <dd className="text-ink-2">{formatContext(model.context_length)}</dd>
          <dt className="text-muted">{copy.pricingPage.cards.maxOutput}</dt>
          <dd className="text-ink-2">{formatContext(model.max_output)}</dd>
          <dt className="text-muted">{detailCopy.mode}</dt>
          <dd className="text-ink-2">
            {isPerRequest ? copy.pricingPage.modeRequest : copy.pricingPage.modeToken}
          </dd>
          <dt className="text-muted">{copy.pricingPage.table.vendor}</dt>
          <dd className="text-ink-2">{model.vendor}</dd>
        </dl>

        {model.capabilities !== null && model.capabilities.length > 0
          ? renderTags("capabilities", model.capabilities)
          : null}
        {model.endpoints !== null && model.endpoints.length > 0
          ? renderTags("endpoints", model.endpoints)
          : null}

        {model.description !== null && model.description !== "" ? (
          <p className="mt-3 text-[11.5px] leading-relaxed text-muted">{model.description}</p>
        ) : null}

        <div className="mt-3.5 flex flex-wrap items-center gap-2">
          <button
            type="button"
            onClick={copyModelID}
            className="h-[30px] rounded-[5px] border border-line-2 px-2.5 text-[11.5px] text-ink-2 transition-colors hover:border-cyan-dim hover:text-ink"
          >
            {detailCopy.copyId}
          </button>
          <span aria-live="polite" className="sr-only">
            {copied ? detailCopy.copied : ""}
          </span>
          <a
            href={`/pricing/${encodeURIComponent(model.model_id)}`}
            className="text-[11.5px] text-cyan transition-colors hover:text-cyan-soft"
          >
            {detailCopy.viewDetail}
          </a>
        </div>
      </section>

      {schedule ? <ScheduleSection schedule={schedule} scheduleCopy={scheduleCopy} /> : null}
    </div>
  );

  function renderTags(key: string, values: string[]) {
    return (
      <div key={key} className="mt-3 flex flex-wrap gap-1.5">
        {values.map((value) => (
          <span
            key={`${key}-${value}`}
            className="inline-flex h-5 items-center rounded-[3px] border border-line-2 px-1.5 text-[10px] text-muted"
          >
            {value}
          </span>
        ))}
      </div>
    );
  }
}
