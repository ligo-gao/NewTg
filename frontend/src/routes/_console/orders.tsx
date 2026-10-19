import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { OrderListPage } from '@/features/account/topup/order-list-page'

export const Route = createFileRoute('/_console/orders')({
  loader: () => loadRouteDictionary('account'),
  component: OrderListPage,
})
