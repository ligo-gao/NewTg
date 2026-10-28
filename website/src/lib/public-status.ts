"use client";

import { useEffect, useState } from "react";
import { isNavEntryVisible, type PublicNavSwitches } from "@newtg/public-shell";

/** The `site` block the public chrome reads; every field is optional in the API. */
export interface PublicStatusSite {
  title?: string;
  about_html?: string;
  docs_url?: string;
  status_page_url?: string;
  server_address?: string;
  filing_no?: string;
}

/**
 * The subset of `GET /api/status` the marketing site reads.
 *
 * `nav` is the same switch block the SPA consumes; the header resolves its
 * entries through `visibleNavItems()` from `@newtg/public-shell`, so both apps
 * answer "which entries exist" with one function instead of two lists.
 */
export interface PublicStatus {
  nav?: PublicNavSwitches;
  site?: PublicStatusSite;
}

export interface PublicStatusState {
  status: PublicStatus | undefined;
  loading: boolean;
  error: Error | undefined;
  /** Server nav switches; `undefined` means "not loaded yet" → documented defaults. */
  nav: PublicNavSwitches | undefined;
  /** Trimmed `site.docs_url`; empty string when unconfigured. */
  docsUrl: string;
  /** Whether the docs entry point is configured and enabled. */
  docsEnabled: boolean;
  /** Trimmed `site.status_page_url`; empty string when unconfigured. */
  statusPageUrl: string;
  /** Trimmed `site.server_address`; the origin the API is served from. */
  serverAddress: string;
  /** Trimmed `site.title`; empty string when unconfigured (callers fall back). */
  siteTitle: string;
}

/**
 * Docs are reachable only when the nav entry is not disabled and the backend
 * exposes a safe, non-empty `site.docs_url`. A dead docs link is worse than one
 * that briefly lags the settings, so anything unproven defaults to hidden.
 * The rule itself is the shared one — no second copy to drift.
 */
export function resolveDocsEnabled(status: PublicStatus | undefined): boolean {
  return isNavEntryVisible(status?.nav, "docs", false, status?.site?.docs_url);
}

interface StatusSnapshot {
  status: PublicStatus | undefined;
  error: Error | undefined;
  loading: boolean;
}

const INITIAL_SNAPSHOT: StatusSnapshot = { status: undefined, error: undefined, loading: true };

/**
 * 页面级共享状态。header、footer、hero CTA 同时挂载时只发一次
 * `GET /api/status`；成功或失败都留在这个模块里，直到整页卸载。
 * 请求不再随单个消费者卸载而中止——同页的其他消费者还在用它。
 */
let snapshot: StatusSnapshot = INITIAL_SNAPSHOT;
let inflight: Promise<void> | null = null;
const listeners = new Set<() => void>();

function publish(next: StatusSnapshot): void {
  snapshot = next;
  listeners.forEach((listener) => listener());
}

function loadStatus(): void {
  if (inflight !== null || !snapshot.loading) return;

  inflight = fetch("/api/status", {
    cache: "no-store",
    headers: { Accept: "application/json" },
    credentials: "omit",
  })
    .then(async (response) => {
      if (!response.ok) {
        throw new Error(`status request failed: ${response.status}`);
      }
      return (await response.json()) as PublicStatus;
    })
    .then((data) => {
      publish({ status: data, error: undefined, loading: false });
    })
    .catch((cause: unknown) => {
      publish({
        status: undefined,
        error: cause instanceof Error ? cause : new Error(String(cause)),
        loading: false,
      });
    })
    .finally(() => {
      inflight = null;
    });
}

/** 测试用：清空模块级缓存，让下一个用例从「未加载」开始。 */
export function resetPublicStatusCache(): void {
  snapshot = INITIAL_SNAPSHOT;
  inflight = null;
}

/** Fetches the public site status once on mount. */
export function usePublicStatus(): PublicStatusState {
  const [state, setState] = useState<StatusSnapshot>(() => snapshot);

  useEffect(() => {
    // 首次渲染与 effect 之间快照可能已更新（另一个消费者先挂载并拿到了响应）。
    setState(snapshot);
    const listener = () => setState(snapshot);
    listeners.add(listener);
    loadStatus();
    return () => {
      listeners.delete(listener);
    };
  }, []);

  return {
    status: state.status,
    loading: state.loading,
    error: state.error,
    nav: state.status?.nav,
    docsUrl: (state.status?.site?.docs_url ?? "").trim(),
    docsEnabled:
      !state.loading && state.error === undefined && resolveDocsEnabled(state.status),
    statusPageUrl: (state.status?.site?.status_page_url ?? "").trim(),
    serverAddress: (state.status?.site?.server_address ?? "").trim(),
    siteTitle: (state.status?.site?.title ?? "").trim(),
  };
}
