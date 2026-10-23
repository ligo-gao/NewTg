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
  meta: PricingMeta | null;
  vendors: PricingVendorFacet[];
  metrics: Record<string, ModelMetrics>;
  retry: () => void;
}

const HomeDataContext = createContext<HomeData | null>(null);

/**
 * Loads the public model catalog once and keeps the same random six models
 * for both the model grid and the performance table. Metrics are fetched
 * per featured model; a partial failure leaves those rows at "—" instead of
 * inventing values.
 */
export function HomeDataProvider({ children }: { children: ReactNode }) {
  const [catalogStatus, setCatalogStatus] = useState<LoadStatus>("loading");
  const [metricsStatus, setMetricsStatus] = useState<MetricsStatus>("idle");
  const [models, setModels] = useState<PricingModel[]>([]);
  const [featured, setFeatured] = useState<PricingModel[]>([]);
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

    fetchPricingCatalog(controller.signal)
      .then((catalog) => {
        if (!active) {
          return;
        }
        const picked = pickRandom(catalog.items, 6);
        setModels(catalog.items);
        setFeatured(picked);
        setMeta(catalog.meta);
        setVendors(catalog.facets?.vendors ?? []);
        setCatalogStatus("ready");
        if (picked.length === 0) {
          setMetricsStatus("ready");
          return;
        }

        setMetricsStatus("loading");
        return Promise.allSettled(
          picked.map((model) => fetchModelMetrics(model.model_id, controller.signal)),
        ).then((results) => {
          if (!active) {
            return;
          }
          const next: Record<string, ModelMetrics> = {};
          let failures = 0;
          results.forEach((result, index) => {
            if (result.status === "fulfilled") {
              next[picked[index].model_id] = result.value;
            } else {
              failures += 1;
            }
          });
          setMetrics(next);
          setMetricsStatus(failures === picked.length ? "error" : "ready");
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
      meta,
      vendors,
      metrics,
      retry,
    }),
    [catalogStatus, metricsStatus, models, featured, meta, vendors, metrics, retry],
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
