import type { PublicNav, PublicSite } from '@/lib/public-status'
import { useI18n } from '@/shared/lib/i18n-context'

import { isSafePublicHref, navEntryOf } from '../nav-visibility'
import { SiteBrand } from '../public-header'

export interface HeroSectionProps {
  site?: PublicSite
  nav?: PublicNav
  isLoggedIn: boolean
  /** `/api/status` still in flight — render the documented skeleton. */
  pending?: boolean
  /** `/api/status` failed — fall back to built-in copy plus a retry link. */
  failed?: boolean
  onRetry?: () => void
}

/**
 * F-09-01 Hero. Left aligned, no illustration (UI design §0.1). The CTA pair
 * is the only place the guest/registered split is visible: guests get
 * Sign up / Pricing, signed-in visitors get Console / Docs. Pricing stays
 * hidden when the operator disabled it; Docs stays hidden without a URL.
 * `/api/me` failures were already folded into "guest" by the caller (401 is
 * the guest answer; F-09-01 异常流程 treats any probe failure as guest).
 */
export function HeroSection({
  site,
  nav,
  isLoggedIn,
  pending = false,
  failed = false,
  onRetry,
}: HeroSectionProps) {
  const { t } = useI18n()
  const rawDocsUrl = (site?.docs_url ?? '').trim()
  // Same allowlist as the header/footer: never emit `javascript:`/`data:` or a
  // protocol-relative href even if a stale row bypassed server validation.
  const docsUrl = isSafePublicHref(rawDocsUrl) ? rawDocsUrl : ''
  const pricingEnabled = navEntryOf(nav, 'pricing').enabled

  return (
    <section className="mx-auto w-full max-w-[1200px] px-4 pb-12 pt-16 md:px-6 md:pb-16 md:pt-24">
      <SiteBrand site={site} className="mb-6" />
      {pending ? (
        <div aria-hidden="true" className="max-w-[640px]">
          <div className="h-10 w-3/4 rounded bg-ink-4/30 md:h-12" />
          <div className="mt-3 h-10 w-1/2 rounded bg-ink-4/30 md:h-12" />
          <div className="mt-4 h-4 w-full rounded bg-ink-4/20" />
          <div className="mt-2 h-4 w-2/3 rounded bg-ink-4/20" />
          <div className="mt-8 flex gap-3">
            <div className="h-11 w-32 rounded-lg bg-ink-4/30" />
            <div className="h-11 w-28 rounded-lg bg-ink-4/20" />
          </div>
        </div>
      ) : (
        <>
          <h1
            tabIndex={-1}
            className="max-w-[640px] text-[28px] font-semibold leading-9 text-ink-1 md:text-[32px] md:leading-10 lg:text-[40px] lg:leading-[48px]"
          >
            {t('public.home.hero.title')}
          </h1>
          <p className="mt-4 max-w-[640px] text-[18px] leading-7 text-ink-2">
            {t('public.home.hero.subtitle')}
          </p>
          <div className="mt-8 flex flex-col gap-3 sm:flex-row">
            {isLoggedIn ? (
              <>
                <a
                  href="/dashboard"
                  className="inline-flex h-11 items-center justify-center rounded-lg bg-brand px-6 text-[15px] font-semibold text-on-brand hover:bg-brand-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                >
                  {t('public.home.hero.cta_console')}
                </a>
                {docsUrl !== '' ? (
                  <a
                    href={docsUrl}
                    target={/^https?:\/\//i.test(docsUrl) ? '_blank' : undefined}
                    rel={/^https?:\/\//i.test(docsUrl) ? 'noopener noreferrer' : undefined}
                    className="inline-flex h-11 items-center justify-center rounded-lg border border-border-default bg-surface-card px-5 text-[15px] font-semibold text-ink-1 hover:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                  >
                    {t('public.home.hero.cta_docs')}
                  </a>
                ) : null}
              </>
            ) : (
              <>
                <a
                  href="/sign-up"
                  className="inline-flex h-11 items-center justify-center rounded-lg bg-brand px-6 text-[15px] font-semibold text-on-brand hover:bg-brand-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                >
                  {t('public.home.hero.cta_start')}
                </a>
                {pricingEnabled ? (
                  <a
                    href="/pricing"
                    className="inline-flex h-11 items-center justify-center rounded-lg border border-border-default bg-surface-card px-5 text-[15px] font-semibold text-ink-1 hover:border-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                  >
                    {t('public.home.hero.cta_pricing')}
                  </a>
                ) : null}
              </>
            )}
          </div>
        </>
      )}
      {failed ? (
        <p className="mt-6 text-xs text-ink-2">
          <button
            type="button"
            onClick={() => onRetry?.()}
            className="rounded text-ink-2 underline-offset-2 hover:text-brand hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {t('public.home.hero.retry')}
          </button>
        </p>
      ) : null}
    </section>
  )
}
