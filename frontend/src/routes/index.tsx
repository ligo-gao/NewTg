import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { HomePage } from '@/features/public/home/home-page'
import { PublicLayout } from '@/features/public/public-layout'

export const Route = createFileRoute('/')({
  loader: () => loadRouteDictionary('public'),
  component: HomeRoute,
})

function HomeRoute() {
  return (
    // `contained={false}`: F-09-01..05 sections own their full-bleed surfaces
    // and vertical rhythm (UI design §0.7).
    <PublicLayout activePath="/" contained={false}>
      <HomePage />
    </PublicLayout>
  )
}
