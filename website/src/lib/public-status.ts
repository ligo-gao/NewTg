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

/** Fetches the public site status once on mount. */
export function usePublicStatus(): PublicStatusState {
  const [status, setStatus] = useState<PublicStatus | undefined>(undefined);
  const [error, setError] = useState<Error | undefined>(undefined);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    const controller = new AbortController();
    let active = true;

    async function load() {
      try {
        const response = await fetch("/api/status", {
          cache: "no-store",
          headers: { Accept: "application/json" },
          credentials: "omit",
          signal: controller.signal,
        });
        if (!response.ok) {
          throw new Error(`status request failed: ${response.status}`);
        }
        const data = (await response.json()) as PublicStatus;
        if (!active) return;
        setStatus(data);
      } catch (cause) {
        if (!active) return;
        setError(cause instanceof Error ? cause : new Error(String(cause)));
      } finally {
        if (active) setLoading(false);
      }
    }

    void load();

    return () => {
      active = false;
      controller.abort();
    };
  }, []);

  return {
    status,
    loading,
    error,
    nav: status?.nav,
    docsUrl: (status?.site?.docs_url ?? "").trim(),
    docsEnabled: !loading && error === undefined && resolveDocsEnabled(status),
    statusPageUrl: (status?.site?.status_page_url ?? "").trim(),
    serverAddress: (status?.site?.server_address ?? "").trim(),
    siteTitle: (status?.site?.title ?? "").trim(),
  };
}
