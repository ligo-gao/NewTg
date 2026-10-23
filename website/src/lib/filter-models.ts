import type { Model, VendorId } from "@/config/models";

export interface ModelFilters {
  query: string;
  vendor: VendorId | "all";
}

export function filterModels(list: readonly Model[], { query, vendor }: ModelFilters): Model[] {
  const normalizedQuery = query.trim().toLowerCase();

  return list.filter((model) => {
    if (vendor !== "all" && model.vendor !== vendor) {
      return false;
    }

    if (normalizedQuery === "") {
      return true;
    }

    return (
      model.name.toLowerCase().includes(normalizedQuery) ||
      model.vendorName.toLowerCase().includes(normalizedQuery)
    );
  });
}
