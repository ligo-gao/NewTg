import { createFileRoute } from '@tanstack/react-router'

import { TokenListPage } from '@/features/tokens/token-list-page'

export const Route = createFileRoute('/_console/tokens')({
  component: TokenListPage,
})
