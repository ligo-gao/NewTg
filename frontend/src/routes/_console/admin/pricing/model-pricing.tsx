import { createFileRoute } from '@tanstack/react-router'

import { loadDictionary } from '@/app/providers/i18n'
import { ModelPricingListPage } from '@/features/admin/pricing/ModelPricingListPage'

export const Route = createFileRoute('/_console/admin/pricing/model-pricing')({
  loader: () => loadDictionary('en', 'admin'),
  component: ModelPricingListPage,
})
