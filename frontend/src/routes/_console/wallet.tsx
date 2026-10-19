import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { TopupPage } from '@/features/account/topup/topup-page'

export const Route = createFileRoute('/_console/wallet')({
  loader: () => loadRouteDictionary('account'),
  component: TopupPage,
})
