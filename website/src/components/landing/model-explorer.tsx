"use client";

import { useId, useMemo, useRef, useState, type MouseEvent } from "react";
import { ModelCard } from "@/components/landing/model-card";
import { useHomeData } from "@/components/landing/home-data";
import type { Copy } from "@/config/copy";
import { site } from "@/config/site";
import { filterPricingModels } from "@/lib/pricing-filter";
import { formatPrice, formatTokenCount } from "@/lib/pricing-format";
import type { PricingModel } from "@/lib/pricing-api";

const MAX_RESULTS = 24;

function chipClass(active: boolean): string {
  return [
    "h-[31px] rounded-full border px-[13px] text-[12.5px] transition-colors",
    active
      ? "border-cyan/45 bg-cyan/[0.07] text-cyan"
      : "border-line bg-transparent text-muted hover:border-line-2 hover:text-ink-2",
  ].join(" ");
}

function SkeletonCard() {
  return (
    <div className="flex flex-col gap-4 rounded-[10px] border border-line bg-panel p-[18px]">
      <div className="flex items-center gap-2">
        <span className="size-1.5 rounded-full bg-line" />
        <span className="h-3.5 w-28 animate-pulse rounded bg-line-2/60" />
      </div>
      <span className="h-3 w-16 animate-pulse rounded bg-line-2/40" />
      <div className="grid grid-cols-2 gap-3 border-t border-line pt-3.5">
        {Array.from({ length: 4 }).map((_, index) => (
          <div key={index} className="space-y-1.5">
            <span className="block h-2.5 w-12 animate-pulse rounded bg-line-2/40" />
            <span className="block h-3 w-16 animate-pulse rounded bg-line-2/60" />
          </div>
        ))}
      </div>
    </div>
  );
}

export function ModelExplorer({ copy }: { copy: Copy }) {
  const { catalogStatus, models, featured, meta, vendors, retry } = useHomeData();
  const [query, setQuery] = useState("");
  const [vendor, setVendor] = useState("all");
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const searchRef = useRef<HTMLInputElement>(null);
  const dialogRef = useRef<HTMLDialogElement>(null);
  const searchId = useId();
  const dialogTitleId = useId();

  const isDefaultView = query.trim() === "" && vendor === "all";
  const filtered = useMemo(
    () => filterPricingModels(models, { query, vendor }),
    [models, query, vendor],
  );
  const shown = isDefaultView ? featured : filtered.slice(0, MAX_RESULTS);
  const truncated = !isDefaultView && filtered.length > MAX_RESULTS;

  const vendorOptions = useMemo(() => {
    // 后台未填写 vendor 时接口会返回空串；不能把它当成一个筛选项（会渲染成空白
    // 按钮，且点选后「筛了等于没筛」）。
    const named = vendors
      .filter((item) => item.value.trim() !== "")
      .map((item) => ({ value: item.value, label: item.label?.trim() || item.value }));
    if (named.length > 0) {
      return named;
    }
    const unique = new Map<string, string>();
    models.forEach((model) => {
      const value = model.vendor?.trim() ?? "";
      if (value !== "" && !unique.has(value)) {
        unique.set(value, value);
      }
    });
    return Array.from(unique, ([value, label]) => ({ value, label }));
  }, [vendors, models]);

  const selectedModels = useMemo(
    () => models.filter((model) => selected.has(model.model_id)),
    [models, selected],
  );

  const compareRows = useMemo(
    () => [
      { label: copy.models.dialog.rows.vendor, value: (model: PricingModel) => model.vendor, mono: false },
      {
        label: copy.models.dialog.rows.context,
        value: (model: PricingModel) => formatTokenCount(model.context_length),
        mono: true,
      },
      {
        label: copy.models.dialog.rows.maxOutput,
        value: (model: PricingModel) => formatTokenCount(model.max_output),
        mono: true,
      },
      {
        label: copy.models.dialog.rows.input,
        value: (model: PricingModel) => formatPrice(model.prompt_price, meta, model.mode),
        mono: true,
      },
      {
        label: copy.models.dialog.rows.output,
        value: (model: PricingModel) => formatPrice(model.completion_price, meta, model.mode),
        mono: true,
      },
    ],
    [meta],
  );

  function toggleModel(id: string) {
    setSelected((previous) => {
      const next = new Set(previous);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  }

  function resetFilters() {
    setQuery("");
    setVendor("all");
    searchRef.current?.focus();
  }

  function openCompare() {
    if (selected.size < 2) {
      return;
    }
    dialogRef.current?.showModal();
  }

  function closeCompare() {
    dialogRef.current?.close();
  }

  function clearCompare() {
    setSelected(new Set());
    dialogRef.current?.close();
  }

  function handleDialogClick(event: MouseEvent<HTMLDialogElement>) {
    if (event.target === event.currentTarget) {
      dialogRef.current?.close();
    }
  }

  return (
    <section
      id="models"
      className="scroll-mt-20 border-t border-line py-8 min-[621px]:py-10"
    >
      <div className="mx-auto w-full max-w-[1520px] px-4 min-[621px]:px-6 wide:px-10">
        <div className="mb-[18px] flex flex-wrap items-start justify-between gap-x-8 gap-y-5">
          <div>
            <h2 className="text-[clamp(1.75rem,3vw,2.4rem)] font-semibold leading-tight tracking-[-0.03em]">
              {copy.models.title}
            </h2>
            <p className="mt-2 max-w-[34em] text-[15px] leading-[1.8] text-ink-2">{copy.models.subtitle}</p>
          </div>

          <div className="relative w-full min-[1081px]:w-[340px] min-[1081px]:flex-none">
            <svg
              aria-hidden="true"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.8"
              className="pointer-events-none absolute left-[13px] top-1/2 size-[15px] -translate-y-1/2 text-faint"
            >
              <circle cx="11" cy="11" r="7" />
              <path d="m20 20-3.5-3.5" />
            </svg>
            <label htmlFor={searchId} className="sr-only">
              {copy.models.searchLabel}
            </label>
            <input
              ref={searchRef}
              id={searchId}
              type="search"
              autoComplete="off"
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder={copy.models.searchPlaceholder}
              className="h-[42px] w-full rounded-md border border-line-2 bg-bg-2 pl-[39px] pr-3.5 text-[13px] text-ink transition-colors placeholder:text-faint focus:border-cyan-dim"
            />
          </div>
        </div>

        <div className="mb-4 flex flex-wrap items-center justify-between gap-4">
          {vendorOptions.length > 0 ? (
            <div
              role="group"
              aria-label={copy.models.vendorFilterLabel}
              className="flex flex-wrap gap-2"
            >
              <button
                type="button"
                aria-pressed={vendor === "all"}
                onClick={() => setVendor("all")}
                className={chipClass(vendor === "all")}
              >
                {copy.models.allVendors}
              </button>
              {vendorOptions.map((item) => (
                <button
                  key={item.value}
                  type="button"
                  aria-pressed={vendor === item.value}
                  onClick={() => setVendor(item.value)}
                  className={chipClass(vendor === item.value)}
                >
                  {item.label}
                </button>
              ))}
            </div>
          ) : null}
          {isDefaultView ? (
            <a
              href={site.modelsHref}
              className="ml-auto inline-flex items-center gap-1 text-[13px] text-cyan transition-colors hover:text-cyan-soft"
            >
              {copy.models.moreModels}
              <svg
                width="12"
                height="12"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
                aria-hidden="true"
              >
                <path d="m9 18 6-6-6-6" />
              </svg>
            </a>
          ) : (
            <p aria-live="polite" className="ml-auto font-mono text-xs text-faint">
              <b className="font-medium text-ink-2">{shown.length}</b> {copy.models.countUnit}
            </p>
          )}
        </div>

        {catalogStatus === "loading" ? (
          <div className="grid grid-cols-1 gap-3 min-[621px]:grid-cols-2 min-[1081px]:grid-cols-3 wide:grid-cols-4">
            {Array.from({ length: 8 }).map((_, index) => (
              <SkeletonCard key={index} />
            ))}
          </div>
        ) : catalogStatus === "error" ? (
          <div className="rounded-[10px] border border-dashed border-line-2 px-6 py-14 text-center text-sm text-muted">
            <p>{copy.models.loadError}</p>
            <button
              type="button"
              onClick={retry}
              className="mt-3 text-[13px] text-cyan underline underline-offset-[3px] transition-colors hover:text-cyan-soft"
            >
              {copy.models.retry}
            </button>
          </div>
        ) : models.length === 0 ? (
          <div className="rounded-[10px] border border-dashed border-line-2 px-6 py-14 text-center text-sm text-muted">
            {copy.models.emptyCatalog}
          </div>
        ) : shown.length === 0 ? (
          <div className="rounded-[10px] border border-dashed border-line-2 px-6 py-14 text-center text-sm text-muted">
            <p>{copy.models.emptyTitle}</p>
            <button
              type="button"
              onClick={resetFilters}
              className="mt-3 text-[13px] text-cyan underline underline-offset-[3px] transition-colors hover:text-cyan-soft"
            >
              {copy.models.emptyAction}
            </button>
          </div>
        ) : (
          <div className="grid grid-cols-1 gap-3 min-[621px]:grid-cols-2 min-[1081px]:grid-cols-3 wide:grid-cols-4">
            {shown.map((model) => (
              <ModelCard
                key={model.model_id}
                copy={copy}
                model={model}
                meta={meta}
                checked={selected.has(model.model_id)}
                onToggle={toggleModel}
              />
            ))}
          </div>
        )}

        {truncated ? (
          <p className="mt-3 text-[12px] text-faint">
            {copy.models.truncatedNote}{" "}
            <a className="text-cyan hover:text-cyan-soft" href="/pricing">
              {copy.models.pricingLink}
            </a>
          </p>
        ) : null}

      </div>

      {selected.size >= 2 ? (
        <div
          role="region"
          aria-label={copy.models.compareBar.ariaLabel}
          className="fixed inset-x-4 bottom-5 z-[70] mx-auto flex max-w-[720px] flex-wrap items-center gap-3.5 rounded-[10px] border border-line-2 bg-panel/95 px-4 py-3 panel-shadow backdrop-blur-md sm:flex-nowrap"
        >
          <p className="mr-auto text-[13px] text-ink-2">
            {copy.models.compareBar.selected}{" "}
            <b className="font-mono font-medium text-ink">{selected.size}</b>{" "}
            {copy.models.compareBar.unit}
          </p>
          <button type="button" onClick={openCompare} className="btn btn-primary btn-sm">
            {copy.models.compareBar.open}
          </button>
          <button
            type="button"
            onClick={clearCompare}
            className="px-1 py-1 text-[12.5px] text-muted transition-colors hover:text-ink"
          >
            {copy.models.compareBar.clear}
          </button>
        </div>
      ) : null}

      <dialog
        ref={dialogRef}
        aria-labelledby={dialogTitleId}
        onClick={handleDialogClick}
        className="m-auto max-h-[min(80vh,640px)] w-[calc(100%_-_2rem)] max-w-[760px] rounded-[10px] border border-line-2 bg-panel p-0 text-ink backdrop:bg-[var(--overlay)] backdrop:backdrop-blur-[3px]"
      >
        <div className="flex items-center justify-between border-b border-line px-5 py-4">
          <h2 id={dialogTitleId} className="text-[15px] font-semibold">
            {copy.models.dialog.title}
          </h2>
          <button
            type="button"
            onClick={closeCompare}
            aria-label={copy.models.dialog.close}
            className="size-[30px] rounded-[5px] border border-line-2 bg-transparent leading-none text-muted transition-colors hover:text-ink"
          >
            ×
          </button>
        </div>
        <div className="max-h-[58vh] overflow-auto px-5 pb-5 pt-1">
          <table className="w-full border-collapse text-[13px]">
            <caption className="sr-only">{copy.models.dialog.title}</caption>
            <thead>
              <tr>
                <th
                  scope="col"
                  className="border-b border-line px-2 py-2.5 text-left text-[11.5px] font-medium text-faint"
                >
                  {copy.models.dialog.indicator}
                </th>
                {selectedModels.map((model) => (
                  <th
                    key={model.model_id}
                    scope="col"
                    className="border-b border-line px-2 py-2.5 text-left font-mono text-xs font-medium text-faint"
                  >
                    {model.display_name || model.model_id}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {compareRows.map((row, rowIndex) => {
                const isLastRow = rowIndex === compareRows.length - 1;
                return (
                  <tr key={row.label}>
                    <th
                      scope="row"
                      className={`px-2 py-2.5 text-left text-[11.5px] font-medium text-faint ${
                        isLastRow ? "" : "border-b border-line"
                      }`}
                    >
                      {row.label}
                    </th>
                    {selectedModels.map((model) => (
                      <td
                        key={model.model_id}
                        className={`px-2 py-2.5 text-ink-2 ${row.mono ? "font-mono text-xs" : "text-[13px]"} ${
                          isLastRow ? "" : "border-b border-line"
                        }`}
                      >
                        {row.value(model)}
                      </td>
                    ))}
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </dialog>
    </section>
  );
}
