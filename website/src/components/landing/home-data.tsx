"use client";

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { pickRandom } from "@/lib/pick-random";
import { isChatCapable } from "@/lib/pricing-format";
import {
  fetchModelMetrics,
  fetchPricingCatalog,
  type ModelMetrics,
  type PricingMeta,
  type PricingModel,
  type PricingVendorFacet,
} from "@/lib/pricing-api";

export type LoadStatus = "loading" | "ready" | "error";
export type MetricsStatus = "idle" | "loading" | "ready" | "error";

export interface HomeData {
  catalogStatus: LoadStatus;
  metricsStatus: MetricsStatus;
  models: PricingModel[];
  featured: PricingModel[];
  perfFeatured: PricingModel[];
  meta: PricingMeta | null;
  vendors: PricingVendorFacet[];
  metrics: Record<string, ModelMetrics>;
  retry: () => void;
}

const HomeDataContext = createContext<HomeData | null>(null);

/**
 * Loads the public model catalog once and keeps two random samples:
 * `featured` (any model) for the model grid, and `perfFeatured` (chat-capable
 * only) for the performance table, since the scheduled probe only tests
 * chat models. Metrics are fetched per `perfFeatured` model; a partial
 * failure leaves those rows at "—" instead of inventing values.
 */
export function HomeDataProvider({ children }: { children: ReactNode }) {
  const [catalogStatus, setCatalogStatus] = useState<LoadStatus>("loading");
  const [metricsStatus, setMetricsStatus] = useState<MetricsStatus>("idle");
  const [models, setModels] = useState<PricingModel[]>([]);
  const [featured, setFeatured] = useState<PricingModel[]>([]);
  const [perfFeatured, setPerfFeatured] = useState<PricingModel[]>([]);
  const [meta, setMeta] = useState<PricingMeta | null>(null);
  const [vendors, setVendors] = useState<PricingVendorFacet[]>([]);
  const [metrics, setMetrics] = useState<Record<string, ModelMetrics>>({});
  const [reloadKey, setReloadKey] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    let active = true;

    setCatalogStatus("loading");
    setMetricsStatus("idle");
    setMetrics({});
    setModels([]);
    setFeatured([]);
    setPerfFeatured([]);

    fetchPricingCatalog(controller.signal)
      .then((catalog) => {
        if (!active) {
          return;
        }
        const picked = pickRandom(catalog.items, 8);
        const perfPicked = pickRandom(catalog.items.filter(isChatCapable), 8);
        setModels(catalog.items);
        setFeatured(picked);
        setPerfFeatured(perfPicked);
        setMeta(catalog.meta);
        setVendors(catalog.facets?.vendors ?? []);
        setCatalogStatus("ready");
        if (perfPicked.length === 0) {
          setMetricsStatus("ready");
          return;
        }

        setMetricsStatus("loading");
        return Promise.allSettled(
          perfPicked.map((model) => fetchModelMetrics(model.model_id, controller.signal)),
        ).then((results) => {
          if (!active) {
            return;
          }
          const next: Record<string, ModelMetrics> = {};
          let failures = 0;
          results.forEach((result, index) => {
            if (result.status === "fulfilled") {
              next[perfPicked[index].model_id] = result.value;
            } else {
              failures += 1;
            }
          });
          setMetrics(next);
          setMetricsStatus(failures === perfPicked.length ? "error" : "ready");
        });
      })
      .catch(() => {
        if (!active || controller.signal.aborted) {
          return;
        }
        setCatalogStatus("error");
        setMetricsStatus("idle");
      });

    return () => {
      active = false;
      controller.abort();
    };
  }, [reloadKey]);

  const retry = useCallback(() => {
    setReloadKey((key) => key + 1);
  }, []);

  const value = useMemo<HomeData>(
    () => ({
      catalogStatus,
      metricsStatus,
      models,
      featured,
      perfFeatured,
      meta,
      vendors,
      metrics,
      retry,
    }),
    [catalogStatus, metricsStatus, models, featured, perfFeatured, meta, vendors, metrics, retry],
  );

  return <HomeDataContext.Provider value={value}>{children}</HomeDataContext.Provider>;
}

export function useHomeData(): HomeData {
  const value = useContext(HomeDataContext);
  if (value === null) {
    throw new Error("useHomeData must be used inside HomeDataProvider");
  }
  return value;
}
