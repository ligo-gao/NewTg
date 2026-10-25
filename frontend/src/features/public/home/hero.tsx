import type { PublicNav, PublicSite } from '@/lib/public-status'
import { useI18n } from '@/shared/lib/i18n-context'

import { isSafePublicHref, navEntryOf } from '../nav-visibility'
import { TRACE_STAGES } from '../content'

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
 * F-09-01 hero. Left aligned; the right-hand instrument is the page's single
 * bold element: one request's path through auth, quota, routing, protocol
 * translation, streaming and settlement.
 *
 * The CTA pair is the only place the guest/registered split is visible. The
 * API-docs entry is deliberately stable: it targets `site.docs_url` when the
 * operator configured one, otherwise the internal `/docs` route (which
 * reports "docs are not available yet" until a URL is set). Pricing and docs
 * still honour their nav switches.
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
  const pricingEnabled = navEntryOf(nav, 'pricing').enabled
  const docsEntry = navEntryOf(nav, 'docs')
  const rawDocsUrl = (site?.docs_url ?? '').trim()
  // Same allowlist as the header/footer: never emit `javascript:`/`data:` or a
  // protocol-relative href even if a stale row bypassed server validation.
  const docsUrl = isSafePublicHref(rawDocsUrl) ? rawDocsUrl : ''
  const docsHref = docsUrl !== '' ? docsUrl : '/docs'
  const docsExternal = /^https?:\/\//i.test(docsHref)
  // A docs entry that requires sign-in stays hidden for guests, matching the
  // header. Everything else is shown even before `docs_url` exists, so the
  // entry point is already on the page when the documentation ships.
  const docsVisible = docsEntry.enabled && (!docsEntry.require_auth || isLoggedIn)

  return (
    <section className="home-hero">
      <div className="wrap home-hero-grid">
        <div className="home-hero-copy">
          {pending ? (
            <div aria-hidden="true" className="hero-skeleton">
              <span className="l1" />
              <span className="l2" />
              <span className="l3" />
              <span className="l4" />
              <span className="l5" />
            </div>
          ) : (
            <>
              <h1 tabIndex={-1} className="home-hero-title">
                <span>{t('public.home.hero.line1')}</span>
                <span>{t('public.home.hero.line2')}</span>
                <span>{t('public.home.hero.line3')}</span>
              </h1>
              <p className="home-hero-lead">{t('public.home.hero.subtitle')}</p>
              <div className="home-hero-actions">
                {isLoggedIn ? (
                  <a href="/dashboard" className="btn">
                    {t('public.home.hero.cta_console')}
                  </a>
                ) : (
                  <a href="/sign-up" className="btn">
                    {t('public.home.hero.cta_start')}
                  </a>
                )}
                {docsVisible ? (
                  <a
                    href={docsHref}
                    target={docsExternal ? '_blank' : undefined}
                    rel={docsExternal ? 'noopener noreferrer' : undefined}
                    className="btn btn-ghost"
                  >
                    {t('public.home.hero.cta_api_docs')}
                  </a>
                ) : null}
                {!isLoggedIn && pricingEnabled ? (
                  <a href="/pricing" className="btn btn-ghost">
                    {t('public.home.hero.cta_pricing')}
                  </a>
                ) : null}
              </div>
              <ul className="home-specs">
                <li>{t('public.home.hero.spec_protocol')}</li>
                <li>{t('public.home.hero.spec_stream')}</li>
                <li>{t('public.home.hero.spec_translate')}</li>
              </ul>
            </>
          )}
          {failed ? (
            <p className="home-hero-failed">
              <button type="button" onClick={() => onRetry?.()}>
                {t('public.home.hero.retry')}
              </button>
            </p>
          ) : null}
        </div>

        <div className="home-instrument">
          <figure className="trace" aria-label={t('public.home.trace.caption')}>
            <div className="trace-bar">
              <span className="trace-name">
                <span className="pulse-dot" aria-hidden="true" />
                {t('public.home.trace.label')}
              </span>
              <span className="trace-tag">{t('public.home.trace.example')}</span>
            </div>
            <div className="trace-req">
              <div className="trace-req-line">
                <span className="trace-verb">POST</span>
                <span>/v1/chat/completions</span>
                <span className="trace-chip">stream: true</span>
              </div>
              <p className="trace-meta">
                <span>
                  model <b>auto</b>
                </span>
                <span>
                  upstream <b>weighted</b>
                </span>
              </p>
            </div>
            <div className="trace-stages-wrap">
              <ol className="trace-stages">
                {TRACE_STAGES.map((stage) => (
                  <li key={stage}>
                    <span className="trace-stage-name">
                      {t(`public.home.trace.stage_${stage}`)}
                    </span>
                    <span className="trace-stage-body">
                      {t(`public.home.trace.body_${stage}`)}
                      {stage === 'route' ? (
                        <span className="trace-channels" aria-hidden="true">
                          <span>openai</span>
                          <span>anthropic</span>
                          <span>gemini</span>
                          <span>+57</span>
                        </span>
                      ) : null}
                    </span>
                  </li>
                ))}
              </ol>
              <span className="trace-pulse-dot" aria-hidden="true" />
            </div>
            <div className="trace-foot">
              <span>{t('public.home.trace.failover')}</span>
              <span>{t('public.home.trace.ledger')}</span>
            </div>
          </figure>
          <p className="trace-caption" aria-hidden="true">
            {t('public.home.trace.caption')}
          </p>
        </div>
      </div>
    </section>
  )
}
