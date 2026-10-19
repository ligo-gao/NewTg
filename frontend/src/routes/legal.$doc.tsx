import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { NotFoundPage } from '@/app/not-found-page'
import { LegalPage } from '@/features/auth/LegalPage'

export const Route = createFileRoute('/legal/$doc')({
  loader: () => loadRouteDictionary('auth'),
  component: LegalRoute,
})

function LegalRoute() {
  const { doc } = Route.useParams()
  if (doc !== 'terms' && doc !== 'privacy') {
    return <NotFoundPage />
  }
  return <LegalPage doc={doc} />
}
