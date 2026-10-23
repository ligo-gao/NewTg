"use client";

import { useEffect, useId, useRef, type MouseEvent, type ReactNode } from "react";
import type { PricingCompareDialogProps } from "@/components/landing/pricing-view-types";
import type { PricingModel } from "@/lib/pricing-api";
import {
  effectivePrices,
  formatContext,
  formatCredits,
  formatPerRequest,
  formatRatio,
  formatUSD,
  type EffectivePrices,
} from "@/lib/pricing-display";

const MAX_COMPARE = 3;

export function PricingCompareDialog({
  models,
  meta,
  copy,
  display,
  open,
  onOpenChange,
  onClear,
}: PricingCompareDialogProps) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  const compareCopy = copy.pricingPage.compare;
  const freeLabel = copy.pricingPage.table.free;
  const dash = copy.pricingPage.table.dash;
  const perCall = copy.pricingPage.table.perCall;

  const selected = models.slice(0, MAX_COMPARE);
  const enough = selected.length >= 2;

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    if (open) {
      if (!dialog.open) dialog.showModal();
    } else if (dialog.open) {
      dialog.close();
    }
  }, [open]);

  if (!enough) {
    return null;
  }

  function tokenValue(credits: string | null, model: PricingModel): string {
    if (credits === null) return dash;
    if (model.is_free) return freeLabel;
    return formatCredits(credits, meta, display.unit);
  }

  function requestValue(credits: string | null, model: PricingModel): string {
    if (credits === null) return dash;
    if (model.is_free) return freeLabel;
    return `${formatPerRequest(credits, meta)} ${perCall}`;
  }

  function usdValue(credits: string | null, request: boolean): string {
    if (credits === null) return "";
    return formatUSD(credits, meta, request ? "M" : display.unit);
  }

  function tokenCell(credits: string | null, model: PricingModel) {
    const showUSD = display.dual && !model.is_free;
    return (
      <span>
        {tokenValue(credits, model)}
        {showUSD ? (
          <small className="mt-0.5 block font-mono text-[10px] text-faint">
            {usdValue(credits, false)}
          </small>
        ) : null}
      </span>
    );
  }

  function requestCell(credits: string | null, model: PricingModel) {
    const showUSD = display.dual && !model.is_free;
    return (
      <span>
        {requestValue(credits, model)}
        {showUSD ? (
          <small className="mt-0.5 block font-mono text-[10px] text-faint">
            {usdValue(credits, true)}
          </small>
        ) : null}
      </span>
    );
  }

  const rows: Array<{
    key: string;
    label: string;
    value: (model: PricingModel, prices: EffectivePrices) => ReactNode;
  }> = [
    {
      key: "context",
      label: compareCopy.context,
      value: (model) => formatContext(model.context_length),
    },
    {
      key: "max_output",
      label: compareCopy.maxOutput,
      value: (model) => formatContext(model.max_output),
    },
    {
      key: "mode",
      label: compareCopy.mode,
      value: (model) =>
        model.mode === "per_request" ? copy.pricingPage.modeRequest : copy.pricingPage.modeToken,
    },
    {
      key: "input",
      label: compareCopy.input,
      value: (model, prices) =>
        model.mode === "per_request" ? dash : tokenCell(prices.input, model),
    },
    {
      key: "output",
      label: compareCopy.output,
      value: (model, prices) =>
        model.mode === "per_request" ? dash : tokenCell(prices.output, model),
    },
    {
      key: "cache_read",
      label: compareCopy.cacheRead,
      value: (model, prices) =>
        model.mode === "per_request" ? dash : tokenCell(prices.cacheRead, model),
    },
    {
      key: "cache_write",
      label: compareCopy.cacheWrite,
      value: (model, prices) =>
        model.mode === "per_request" ? dash : tokenCell(prices.cacheWrite, model),
    },
    {
      key: "per_request",
      label: compareCopy.perRequest,
      value: (model, prices) =>
        model.mode === "per_request" ? requestCell(prices.input, model) : dash,
    },
    {
      key: "group",
      label: compareCopy.group,
      value: (_model, prices) => {
        const ratio = formatRatio(prices.ratio);
        return ratio === "" ? dash : ratio;
      },
    },
  ];

  function handleBackdropClick(event: MouseEvent<HTMLDialogElement>) {
    if (event.target === event.currentTarget) {
      onOpenChange(false);
    }
  }

  function handleClear() {
    onClear();
    onOpenChange(false);
  }

  return (
    <dialog
      ref={dialogRef}
      aria-labelledby={titleId}
      onClick={handleBackdropClick}
      onClose={() => onOpenChange(false)}
      className="m-auto max-h-[min(80vh,640px)] w-[calc(100%_-_2rem)] max-w-[860px] rounded-[10px] border border-line-2 bg-panel p-0 text-ink backdrop:bg-[var(--overlay)] backdrop:backdrop-blur-[3px]"
    >
      <div className="flex items-center justify-between gap-3 border-b border-line px-[18px] py-3.5">
        <h2 id={titleId} className="text-[14px] font-semibold">
          {compareCopy.title}
        </h2>
        <div className="flex items-center gap-2.5">
          <button
            type="button"
            onClick={handleClear}
            className="text-[12px] text-muted transition-colors hover:text-ink"
          >
            {compareCopy.clear}
          </button>
          <button
            type="button"
            onClick={() => onOpenChange(false)}
            aria-label={compareCopy.close}
            className="size-[30px] rounded-[5px] border border-line-2 bg-transparent leading-none text-muted transition-colors hover:text-ink"
          >
            ×
          </button>
        </div>
      </div>
      <div className="max-h-[64vh] overflow-auto px-[18px] pb-[18px] pt-1">
        <table className="w-full border-collapse text-[12.5px]">
          <caption className="sr-only">{compareCopy.title}</caption>
          <thead>
            <tr>
              <th
                scope="col"
                className="border-b border-line px-1.5 py-2.5 text-left text-[11px] font-medium text-faint"
              >
                {compareCopy.indicator}
              </th>
              {selected.map((model) => (
                <th
                  key={model.model_id}
                  scope="col"
                  className="border-b border-line px-1.5 py-2.5 text-left font-mono text-xs font-medium text-ink"
                >
                  {model.display_name || model.model_id}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((row, rowIndex) => {
              const isLast = rowIndex === rows.length - 1;
              return (
                <tr key={row.key}>
                  <th
                    scope="row"
                    className={`px-1.5 py-2.5 text-left text-[11px] font-medium text-faint ${
                      isLast ? "" : "border-b border-line"
                    }`}
                  >
                    {row.label}
                  </th>
                  {selected.map((model) => (
                    <td
                      key={model.model_id}
                      className={`px-1.5 py-2.5 font-mono text-xs text-ink-2 ${
                        isLast ? "" : "border-b border-line"
                      }`}
                    >
                      {row.value(model, effectivePrices(model, display.group))}
                    </td>
                  ))}
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </dialog>
  );
}
