"use client";

import { useEffect, useState } from "react";

/** `PublicNavItem` from `GET /api/status`; only the docs flag is consumed here. */
export interface PublicStatusNavItem {
  enabled?: boolean;
  require_auth?: boolean;
}

/**
 * The subset of the `GET /api/status` payload the marketing site reads. The
 * endpoint is same-origin and never cached, so the docs entry can follow the
 * live deployment settings.
 */
export interface PublicStatus {
  nav?: {
    docs?: PublicStatusNavItem;
  };
  site?: {
    docs_url?: string;
  };
}

export interface PublicStatusState {
  status: PublicStatus | undefined;
  loading: boolean;
  error: Error | undefined;
  /** Whether the docs entry point is configured and enabled. */
  docsEnabled: boolean;
}

/**
 * Docs are reachable only when the nav entry is not disabled and the backend
 * exposes a non-empty `site.docs_url`. A dead docs link is worse than one that
 * briefly lags the settings, so anything unproven defaults to hidden.
 */
export function resolveDocsEnabled(status: PublicStatus | undefined): boolean {
  if (!status) return false;
  const navEnabled = status.nav?.docs?.enabled !== false;
  const docsUrl = status.site?.docs_url;
  return navEnabled && typeof docsUrl === "string" && docsUrl.trim() !== "";
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
    docsEnabled: !loading && error === undefined && resolveDocsEnabled(status),
  };
}
