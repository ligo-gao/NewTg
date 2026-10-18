import { Link } from '@tanstack/react-router'

import { useI18n } from '@/shared/lib/i18n-context'

export interface ErrorStatusPageProps {
  status: 401 | 403
  /** Origin path preserved for `/401` so sign-in can return the user there. */
  redirect?: string
}

/**
 * Shared body for the §8.3 credential-plane error pages. Copy lives in the
 * `common` domain because these routes sit directly under the root and the
 * always-loaded dictionary must cover them (a console domain dictionary may
 * not be loaded yet).
 */
export function ErrorStatusPage({ status, redirect }: ErrorStatusPageProps) {
  const { t } = useI18n()
  const title =
    status === 401 ? t('common.error401.title') : t('common.error403.title')
  const body =
    status === 401 ? t('common.error401.body') : t('common.error403.body')

  return (
    <main
      id="main"
      className="flex min-h-screen flex-col items-center justify-center gap-3 bg-surface-page p-6 text-center text-ink-1"
    >
      <h1 className="text-3xl font-bold">{status}</h1>
      <p className="text-sm font-medium">{title}</p>
      <p className="max-w-prose text-sm text-ink-2">{body}</p>
      {status === 401 ? (
        <Link
          to="/sign-in"
          search={{ redirect }}
          className="text-sm font-medium text-brand underline-offset-2 hover:text-brand-hover hover:underline"
        >
          {t('common.error401.sign_in')}
        </Link>
      ) : (
        <Link
          to="/dashboard"
          className="text-sm font-medium text-brand underline-offset-2 hover:text-brand-hover hover:underline"
        >
          {t('common.error403.back')}
        </Link>
      )}
    </main>
  )
}
