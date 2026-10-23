import type { Copy } from "@/config/copy";
import type { PricingMeta, PricingModel } from "@/lib/pricing-api";

export type TokenUnit = "M" | "K";

export interface PricingDisplay {
  unit: TokenUnit;
  dual: boolean;
  group: string;
}

export interface PricingDetailProps {
  model: PricingModel;
  meta: PricingMeta;
  copy: Copy;
  display: PricingDisplay;
}

export interface PricingCardProps {
  model: PricingModel;
  meta: PricingMeta;
  copy: Copy;
  display: PricingDisplay;
  selected: boolean;
  compareDisabled: boolean;
  onToggleCompare: (modelID: string, checked: boolean) => void;
  onOpenDetail: (model: PricingModel) => void;
}

export interface PricingCompareDialogProps {
  models: PricingModel[];
  meta: PricingMeta;
  copy: Copy;
  display: PricingDisplay;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onClear: () => void;
}
