import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { GroupPricingListPage } from '@/features/admin/pricing/GroupPricingListPage'

export const Route = createFileRoute('/_console/admin/pricing/group-pricing')({
  loader: () => loadRouteDictionary('admin'),
  component: GroupPricingListPage,
})
