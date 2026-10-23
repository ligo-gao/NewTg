"use client";

import { useId, useMemo, useRef, useState, type MouseEvent } from "react";
import { copy } from "@/config/copy";
import { models, vendors, type Model, type VendorId } from "@/config/models";
import { filterModels } from "@/lib/filter-models";
import { ModelCard } from "./model-card";

type VendorFilter = VendorId | "all";

interface CompareRow {
  label: string;
  value: (model: Model) => string;
  mono: boolean;
}

const compareRows: CompareRow[] = [
  { label: copy.models.dialog.rows.vendor, value: (model) => model.vendorName, mono: false },
  { label: copy.models.dialog.rows.context, value: (model) => model.context, mono: true },
  { label: copy.models.dialog.rows.input, value: (model) => model.inputPrice, mono: true },
  { label: copy.models.dialog.rows.output, value: (model) => model.outputPrice, mono: true },
  { label: copy.models.dialog.rows.latency, value: (model) => model.latency, mono: true },
];

function chipClass(active: boolean): string {
  return [
    "h-[31px] rounded-full border px-[13px] text-[12.5px] transition-colors",
    active
      ? "border-cyan/45 bg-cyan/[0.07] text-cyan"
      : "border-line bg-transparent text-muted hover:border-line-2 hover:text-ink-2",
  ].join(" ");
}

export function ModelExplorer() {
  const [query, setQuery] = useState("");
  const [vendor, setVendor] = useState<VendorFilter>("all");
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const searchRef = useRef<HTMLInputElement>(null);
  const dialogRef = useRef<HTMLDialogElement>(null);
  const searchId = useId();
  const dialogTitleId = useId();

  const filtered = useMemo(() => filterModels(models, { query, vendor }), [query, vendor]);

  const selectedModels = useMemo(
    () => models.filter((model) => selected.has(model.id)),
    [selected],
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
    <section id="models" className="scroll-mt-20 border-t border-line py-16">
      <div className="mx-auto w-full max-w-[1520px] px-4 min-[621px]:px-6 wide:px-10">
        <div className="mb-7 flex flex-wrap items-start justify-between gap-x-8 gap-y-5">
          <div>
            <h2 className="text-[clamp(1.7rem,3vw,2.4rem)] font-semibold leading-tight tracking-[-0.03em]">
              {copy.models.title}
            </h2>
            <p className="mt-2.5 max-w-[34em] text-[14.5px] text-muted">{copy.models.subtitle}</p>
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

        <div className="mb-[22px] flex flex-wrap items-center justify-between gap-4">
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
            {vendors.map((item) => (
              <button
                key={item.id}
                type="button"
                aria-pressed={vendor === item.id}
                onClick={() => setVendor(item.id)}
                className={chipClass(vendor === item.id)}
              >
                {item.name}
              </button>
            ))}
          </div>
          <p aria-live="polite" className="font-mono text-xs text-faint">
            <b className="font-medium text-ink-2">{filtered.length}</b> {copy.models.countUnit}
          </p>
        </div>

        {filtered.length === 0 ? (
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
          <div className="grid grid-cols-1 gap-3 min-[621px]:grid-cols-2 min-[1081px]:grid-cols-3 xwide:grid-cols-4">
            {filtered.map((model) => (
              <ModelCard
                key={model.id}
                model={model}
                checked={selected.has(model.id)}
                onToggle={toggleModel}
              />
            ))}
          </div>
        )}

        <p className="mt-[18px] text-[11.5px] text-faint">{copy.models.footnote}</p>
      </div>

      {selected.size >= 2 ? (
        <div
          role="region"
          aria-label={copy.models.compareBar.ariaLabel}
          className="fixed inset-x-4 bottom-5 z-[70] mx-auto flex max-w-[720px] flex-wrap items-center gap-3.5 rounded-[10px] border border-line-2 bg-panel/95 px-4 py-3 shadow-[0_20px_50px_-20px_rgba(0,0,0,0.9)] backdrop-blur-md sm:flex-nowrap"
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
        className="m-auto max-h-[min(80vh,640px)] w-[calc(100%_-_2rem)] max-w-[760px] rounded-[10px] border border-line-2 bg-panel p-0 text-ink backdrop:bg-[rgba(3,6,11,0.72)] backdrop:backdrop-blur-[3px]"
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
                    key={model.id}
                    scope="col"
                    className="border-b border-line px-2 py-2.5 text-left font-mono text-xs font-medium text-faint"
                  >
                    {model.name}
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
                        key={model.id}
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
