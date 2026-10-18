import { createFileRoute } from '@tanstack/react-router'

import { loadDictionary } from '@/app/providers/i18n'
import { GroupPricingListPage } from '@/features/admin/pricing/GroupPricingListPage'

export const Route = createFileRoute('/_console/admin/pricing/group-pricing')({
  loader: () => loadDictionary('en', 'admin'),
  component: GroupPricingListPage,
})
