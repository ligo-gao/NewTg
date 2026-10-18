import { Link } from '@tanstack/react-router'

import { useI18n } from '@/shared/lib/i18n-context'

/**
 * Shared 404 body (web-00 §8.3: the error pages share one public skeleton).
 * Both the `/404` route and the router's `notFoundComponent` render this, so
 * the two entry points cannot drift apart as the error shell gains the shared
 * graphic/request_id treatment in a later milestone.
 *
 * Copy comes from `common.not_found.*` because both entry points render inside
 * `I18nProvider`. (ErrorFallback stays hardcoded: the global boundary sits
 * outside the providers, where no dictionary is guaranteed to be loaded.)
 */
export function NotFoundPage() {
  const { t } = useI18n()
  return (
    <main
      id="main"
      className="flex min-h-screen flex-col items-center justify-center gap-3 bg-surface-page p-6 text-center text-ink-1"
    >
      <h1 className="text-3xl font-bold">404</h1>
      <p className="text-sm text-ink-2">{t('common.not_found.title')}</p>
      <Link
        to="/"
        className="text-sm font-medium text-brand underline-offset-2 hover:text-brand-hover hover:underline"
      >
        {t('common.not_found.action')}
      </Link>
    </main>
  )
}
