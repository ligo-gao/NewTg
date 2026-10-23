import type { PricingModel } from "@/lib/pricing-api";

export interface PricingFilters {
  query: string;
  vendor: string;
}

export function filterPricingModels(
  list: readonly PricingModel[],
  { query, vendor }: PricingFilters,
): PricingModel[] {
  const normalizedQuery = query.trim().toLowerCase();
  const normalizedVendor = vendor.toLowerCase();

  return list.filter((model) => {
    if (normalizedVendor !== "all" && model.vendor.toLowerCase() !== normalizedVendor) {
      return false;
    }
    if (normalizedQuery === "") {
      return true;
    }
    return (
      model.display_name.toLowerCase().includes(normalizedQuery) ||
      model.model_id.toLowerCase().includes(normalizedQuery) ||
      model.vendor.toLowerCase().includes(normalizedQuery)
    );
  });
}
