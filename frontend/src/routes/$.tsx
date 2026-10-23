import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { NotFoundPage } from '@/app/not-found-page'

export const Route = createFileRoute('/$')({
  loader: () => loadRouteDictionary('public'),
  component: NotFoundPage,
})
