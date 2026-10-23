"use client";

import { Fragment, useEffect, useMemo, useRef, useState, type MouseEvent } from "react";
import { PricingCard } from "@/components/landing/pricing-cards";
import { PricingCompareDialog } from "@/components/landing/pricing-compare";
import { PricingDetail } from "@/components/landing/pricing-detail";
import type { PricingDisplay, TokenUnit } from "@/components/landing/pricing-view-types";
import type { Copy } from "@/config/copy";
import type { Locale } from "@/config/locales";
import {
  DEFAULT_PRICING_QUERY,
  fetchPricingPage,
  pricingQueryString,
  type PricingMeta,
  type PricingModel,
  type PricingQuery,
} from "@/lib/pricing-api";
import {
  effectivePrices,
  formatContext,
  formatCredits,
  formatPerRequest,
  formatRatio,
  formatUSD,
} from "@/lib/pricing-display";

const CONTAINER = "mx-auto w-full max-w-[1520px] px-4 min-[621px]:px-6 wide:px-10";
const PAGE_SIZES = [12, 24, 48] as const;
const MAX_COMPARE = 3;
const MOBILE_QUERY = "(max-width: 760px)";
const EMPTY_MODELS: PricingModel[] = [];

/** Used only before the first payload arrives; nothing is displayed from it. */
const FALLBACK_META: PricingMeta = {
  credit_symbol: "¥",
  quota_per_unit: 100,
  usd_exchange_rate: 7.1,
  version: "",
  generated_at: "",
};

type ViewMode = "table" | "cards";

interface UrlState {
  query: PricingQuery;
  view: ViewMode;
  unit: TokenUnit;
  dual: boolean;
}

interface ActiveChip {
  key: string;
  label: string;
  onRemove: () => void;
}

function oneOf<T extends string>(raw: string | null, allowed: readonly T[], fallback: T): T {
  return allowed.includes(raw as T) ? (raw as T) : fallback;
}

function parsePageSize(raw: string | null): number {
  const value = Number(raw);
  return (PAGE_SIZES as readonly number[]).includes(value)
    ? value
    : DEFAULT_PRICING_QUERY.pageSize;
}

function readUrlState(): UrlState {
  if (typeof window === "undefined") {
    return { query: { ...DEFAULT_PRICING_QUERY }, view: "table", unit: "M", dual: false };
  }
  const params = new URLSearchParams(window.location.search);
  const page = Number.parseInt(params.get("p") ?? "", 10);
  const dualRaw = params.get("dualCurrency");
  return {
    query: {
      page: Number.isFinite(page) && page > 0 ? page : 1,
      pageSize: parsePageSize(params.get("page_size")),
      search: params.get("search") ?? "",
      sort: oneOf(params.get("sort"), ["name", "price_asc", "price_desc"] as const, "name"),
      vendors: params.getAll("vendor"),
      capabilities: params.getAll("capability"),
      contexts: params.getAll("context"),
      endpoints: params.getAll("endpointType"),
      group: params.get("group") ?? "default",
      quotaType: oneOf(params.get("quotaType"), ["all", "token", "request"] as const, "all"),
    },
    view: params.get("view") === "cards" || params.get("view") === "card" ? "cards" : "table",
    unit: params.get("tokenUnit") === "K" ? "K" : "M",
    dual: dualRaw === "1" || dualRaw === "true",
  };
}

function writeUrlState(state: UrlState) {
  if (typeof window === "undefined") return;
  const { query } = state;
  const params = new URLSearchParams();
  if (query.page > 1) params.set("p", String(query.page));
  if (query.pageSize !== DEFAULT_PRICING_QUERY.pageSize) {
    params.set("page_size", String(query.pageSize));
  }
  if (query.search.trim() !== "") params.set("search", query.search.trim());
  if (query.sort !== "name") params.set("sort", query.sort);
  query.vendors.forEach((value) => params.append("vendor", value));
  query.capabilities.forEach((value) => params.append("capability", value));
  query.contexts.forEach((value) => params.append("context", value));
  query.endpoints.forEach((value) => params.append("endpointType", value));
  if (query.group !== "default") params.set("group", query.group);
  if (query.quotaType !== "all") params.set("quotaType", query.quotaType);
  if (state.view === "cards") params.set("view", "cards");
  if (state.unit === "K") params.set("tokenUnit", "K");
  if (state.dual) params.set("dualCurrency", "1");
  const search = params.toString();
  const next = `${window.location.pathname}${search ? `?${search}` : ""}${window.location.hash}`;
  window.history.replaceState(null, "", next);
}

function chipClass(active: boolean): string {
  return [
    "h-7 rounded-full border px-2.5 text-[11.5px] transition-colors",
    active
      ? "border-cyan/40 bg-cyan/5 text-cyan"
      : "border-line bg-transparent text-muted hover:border-line-2 hover:text-ink-2",
  ].join(" ");
}

function pageList(current: number, count: number): Array<number | "gap"> {
  if (count <= 7) {
    return Array.from({ length: count }, (_, index) => index + 1);
  }
  const wanted = new Set([1, count, current - 1, current, current + 1]);
  const sorted = Array.from(wanted)
    .filter((value) => value >= 1 && value <= count)
    .sort((a, b) => a - b);
  const output: Array<number | "gap"> = [];
  let previous = 0;
  sorted.forEach((value) => {
    if (value - previous > 1) output.push("gap");
    output.push(value);
    previous = value;
  });
  return output;
}

function TableSkeleton() {
  return (
    <div
      data-testid="pricing-skeleton"
      className="overflow-hidden rounded-lg border border-line bg-panel"
    >
      <div className="flex items-center gap-4 border-b border-line bg-panel-2 px-3.5 py-3">
        {[26, 12, 10, 11, 11, 11, 11, 10, 12, 10].map((width, index) => (
          <span
            key={index}
            className="h-2.5 animate-pulse rounded bg-line-2/50"
            style={{ width: `${width * 4}px` }}
          />
        ))}
      </div>
      {Array.from({ length: 6 }).map((_, row) => (
        <div
          key={row}
          className="flex items-center gap-4 border-b border-line px-3.5 py-4 last:border-b-0"
        >
          <div className="w-[210px] flex-none space-y-1.5">
            <span className="block h-3 w-28 animate-pulse rounded bg-line-2/60" />
            <span className="block h-2.5 w-36 animate-pulse rounded bg-line-2/40" />
          </div>
          {[56, 48, 62, 62, 62, 62, 48, 62, 56].map((width, index) => (
            <span
              key={index}
              className="h-3 flex-none animate-pulse rounded bg-line-2/40"
              style={{ width: `${width}px` }}
            />
          ))}
        </div>
      ))}
    </div>
  );
}

function CardsSkeleton() {
  return (
    <div
      data-testid="pricing-skeleton"
      className="grid grid-cols-1 gap-3 min-[621px]:grid-cols-2 wide:grid-cols-3"
    >
      {Array.from({ length: 6 }).map((_, index) => (
        <div key={index} className="rounded-lg border border-line bg-panel p-4">
          <span className="block h-3.5 w-32 animate-pulse rounded bg-line-2/60" />
          <span className="mt-2 block h-2.5 w-44 animate-pulse rounded bg-line-2/40" />
          <div className="mt-4 grid grid-cols-2 gap-2 border-t border-line pt-3">
            {Array.from({ length: 4 }).map((_, cell) => (
              <span key={cell} className="block h-3 w-20 animate-pulse rounded bg-line-2/40" />
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}

export function PricingWorkbench({ copy, locale }: { copy: Copy; locale: Locale }) {
  const pageCopy = copy.pricingPage;
  const groupAriaLabel = pageCopy.toolbar.groupPrefix.replace(/[:：]\s*$/, "").trim();

  const [query, setQuery] = useState<PricingQuery>(() => ({ ...DEFAULT_PRICING_QUERY }));
  const [view, setView] = useState<ViewMode>("table");
  const [unit, setUnit] = useState<TokenUnit>("M");
  const [dual, setDual] = useState(false);
  const [ready, setReady] = useState(false);
  const [searchDraft, setSearchDraft] = useState("");
  const [catalog, setCatalog] = useState<Awaited<ReturnType<typeof fetchPricingPage>> | null>(
    null,
  );
  const [fetching, setFetching] = useState(true);
  const [fetchError, setFetchError] = useState(false);
  const [reload, setReload] = useState(0);
  const [expandedModelID, setExpandedModelID] = useState<string | null>(null);
  const [drawerModel, setDrawerModel] = useState<PricingModel | null>(null);
  const [selected, setSelected] = useState<Map<string, PricingModel>>(() => new Map());
  const [compareOpen, setCompareOpen] = useState(false);
  const [mobileFiltersOpen, setMobileFiltersOpen] = useState(false);
  const [forceCards, setForceCards] = useState(false);

  const queryRef = useRef(query);
  queryRef.current = query;
  const viewRef = useRef(view);
  viewRef.current = view;
  const unitRef = useRef(unit);
  unitRef.current = unit;
  const dualRef = useRef(dual);
  dualRef.current = dual;

  // The URL is the source of truth: hydrate once, then follow back/forward.
  useEffect(() => {
    const initial = readUrlState();
    setQuery(initial.query);
    setView(initial.view);
    setUnit(initial.unit);
    setDual(initial.dual);
    setSearchDraft(initial.query.search);
    setReady(true);

    function onPopState() {
      const next = readUrlState();
      setQuery(next.query);
      setView(next.view);
      setUnit(next.unit);
      setDual(next.dual);
      setSearchDraft(next.query.search);
      setExpandedModelID(null);
    }

    window.addEventListener("popstate", onPopState);
    return () => window.removeEventListener("popstate", onPopState);
  }, []);

  // Phones always get cards, whatever the stored view says.
  useEffect(() => {
    if (typeof window.matchMedia !== "function") return;
    const media = window.matchMedia(MOBILE_QUERY);
    const update = () => setForceCards(media.matches);
    update();
    media.addEventListener?.("change", update);
    return () => media.removeEventListener?.("change", update);
  }, []);

  useEffect(() => {
    if (!drawerModel) return;
    function onKeyDown(event: KeyboardEvent) {
      if (event.key === "Escape") setDrawerModel(null);
    }
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [drawerModel]);

  const queryKey = useMemo(() => pricingQueryString(query), [query]);

  useEffect(() => {
    if (!ready) return;
    const controller = new AbortController();
    let alive = true;
    setFetching(true);
    setFetchError(false);
    fetchPricingPage(queryRef.current, controller.signal)
      .then((payload) => {
        if (!alive) return;
        setCatalog(payload);
        setFetching(false);
      })
      .catch((cause: unknown) => {
        if (!alive || controller.signal.aborted) return;
        if (cause instanceof DOMException && cause.name === "AbortError") return;
        setFetching(false);
        setFetchError(true);
      });
    return () => {
      alive = false;
      controller.abort();
    };
  }, [ready, queryKey, reload]);

  // Debounced search; the input stays responsive while the request waits.
  useEffect(() => {
    if (!ready) return;
    const trimmed = searchDraft.trim();
    if (trimmed === query.search) return;
    const timer = window.setTimeout(() => {
      const next = { ...queryRef.current, search: trimmed, page: 1 };
      setQuery(next);
      writeUrlState({
        query: next,
        view: viewRef.current,
        unit: unitRef.current,
        dual: dualRef.current,
      });
    }, 300);
    return () => window.clearTimeout(timer);
  }, [ready, searchDraft, query.search]);

  function commitQuery(next: PricingQuery) {
    setQuery(next);
    writeUrlState({
      query: next,
      view: viewRef.current,
      unit: unitRef.current,
      dual: dualRef.current,
    });
  }

  function clearFilters() {
    const next: PricingQuery = {
      ...queryRef.current,
      page: 1,
      search: "",
      vendors: [],
      capabilities: [],
      contexts: [],
      endpoints: [],
      quotaType: "all",
    };
    setSearchDraft("");
    setExpandedModelID(null);
    commitQuery(next);
  }

  function toggleVendor(value: string, checked: boolean) {
    const vendors = checked
      ? [...query.vendors, value]
      : query.vendors.filter((item) => item !== value);
    commitQuery({ ...query, page: 1, vendors });
  }

  function toggleCapability(value: string) {
    const active = query.capabilities.includes(value);
    const capabilities = active
      ? query.capabilities.filter((item) => item !== value)
      : [...query.capabilities, value];
    commitQuery({ ...query, page: 1, capabilities });
  }

  function setSingleContext(value: string) {
    commitQuery({ ...query, page: 1, contexts: value === "all" ? [] : [value] });
  }

  function setSingleEndpoint(value: string) {
    commitQuery({ ...query, page: 1, endpoints: value === "all" ? [] : [value] });
  }

  function setQuotaType(value: PricingQuery["quotaType"]) {
    commitQuery({ ...query, page: 1, quotaType: value });
  }

  function setSort(value: PricingQuery["sort"]) {
    commitQuery({ ...query, page: 1, sort: value });
  }

  function setGroup(value: string) {
    commitQuery({ ...query, group: value });
  }

  function setPageSize(value: number) {
    commitQuery({ ...query, page: 1, pageSize: value });
  }

  function goToPage(value: number) {
    const bounded = Math.max(1, value);
    if (bounded === query.page) return;
    commitQuery({ ...query, page: bounded });
  }

  function changeView(next: ViewMode) {
    setView(next);
    writeUrlState({
      query: queryRef.current,
      view: next,
      unit: unitRef.current,
      dual: dualRef.current,
    });
  }

  function changeUnit(next: TokenUnit) {
    setUnit(next);
    writeUrlState({
      query: queryRef.current,
      view: viewRef.current,
      unit: next,
      dual: dualRef.current,
    });
  }

  function toggleDual() {
    const next = !dual;
    setDual(next);
    writeUrlState({
      query: queryRef.current,
      view: viewRef.current,
      unit: unitRef.current,
      dual: next,
    });
  }

  function toggleExpanded(modelID: string) {
    setExpandedModelID((current) => (current === modelID ? null : modelID));
  }

  function handleRowClick(event: MouseEvent<HTMLTableRowElement>, modelID: string) {
    const target = event.target as HTMLElement;
    if (target.closest("input, button, label, a, select, textarea, summary")) return;
    toggleExpanded(modelID);
  }

  function toggleCompare(model: PricingModel, checked: boolean) {
    setSelected((previous) => {
      const next = new Map(previous);
      if (!checked) {
        next.delete(model.model_id);
      } else if (next.size < MAX_COMPARE) {
        next.set(model.model_id, model);
      }
      return next;
    });
  }

  function clearCompare() {
    setSelected(new Map());
    setCompareOpen(false);
  }

  const items = catalog?.items ?? EMPTY_MODELS;
  const meta = catalog?.meta ?? FALLBACK_META;
  const facets = catalog?.facets;
  const total = catalog ? Number(catalog.total) : 0;
  const totalSafe = Number.isFinite(total) && total > 0 ? total : 0;
  const pageCount = Math.max(1, Math.ceil(totalSafe / query.pageSize));
  const effectiveView: ViewMode = forceCards ? "cards" : view;
  const display = useMemo<PricingDisplay>(
    () => ({ unit, dual, group: query.group }),
    [unit, dual, query.group],
  );
  const selectedModels = useMemo(() => Array.from(selected.values()), [selected]);
  const selectedGroupRatio =
    facets?.groups.find((group) => group.value === query.group)?.ratio ?? null;

  const lowestCredits = useMemo(() => {
    let lowest: number | null = null;
    items.forEach((model) => {
      if (model.mode !== "per_token" || model.is_free || model.prompt_price === null) return;
      const value = Number(model.prompt_price);
      if (!Number.isFinite(value) || value <= 0) return;
      lowest = lowest === null ? value : Math.min(lowest, value);
    });
    return lowest === null ? null : String(lowest);
  }, [items]);

  const lowestValue =
    lowestCredits === null ? pageCopy.table.dash : formatCredits(lowestCredits, meta, "M");

  const stats = [
    { value: catalog ? String(totalSafe) : pageCopy.table.dash, label: pageCopy.stats.models },
    {
      value: catalog ? String(facets?.vendors.length ?? 0) : pageCopy.table.dash,
      label: pageCopy.stats.vendors,
    },
    { value: catalog ? lowestValue : pageCopy.table.dash, label: pageCopy.stats.lowest },
    { value: "60s", label: pageCopy.stats.sync },
  ];

  function facetLabel(options: Array<{ value: string; label: string }> | undefined, value: string) {
    return options?.find((option) => option.value === value)?.label || value;
  }

  const activeChips: ActiveChip[] = [];
  if (query.search.trim() !== "") {
    const label = query.search.trim();
    activeChips.push({
      key: "search",
      label,
      onRemove: () => {
        setSearchDraft("");
        commitQuery({ ...query, page: 1, search: "" });
      },
    });
  }
  query.vendors.forEach((value) => {
    activeChips.push({
      key: `vendor-${value}`,
      label: facetLabel(facets?.vendors, value),
      onRemove: () =>
        commitQuery({
          ...query,
          page: 1,
          vendors: query.vendors.filter((item) => item !== value),
        }),
    });
  });
  query.capabilities.forEach((value) => {
    activeChips.push({
      key: `capability-${value}`,
      label: facetLabel(facets?.capabilities, value),
      onRemove: () =>
        commitQuery({
          ...query,
          page: 1,
          capabilities: query.capabilities.filter((item) => item !== value),
        }),
    });
  });
  query.contexts.forEach((value) => {
    activeChips.push({
      key: `context-${value}`,
      label: facetLabel(facets?.context, value),
      onRemove: () => commitQuery({ ...query, page: 1, contexts: [] }),
    });
  });
  query.endpoints.forEach((value) => {
    activeChips.push({
      key: `endpoint-${value}`,
      label: facetLabel(facets?.endpoints, value),
      onRemove: () => commitQuery({ ...query, page: 1, endpoints: [] }),
    });
  });
  if (query.quotaType !== "all") {
    activeChips.push({
      key: "quotaType",
      label:
        query.quotaType === "token" ? pageCopy.filters.perToken : pageCopy.filters.perRequest,
      onRemove: () => commitQuery({ ...query, page: 1, quotaType: "all" }),
    });
  }

  const selectClass =
    "h-[34px] w-full rounded-[5px] border border-line-2 bg-bg-2 px-2.5 text-[12.5px] text-ink";
  const toolbarSelectClass =
    "h-[34px] w-auto min-w-[120px] rounded-[5px] border border-line-2 bg-bg-2 px-2.5 text-[12.5px] text-ink";
  const thBase =
    "whitespace-nowrap border-b border-line bg-panel-2 px-3 py-2.5 text-left text-[11px] font-medium text-faint";
  const thNum = `${thBase} text-right`;
  const cellBase = "border-b border-line px-3 py-3 align-middle";
  const priceCellClass =
    "font-mono text-[12.5px] text-ink [font-variant-numeric:tabular-nums]";

  function dashNode() {
    return <span className="font-mono text-[12.5px] text-faint">{pageCopy.table.dash}</span>;
  }

  function freeNode() {
    return <span className="text-[12.5px] font-medium text-green">{pageCopy.table.free}</span>;
  }

  function priceNode(price: string | null, model: PricingModel) {
    if (model.is_free) return freeNode();
    if (price === null) return dashNode();
    const usd = dual ? formatUSD(price, meta, unit) : "";
    return (
      <span className={priceCellClass}>
        <span className="block">{formatCredits(price, meta, unit)}</span>
        {usd ? <span className="block text-[10px] text-faint">{usd}</span> : null}
      </span>
    );
  }

  function perRequestNode(price: string | null, model: PricingModel, tone: boolean) {
    if (model.is_free) return freeNode();
    if (price === null) return dashNode();
    const usd = dual ? formatUSD(price, meta, "M") : "";
    return (
      <span className={`font-mono text-[12.5px] ${tone ? "text-cyan" : "text-ink"}`}>
        <span className="block">
          {formatPerRequest(price, meta)}
          <span className="text-[10px] text-faint"> {pageCopy.table.perCall}</span>
        </span>
        {usd ? <span className="block text-[10px] text-faint">{usd}</span> : null}
      </span>
    );
  }

  function groupNode(model: PricingModel) {
    if (query.group === "default") return dashNode();
    if (model.is_free) return freeNode();
    const prices = effectivePrices(model, query.group);
    if (model.mode === "per_request") {
      return perRequestNode(prices.input, model, true);
    }
    return (
      <span className="block leading-[1.55]">
        <span className="flex items-baseline justify-end gap-1">
          <span className="text-[9.5px] text-faint">{pageCopy.table.input}</span>
          <span className="font-mono text-[12.5px] text-cyan">
            {prices.input === null ? pageCopy.table.dash : formatCredits(prices.input, meta, unit)}
          </span>
        </span>
        <span className="flex items-baseline justify-end gap-1">
          <span className="text-[9.5px] text-faint">{pageCopy.table.output}</span>
          <span className="font-mono text-[12.5px] text-cyan">
            {prices.output === null ? pageCopy.table.dash : formatCredits(prices.output, meta, unit)}
          </span>
        </span>
      </span>
    );
  }

  function modelBadges(model: PricingModel) {
    const tags = (model.tags ?? []).filter(
      (tag) => tag !== pageCopy.table.free && tag !== pageCopy.modeRequest,
    );
    return (
      <span className="mt-1 flex flex-wrap gap-1">
        {model.is_free ? (
          <span className="rounded-[3px] border border-green/30 px-1.5 text-[9.5px] leading-[1.5] text-green">
            {pageCopy.table.free}
          </span>
        ) : null}
        {model.mode === "per_request" ? (
          <span className="rounded-[3px] border border-cyan/30 px-1.5 text-[9.5px] leading-[1.5] text-cyan">
            {pageCopy.modeRequest}
          </span>
        ) : null}
        {model.pricing_schedule ? (
          <span
            data-testid="schedule-badge"
            className="rounded-[3px] border border-cyan/30 px-1.5 text-[9.5px] leading-[1.5] text-cyan"
          >
            {pageCopy.schedule.badge}
          </span>
        ) : null}
        {tags.slice(0, 3).map((tag) => (
          <span
            key={tag}
            className="rounded-[3px] border border-line-2 px-1.5 text-[9.5px] leading-[1.5] text-muted"
          >
            {tag}
          </span>
        ))}
      </span>
    );
  }

  const start = totalSafe === 0 ? 0 : (query.page - 1) * query.pageSize + 1;
  const end = Math.min(totalSafe, query.page * query.pageSize);
  const rangeText = `${start}–${end} / ${totalSafe}`;
  const pageNumbers = pageList(Math.min(query.page, pageCount), pageCount);

  return (
    <div data-locale={locale}>
      <section className="pb-[30px] pt-14">
        <div className={CONTAINER}>
          <h1 className="text-[clamp(2.1rem,3.6vw,3rem)] font-bold leading-[1.12] tracking-[-0.035em]">
            {pageCopy.title}
          </h1>
          <p className="mt-3 max-w-[46em] text-[15px] leading-[1.8] text-ink-2">
            {pageCopy.subtitle}
          </p>

          <div className="mt-[26px] overflow-hidden rounded-lg border border-line bg-line">
            <div className="grid grid-cols-2 gap-px min-[621px]:grid-cols-4">
              {stats.map((stat) => (
                <div key={stat.label} className="bg-panel px-5 py-3.5">
                  <span className="block font-mono text-xl font-medium tracking-[-0.02em] text-ink">
                    {stat.value}
                  </span>
                  <span className="mt-0.5 block text-[11.5px] text-muted">{stat.label}</span>
                </div>
              ))}
            </div>
          </div>

          <div className="mt-3.5 flex flex-wrap items-start gap-x-[18px] gap-y-2 text-[11.5px] text-faint">
            {pageCopy.legend[0] ? (
              <span className="inline-flex items-center gap-1.5">
                <i aria-hidden="true" className="size-1 shrink-0 rounded-full bg-cyan-dim" />
                {pageCopy.legend[0]}
              </span>
            ) : null}
            {pageCopy.legend.length > 1 ? (
              <details className="group min-w-0">
                <summary className="inline-flex cursor-pointer list-none items-center gap-1.5 text-cyan transition-colors hover:text-cyan-soft [&::-webkit-details-marker]:hidden">
                  <svg
                    width="10"
                    height="10"
                    viewBox="0 0 10 10"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.4"
                    strokeLinecap="round"
                    aria-hidden="true"
                    className="shrink-0 transition-transform group-open:rotate-90"
                  >
                    <path d="M3.5 2l3 3-3 3" />
                  </svg>
                  {pageCopy.legendSummary}
                </summary>
                <ul className="mt-2 flex flex-col gap-1.5">
                  {pageCopy.legend.slice(1).map((line) => (
                    <li key={line} className="inline-flex items-center gap-1.5">
                      <i aria-hidden="true" className="size-1 shrink-0 rounded-full bg-cyan-dim" />
                      {line}
                    </li>
                  ))}
                </ul>
              </details>
            ) : null}
          </div>
        </div>
      </section>

      <div
        className={`${CONTAINER} grid items-start gap-6 pb-24 min-[1101px]:grid-cols-[220px_minmax(0,1fr)]`}
      >
        <aside
          aria-label={pageCopy.filters.title}
          className={`${
            mobileFiltersOpen ? "block" : "hidden"
          } rounded-lg border border-line bg-panel p-[18px] min-[1101px]:sticky min-[1101px]:top-[84px] min-[1101px]:block`}
        >
          <div className="flex items-center justify-between gap-3 border-b border-line pb-3">
            <h2 className="text-[13px] font-semibold text-ink">{pageCopy.filters.title}</h2>
            <div className="flex items-center gap-2">
              <button
                type="button"
                onClick={clearFilters}
                className="text-xs text-cyan transition-colors hover:text-cyan-soft"
              >
                {pageCopy.filters.reset}
              </button>
              <button
                type="button"
                onClick={() => setMobileFiltersOpen(false)}
                className="text-xs text-cyan transition-colors hover:text-cyan-soft min-[1101px]:hidden"
              >
                {pageCopy.filters.close}
              </button>
            </div>
          </div>

          <div className="mt-3">
            <h3 className="mb-2 text-[11px] font-medium text-faint">{pageCopy.filters.vendors}</h3>
            <div>
              {(facets?.vendors ?? []).map((vendor) => (
                <label
                  key={vendor.value}
                  className="flex cursor-pointer items-center gap-2 py-[3px] text-[12.5px] text-ink-2 transition-colors hover:text-ink"
                >
                  <input
                    type="checkbox"
                    aria-label={vendor.label || vendor.value}
                    checked={query.vendors.includes(vendor.value)}
                    onChange={(event) => toggleVendor(vendor.value, event.target.checked)}
                    className="size-3.5 accent-cyan"
                  />
                  <span>{vendor.label || vendor.value}</span>
                  <span className="ml-auto font-mono text-[11px] text-faint">{vendor.count}</span>
                </label>
              ))}
            </div>
          </div>

          <div className="mt-3.5 border-t border-line pt-3.5">
            <h3 className="mb-2 text-[11px] font-medium text-faint">
              {pageCopy.filters.capabilities}
            </h3>
            <div className="flex flex-wrap gap-1.5">
              {(facets?.capabilities ?? []).map((capability) => (
                <button
                  key={capability.value}
                  type="button"
                  aria-pressed={query.capabilities.includes(capability.value)}
                  onClick={() => toggleCapability(capability.value)}
                  className={chipClass(query.capabilities.includes(capability.value))}
                >
                  {capability.label || capability.value}
                </button>
              ))}
            </div>
          </div>

          <div className="mt-3.5 border-t border-line pt-3.5">
            <h3 className="mb-2 text-[11px] font-medium text-faint">{pageCopy.filters.context}</h3>
            <select
              aria-label={pageCopy.filters.context}
              value={query.contexts[0] ?? "all"}
              onChange={(event) => setSingleContext(event.target.value)}
              className={selectClass}
            >
              <option value="all">{pageCopy.filters.all}</option>
              {(facets?.context ?? []).map((option) => (
                <option key={option.value} value={option.value}>
                  {option.label || option.value}
                </option>
              ))}
            </select>
          </div>

          <div className="mt-3.5 border-t border-line pt-3.5">
            <h3 className="mb-2 text-[11px] font-medium text-faint">{pageCopy.filters.endpoint}</h3>
            <select
              aria-label={pageCopy.filters.endpoint}
              value={query.endpoints[0] ?? "all"}
              onChange={(event) => setSingleEndpoint(event.target.value)}
              className={selectClass}
            >
              <option value="all">{pageCopy.filters.all}</option>
              {(facets?.endpoints ?? []).map((option) => (
                <option key={option.value} value={option.value}>
                  {option.label || option.value}
                </option>
              ))}
            </select>
          </div>

          <div className="mt-3.5 border-t border-line pt-3.5">
            <h3 className="mb-2 text-[11px] font-medium text-faint">{pageCopy.filters.mode}</h3>
            <select
              aria-label={pageCopy.filters.mode}
              value={query.quotaType}
              onChange={(event) => setQuotaType(event.target.value as PricingQuery["quotaType"])}
              className={selectClass}
            >
              <option value="all">{pageCopy.filters.all}</option>
              <option value="token">{pageCopy.filters.perToken}</option>
              <option value="request">{pageCopy.filters.perRequest}</option>
            </select>
          </div>
        </aside>

        <section aria-label={pageCopy.title} aria-busy={fetching} className="min-w-0">
          <div className="flex flex-wrap items-center gap-2.5 border-b border-line pb-3.5 pt-3">
            <div className="relative min-w-[220px] flex-[1_1_280px]">
              <svg
                aria-hidden="true"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                strokeWidth="1.8"
                className="pointer-events-none absolute left-3 top-1/2 size-[15px] -translate-y-1/2 text-faint"
              >
                <circle cx="11" cy="11" r="7" />
                <path d="m20 20-3.5-3.5" />
              </svg>
              <input
                type="search"
                autoComplete="off"
                aria-label={pageCopy.toolbar.searchPlaceholder}
                value={searchDraft}
                onChange={(event) => setSearchDraft(event.target.value)}
                placeholder={pageCopy.toolbar.searchPlaceholder}
                className="h-[38px] w-full rounded-md border border-line-2 bg-bg-2 pl-9 pr-3 text-[13px] text-ink transition-colors placeholder:text-faint focus:border-cyan-dim"
              />
            </div>

            <button
              type="button"
              onClick={() => setMobileFiltersOpen(true)}
              className="btn btn-ghost btn-sm min-[1101px]:hidden"
            >
              {pageCopy.toolbar.filters}
            </button>

            <select
              aria-label={pageCopy.toolbar.sortName}
              value={query.sort}
              onChange={(event) => setSort(event.target.value as PricingQuery["sort"])}
              className={toolbarSelectClass}
            >
              <option value="name">{pageCopy.toolbar.sortName}</option>
              <option value="price_asc">{pageCopy.toolbar.sortPriceAsc}</option>
              <option value="price_desc">{pageCopy.toolbar.sortPriceDesc}</option>
            </select>

            <select
              aria-label={groupAriaLabel}
              value={query.group}
              onChange={(event) => setGroup(event.target.value)}
              className={toolbarSelectClass}
            >
              <option value="default">{`${pageCopy.toolbar.groupPrefix}default`}</option>
              {(facets?.groups ?? [])
                .filter((group) => group.value !== "default")
                .map((group) => (
                  <option key={group.value} value={group.value}>
                    {`${pageCopy.toolbar.groupPrefix}${group.label || group.value}${
                      group.ratio !== null ? ` (${formatRatio(group.ratio)})` : ""
                    }`}
                  </option>
                ))}
            </select>

            <div
              role="group"
              className="inline-flex overflow-hidden rounded-md border border-line-2"
            >
              {(["M", "K"] as const).map((value) => (
                <button
                  key={value}
                  type="button"
                  aria-pressed={unit === value}
                  onClick={() => changeUnit(value)}
                  className={`h-[34px] border-l border-line-2 px-2.5 text-xs transition-colors first:border-l-0 ${
                    unit === value
                      ? "bg-[var(--head-tint)] text-ink"
                      : "text-muted hover:text-ink-2"
                  }`}
                >
                  {value === "M" ? pageCopy.toolbar.unitM : pageCopy.toolbar.unitK}
                </button>
              ))}
            </div>

            <button
              type="button"
              aria-pressed={dual}
              onClick={toggleDual}
              className={`h-[34px] rounded-md border px-2.5 text-[11.5px] transition-colors ${
                dual
                  ? "border-cyan/40 bg-cyan/5 text-cyan"
                  : "border-line text-muted hover:border-line-2 hover:text-ink-2"
              }`}
            >
              {pageCopy.toolbar.dual}
            </button>

            <span className="hidden flex-1 min-[761px]:block" />

            <div
              role="group"
              className="hidden overflow-hidden rounded-md border border-line-2 min-[761px]:inline-flex"
            >
              {(["table", "cards"] as const).map((value) => (
                <button
                  key={value}
                  type="button"
                  aria-pressed={view === value}
                  onClick={() => changeView(value)}
                  className={`h-8 border-l border-line-2 px-2.5 text-xs transition-colors first:border-l-0 ${
                    view === value
                      ? "bg-[var(--head-tint)] text-ink"
                      : "text-muted hover:text-ink-2"
                  }`}
                >
                  {value === "table" ? pageCopy.toolbar.table : pageCopy.toolbar.cards}
                </button>
              ))}
            </div>
          </div>

          <div className="flex flex-wrap items-center justify-between gap-3 pb-2.5 pt-3.5">
            <div>
              <p aria-live="polite" className="font-mono text-xs text-faint">
                {pageCopy.states.results.replace("{count}", String(totalSafe))}
                <span className="text-faint"> · </span>
                {query.group === "default"
                  ? `${pageCopy.toolbar.groupPrefix}default`
                  : `${pageCopy.toolbar.groupPrefix}${query.group}${
                      selectedGroupRatio !== null ? ` (${formatRatio(selectedGroupRatio)})` : ""
                    }`}
              </p>
              {activeChips.length > 0 ? (
                <div className="mt-2 flex flex-wrap gap-1.5">
                  {activeChips.map((chip) => (
                    <span
                      key={chip.key}
                      className="inline-flex h-6 items-center gap-1.5 rounded border border-line px-2 text-[11px] text-muted"
                    >
                      {chip.label}
                      <button
                        type="button"
                        aria-label={`${pageCopy.filters.reset}: ${chip.label}`}
                        onClick={chip.onRemove}
                        className="leading-none text-faint transition-colors hover:text-ink"
                      >
                        ×
                      </button>
                    </span>
                  ))}
                </div>
              ) : null}
            </div>
          </div>

          {!catalog ? (
            fetchError ? (
              <div
                role="alert"
                className="rounded-lg border border-dashed border-line-2 px-6 py-14 text-center text-sm text-muted"
              >
                <p>{pageCopy.states.error}</p>
                <button
                  type="button"
                  onClick={() => setReload((value) => value + 1)}
                  className="mt-3 text-[13px] text-cyan underline underline-offset-[3px] transition-colors hover:text-cyan-soft"
                >
                  {pageCopy.states.retry}
                </button>
              </div>
            ) : effectiveView === "cards" ? (
              <CardsSkeleton />
            ) : (
              <TableSkeleton />
            )
          ) : (
            <>
              {fetchError ? (
                <div
                  role="alert"
                  className="mb-3 flex flex-wrap items-center gap-3 rounded-lg border border-line bg-panel px-4 py-2.5 text-[12.5px] text-muted"
                >
                  <span>{pageCopy.states.error}</span>
                  <button
                    type="button"
                    onClick={() => setReload((value) => value + 1)}
                    className="text-cyan transition-colors hover:text-cyan-soft"
                  >
                    {pageCopy.states.retry}
                  </button>
                </div>
              ) : null}

              {items.length === 0 ? (
                <div className="rounded-lg border border-dashed border-line-2 px-6 py-16 text-center text-[13px] text-muted">
                  <p>{pageCopy.states.empty}</p>
                  <button
                    type="button"
                    onClick={clearFilters}
                    className="mt-3 text-[13px] text-cyan underline underline-offset-[3px] transition-colors hover:text-cyan-soft"
                  >
                    {pageCopy.states.emptyAction}
                  </button>
                </div>
              ) : effectiveView === "cards" ? (
                <div className="grid grid-cols-1 gap-3 min-[621px]:grid-cols-2 wide:grid-cols-3">
                  {items.map((model) => (
                    <PricingCard
                      key={model.model_id}
                      model={model}
                      meta={meta}
                      copy={copy}
                      display={display}
                      selected={selected.has(model.model_id)}
                      compareDisabled={
                        selected.size >= MAX_COMPARE && !selected.has(model.model_id)
                      }
                      onToggleCompare={(_modelID, checked) => toggleCompare(model, checked)}
                      onOpenDetail={setDrawerModel}
                    />
                  ))}
                </div>
              ) : (
                <div className="overflow-x-auto rounded-lg border border-line bg-panel">
                  <table className="w-full min-w-[1060px] border-collapse text-[13px]">
                    <caption className="sr-only">{pageCopy.title}</caption>
                    <thead>
                      <tr>
                        <th
                          scope="col"
                          className={`${thBase} sticky left-0 z-10 min-w-[210px]`}
                        >
                          {pageCopy.table.model}
                        </th>
                        <th scope="col" className={thBase}>
                          {pageCopy.table.vendor}
                        </th>
                        <th scope="col" className={thNum}>
                          {pageCopy.table.context}
                        </th>
                        <th scope="col" className={thNum}>
                          {pageCopy.table.input}{" "}
                          {unit === "M" ? pageCopy.toolbar.unitM : pageCopy.toolbar.unitK}
                        </th>
                        <th scope="col" className={thNum}>
                          {pageCopy.table.output}{" "}
                          {unit === "M" ? pageCopy.toolbar.unitM : pageCopy.toolbar.unitK}
                        </th>
                        <th scope="col" className={thNum}>
                          {pageCopy.table.cacheRead}
                        </th>
                        <th scope="col" className={thNum}>
                          {pageCopy.table.cacheWrite}
                        </th>
                        <th scope="col" className={thNum}>
                          {pageCopy.table.perRequest}
                        </th>
                        <th scope="col" className={thNum}>
                          {pageCopy.table.group}
                          {query.group !== "default" && selectedGroupRatio !== null ? (
                            <span className="ml-1 inline-flex h-[18px] items-center rounded-[3px] border border-cyan/30 px-1.5 align-middle text-[10px] text-cyan">
                              {formatRatio(selectedGroupRatio)}
                            </span>
                          ) : null}
                        </th>
                        <th scope="col" className={`${thBase} text-right`}>
                          {pageCopy.table.actions}
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {items.map((model) => {
                        const isExpanded = expandedModelID === model.model_id;
                        const isSelected = selected.has(model.model_id);
                        const compareDisabled = selected.size >= MAX_COMPARE && !isSelected;
                        return (
                          <Fragment key={model.model_id}>
                            <tr
                              className="group/row cursor-pointer transition-colors hover:bg-[var(--hover-tint)]"
                              onClick={(event) => handleRowClick(event, model.model_id)}
                            >
                              <td
                                className={`${cellBase} sticky left-0 z-[5] min-w-[210px] bg-panel group-hover/row:bg-panel-2`}
                              >
                                <span className="flex items-start gap-2.5">
                                  <i
                                    aria-hidden="true"
                                    className="mt-[7px] size-[7px] flex-none rounded-full bg-cyan-dim"
                                  />
                                  <span className="min-w-0">
                                    <span className="block text-[13px] font-semibold leading-[1.35] text-ink">
                                      {model.display_name || model.model_id}
                                    </span>
                                    <span className="mt-px block font-mono text-[10.5px] text-faint">
                                      {model.model_id}
                                    </span>
                                    {modelBadges(model)}
                                  </span>
                                </span>
                              </td>
                              <td className={`${cellBase} whitespace-nowrap text-[12.5px] text-ink-2`}>
                                {model.vendor}
                              </td>
                              <td
                                className={`${cellBase} whitespace-nowrap text-right font-mono text-xs text-ink-2`}
                              >
                                {formatContext(model.context_length)}
                              </td>
                              <td className={`${cellBase} whitespace-nowrap text-right`}>
                                {model.mode === "per_token"
                                  ? priceNode(model.prompt_price, model)
                                  : dashNode()}
                              </td>
                              <td className={`${cellBase} whitespace-nowrap text-right`}>
                                {model.mode === "per_token"
                                  ? priceNode(model.completion_price, model)
                                  : dashNode()}
                              </td>
                              <td className={`${cellBase} whitespace-nowrap text-right`}>
                                {model.mode === "per_token"
                                  ? priceNode(model.cache_read_price, model)
                                  : dashNode()}
                              </td>
                              <td className={`${cellBase} whitespace-nowrap text-right`}>
                                {model.mode === "per_token"
                                  ? priceNode(model.cache_write_price, model)
                                  : dashNode()}
                              </td>
                              <td className={`${cellBase} whitespace-nowrap text-right`}>
                                {model.mode === "per_request"
                                  ? perRequestNode(model.prompt_price, model, false)
                                  : dashNode()}
                              </td>
                              <td className={`${cellBase} whitespace-nowrap text-right`}>
                                {groupNode(model)}
                              </td>
                              <td className={`${cellBase} whitespace-nowrap text-right`}>
                                <span className="inline-flex items-center gap-1.5">
                                  <label className="inline-flex cursor-pointer items-center gap-1.5 text-[11px] text-faint">
                                    <input
                                      type="checkbox"
                                      checked={isSelected}
                                      disabled={compareDisabled}
                                      aria-label={`${pageCopy.table.compare} ${
                                        model.display_name || model.model_id
                                      }`}
                                      onChange={(event) =>
                                        toggleCompare(model, event.target.checked)
                                      }
                                      className="size-[13px] accent-cyan disabled:cursor-not-allowed disabled:opacity-40"
                                    />
                                    {pageCopy.table.compare}
                                  </label>
                                  <button
                                    type="button"
                                    aria-expanded={isExpanded}
                                    aria-label={pageCopy.table.expand}
                                    onClick={(event) => {
                                      event.stopPropagation();
                                      toggleExpanded(model.model_id);
                                    }}
                                    className="inline-flex size-[26px] items-center justify-center rounded-[5px] border border-line-2 text-muted transition-colors hover:border-cyan-dim hover:text-ink"
                                  >
                                    <svg
                                      width="12"
                                      height="12"
                                      viewBox="0 0 24 24"
                                      fill="none"
                                      stroke="currentColor"
                                      strokeWidth="2"
                                      strokeLinecap="round"
                                      aria-hidden="true"
                                      className={`transition-transform ${
                                        isExpanded ? "rotate-180" : ""
                                      }`}
                                    >
                                      <path d="m6 9 6 6 6-6" />
                                    </svg>
                                  </button>
                                </span>
                              </td>
                            </tr>
                            {isExpanded ? (
                              <tr>
                                <td
                                  colSpan={10}
                                  className="border-b border-line bg-bg-2 p-0"
                                >
                                  <PricingDetail
                                    model={model}
                                    meta={meta}
                                    copy={copy}
                                    display={display}
                                  />
                                </td>
                              </tr>
                            ) : null}
                          </Fragment>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              )}

              <div className="flex flex-wrap items-center justify-between gap-3 pt-4">
                <div className="flex flex-wrap items-center gap-2 text-[11.5px] text-faint">
                  <span>{rangeText}</span>
                  <span aria-hidden="true">·</span>
                  <label className="inline-flex items-center gap-1.5">
                    {pageCopy.states.perPage}
                    <select
                      value={query.pageSize}
                      onChange={(event) => setPageSize(Number(event.target.value))}
                      className="h-7 rounded border border-line-2 bg-bg-2 px-1.5 text-xs text-ink"
                    >
                      {PAGE_SIZES.map((size) => (
                        <option key={size} value={size}>
                          {size}
                        </option>
                      ))}
                    </select>
                  </label>
                </div>
                <div className="flex flex-wrap items-center gap-1.5">
                  <button
                    type="button"
                    disabled={query.page <= 1}
                    onClick={() => goToPage(query.page - 1)}
                    className="h-[30px] rounded-[5px] border border-line-2 px-2 text-xs text-muted transition-colors hover:text-ink disabled:cursor-not-allowed disabled:opacity-40"
                  >
                    {pageCopy.states.previous}
                  </button>
                  {pageNumbers.map((entry, index) =>
                    entry === "gap" ? (
                      <span key={`gap-${index}`} className="px-1 text-xs text-faint">
                        …
                      </span>
                    ) : (
                      <button
                        key={entry}
                        type="button"
                        aria-current={entry === query.page ? "page" : undefined}
                        onClick={() => goToPage(entry)}
                        className={`h-[30px] min-w-[30px] rounded-[5px] border px-2 font-mono text-xs transition-colors ${
                          entry === query.page
                            ? "border-cyan-dim text-cyan"
                            : "border-line-2 text-muted hover:text-ink"
                        }`}
                      >
                        {entry}
                      </button>
                    ),
                  )}
                  <button
                    type="button"
                    disabled={query.page >= pageCount}
                    onClick={() => goToPage(query.page + 1)}
                    className="h-[30px] rounded-[5px] border border-line-2 px-2 text-xs text-muted transition-colors hover:text-ink disabled:cursor-not-allowed disabled:opacity-40"
                  >
                    {pageCopy.states.next}
                  </button>
                </div>
              </div>

              <p className="mt-4 text-[11.5px] text-faint">{pageCopy.footnote}</p>
            </>
          )}
        </section>
      </div>

      {selected.size >= 2 ? (
        <div
          role="region"
          aria-label={pageCopy.compare.title}
          className="fixed bottom-5 left-1/2 z-[70] flex w-[min(760px,calc(100%-32px))] -translate-x-1/2 flex-wrap items-center gap-3 rounded-[9px] border border-line-2 bg-panel px-[18px] py-[11px] panel-shadow"
        >
          <p className="mr-auto text-[12.5px] text-ink-2">
            {pageCopy.compare.selected}{" "}
            <b className="font-mono font-medium text-ink">{selected.size}</b>{" "}
            {pageCopy.compare.unit}
            <span className="text-faint">
              {" · "}
              {pageCopy.compare.max.replace("{count}", String(MAX_COMPARE))}
            </span>
          </p>
          <button
            type="button"
            onClick={() => setCompareOpen(true)}
            className="btn btn-primary btn-sm"
          >
            {pageCopy.compare.open}
          </button>
          <button
            type="button"
            onClick={clearCompare}
            className="text-xs text-muted transition-colors hover:text-ink"
          >
            {pageCopy.compare.clear}
          </button>
        </div>
      ) : null}

      <PricingCompareDialog
        models={selectedModels}
        meta={meta}
        copy={copy}
        display={display}
        open={compareOpen}
        onOpenChange={setCompareOpen}
        onClear={clearCompare}
      />

      {drawerModel ? (
        <>
          <div
            aria-hidden="true"
            onClick={() => setDrawerModel(null)}
            className="fixed inset-0 z-[80] bg-[var(--overlay)]"
          />
          <aside
            role="dialog"
            aria-modal="true"
            aria-label={`${drawerModel.display_name || drawerModel.model_id} · ${
              pageCopy.detail.register
            }`}
            className="fixed inset-y-0 right-0 z-[90] w-[min(720px,100%)] overflow-y-auto border-l border-line-2 bg-bg"
          >
            <div className="sticky top-0 z-[2] flex items-center justify-between gap-3 border-b border-line bg-bg px-5 py-3.5">
              <h2 className="text-sm font-semibold text-ink">
                {drawerModel.display_name || drawerModel.model_id} · {pageCopy.detail.register}
              </h2>
              <button
                type="button"
                onClick={() => setDrawerModel(null)}
                aria-label={pageCopy.filters.close}
                className="inline-flex size-[30px] items-center justify-center rounded-[5px] border border-line-2 text-muted transition-colors hover:text-ink"
              >
                ×
              </button>
            </div>
            <div className="p-5">
              <PricingDetail model={drawerModel} meta={meta} copy={copy} display={display} />
            </div>
          </aside>
        </>
      ) : null}
    </div>
  );
}
