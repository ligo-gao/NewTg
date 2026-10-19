import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { PublicLayout } from '@/features/public/public-layout'
import { useI18n } from '@/shared/lib/i18n-context'

export const Route = createFileRoute('/')({
  loader: () => loadRouteDictionary('public'),
  component: HomePage,
})

function HomePage() {
  const { t } = useI18n()

  return (
    <PublicLayout activePath="/">
      <div className="flex flex-col gap-2">
        <p className="text-xs font-medium uppercase tracking-wide text-ink-3">
          {t('public.nav.brand')}
        </p>
        <h1 className="text-xl font-semibold">{t('public.home.tagline')}</h1>
        <p className="max-w-prose text-sm text-ink-2">
          {t('public.home.placeholder')}
        </p>
      </div>
    </PublicLayout>
  )
}
