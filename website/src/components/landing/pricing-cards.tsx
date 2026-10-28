"use client";

import type { PricingCardProps } from "@/components/landing/pricing-view-types";
import {
  effectivePrices,
  formatContext,
  formatCredits,
  formatPerRequest,
} from "@/lib/pricing-display";

export function PricingCard({
  model,
  meta,
  copy,
  display,
  selected,
  compareDisabled,
  onToggleCompare,
  onOpenDetail,
}: PricingCardProps) {
  const displayName = model.display_name || model.model_id;
  const base = effectivePrices(model, "default");
  const group = effectivePrices(model, display.group);
  const isGrouped = display.group !== "default";
  const isPerRequest = model.mode === "per_request";
  const unitLabel = display.unit === "K" ? copy.pricingPage.toolbar.unitK : copy.pricingPage.toolbar.unitM;
  const freeLabel = copy.pricingPage.table.free;
  const dash = copy.pricingPage.table.dash;
  const perCall = copy.pricingPage.table.perCall;

  function tokenValue(credits: string | null): string {
    if (model.is_free) return freeLabel;
    if (credits === null) return dash;
    return formatCredits(credits, meta, display.unit);
  }

  function requestValue(credits: string | null): string {
    if (model.is_free) return freeLabel;
    if (credits === null) return dash;
    return `${formatPerRequest(credits, meta)} ${perCall}`;
  }

  const priceCells = isPerRequest
    ? [{ key: "per_request", label: copy.pricingPage.table.perRequest, value: requestValue(base.input) }]
    : [
        {
          key: "input",
          label: `${copy.pricingPage.table.input} ${unitLabel}`,
          value: tokenValue(base.input),
        },
        {
          key: "output",
          label: `${copy.pricingPage.table.output} ${unitLabel}`,
          value: tokenValue(base.output),
        },
        { key: "cache_read", label: copy.pricingPage.table.cacheRead, value: tokenValue(base.cacheRead) },
        {
          key: "cache_write",
          label: copy.pricingPage.table.cacheWrite,
          value: tokenValue(base.cacheWrite),
        },
      ];

  const tags = (model.tags ?? []).filter(
    (tag) => tag !== freeLabel && tag !== copy.pricingPage.filters.perRequest,
  );

  return (
    <article
      data-testid="pricing-card"
      className="flex flex-col gap-3 rounded-[10px] border border-line bg-linear-to-b from-panel to-bg-2 p-4 transition-[border-color,transform] duration-200 hover:-translate-y-0.5 hover:border-line-2"
    >
      <div className="flex items-start justify-between gap-2.5">
        <div className="flex min-w-0 items-start gap-2">
          <span aria-hidden="true" className="mt-[7px] size-1.5 shrink-0 rounded-full bg-cyan" />
          <div className="min-w-0">
            <h3 className="truncate font-mono text-public-body font-medium text-ink">{displayName}</h3>
            {model.display_name && model.display_name !== model.model_id ? (
              <span className="mt-px block truncate font-mono text-public-caption text-faint">
                {model.model_id}
              </span>
            ) : null}
            <p className="mt-1 truncate text-public-caption text-faint">{model.vendor}</p>
          </div>
        </div>
        <div className="flex shrink-0 flex-wrap justify-end gap-1">
          {model.is_free ? (
            <span className="rounded-[3px] border border-green/35 px-1 py-px text-public-caption text-green">
              {freeLabel}
            </span>
          ) : null}
          {isPerRequest ? (
            <span className="rounded-[3px] border border-cyan/30 px-1 py-px text-public-caption text-cyan">
              {copy.pricingPage.filters.perRequest}
            </span>
          ) : null}
          {tags.map((tag) => (
            <span
              key={tag}
              className="rounded-[3px] border border-line-2 px-1 py-px text-public-caption text-muted"
            >
              {tag}
            </span>
          ))}
        </div>
      </div>

      <p className="text-public-caption text-faint">
        {copy.pricingPage.cards.context} {formatContext(model.context_length)} ·{" "}
        {copy.pricingPage.cards.maxOutput} {formatContext(model.max_output)}
      </p>

      <dl className="grid grid-cols-2 gap-x-2 gap-y-2 border-t border-line pt-2.5">
        {priceCells.map((cell) => (
          <div key={cell.key}>
            <dt className="text-public-caption text-faint">{cell.label}</dt>
            <dd className="mt-px truncate font-mono text-public-caption tabular-nums text-ink">
              {cell.value}
            </dd>
          </div>
        ))}
      </dl>

      {isGrouped ? (
        <p className="text-public-caption text-faint">
          {copy.pricingPage.detail.group}{" "}
          <span className="font-mono text-cyan">
            {isPerRequest
              ? requestValue(group.input)
              : `${copy.pricingPage.table.input} ${tokenValue(group.input)} · ${
                  copy.pricingPage.table.output
                } ${tokenValue(group.output)}`}
          </span>
        </p>
      ) : null}

      <div className="mt-auto flex items-center justify-between gap-2.5">
        <label className="inline-flex cursor-pointer select-none items-center gap-[7px] text-public-caption text-muted">
          <span className="relative inline-flex size-3.5">
            <input
              type="checkbox"
              checked={selected}
              disabled={compareDisabled}
              onChange={(event) => onToggleCompare(model.model_id, event.target.checked)}
              aria-label={`${copy.pricingPage.table.compare} ${displayName}`}
              className="peer size-3.5 cursor-pointer appearance-none rounded-[3px] border border-line-2 bg-transparent transition-colors checked:border-[var(--action-bg)] checked:bg-[var(--action-bg)] disabled:cursor-not-allowed disabled:opacity-40"
            />
            <svg
              aria-hidden="true"
              viewBox="0 0 12 12"
              className="pointer-events-none absolute left-1/2 top-1/2 size-2.5 -translate-x-1/2 -translate-y-1/2 text-on-action opacity-0 peer-checked:opacity-100"
            >
              <path
                d="M2 6.5 4.5 9 10 3"
                fill="none"
                stroke="currentColor"
                strokeWidth="1.8"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          </span>
          {copy.pricingPage.table.compare}
        </label>
        <button
          type="button"
          onClick={() => onOpenDetail(model)}
          className="text-public-caption text-cyan transition-colors hover:text-cyan-soft"
        >
          {copy.pricingPage.cards.detail}
        </button>
      </div>
    </article>
  );
}
