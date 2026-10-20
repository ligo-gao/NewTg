import { useQuery } from '@tanstack/react-query'

import { usePublicStatus } from '@/lib/public-status'
import { ME_QUERY_KEY, meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'

import { SiteBrand } from '../public-header'
import { CustomHomeHtml, sanitizeHtmlFragment } from './custom-html'
import { FeaturesSection } from './features'
import { HeroSection } from './hero'
import { LiveFeedSection } from './live-feed'
import { OnboardingSection } from './onboarding'
import { PublicSeo } from './seo'
import { StatsSection } from './stats'

/**
 * F-09-01/03/04/05/06 homepage composition.
 *
 * Section order is frozen (`hero → live_feed → features → stats → onboarding`);
 * the front end never reorders. `live_feed` renders when
 * `site.home_blocks.live_feed !== false` and owns its own SSE/polling channel
 * (F-09-02); it is skipped entirely under a custom-HTML takeover.
 *
 * The custom-HTML takeover replaces all five built-in sections. Its sanitized
 * result being empty is the documented degradation branch: fall back to the
 * normal sections rather than showing a blank page.
 */
export function HomePage() {
  const { t } = useI18n()
  const status = usePublicStatus()
  const me = useQuery({ ...meQueryOptions(), queryKey: ME_QUERY_KEY })
  const isLoggedIn = me.isSuccess
  // Snapshot before the pending branch: narrowing the query result there
  // would otherwise collapse `data` to the pending union member.
  const statusData = status.data
  const site = statusData?.site
  const blocks = site?.home_blocks

  const customHtml =
    site?.home_custom_enabled === true ? sanitizeHtmlFragment(site.home_custom_html) : ''

  if (status.isPending) {
    return (
      <>
        <PublicSeo site={site} />
        <HeroSection site={site} nav={statusData?.nav} isLoggedIn={isLoggedIn} pending />
      </>
    )
  }

  const sections = {
    hero: blocks?.hero !== false,
    live_feed: blocks?.live_feed !== false,
    features: blocks?.features !== false,
    stats: blocks?.stats !== false,
    onboarding: blocks?.onboarding !== false,
  }
  const hasBuiltInSection = Object.values(sections).some(Boolean)

  return (
    <>
      <PublicSeo site={site} />
      {customHtml !== '' ? (
        <CustomHomeHtml html={site?.home_custom_html ?? ''} />
      ) : !hasBuiltInSection ? (
        <section className="mx-auto flex w-full max-w-[1200px] flex-col items-start gap-4 px-4 py-16 md:px-6 md:py-24">
          <SiteBrand site={site} />
          <p className="text-sm text-ink-2">{t('public.home.empty_config')}</p>
          <a
            href="/dashboard"
            className="inline-flex h-10 items-center rounded-lg bg-brand px-4 text-sm font-medium text-on-brand hover:bg-brand-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {t('public.home.hero.cta_console')}
          </a>
        </section>
      ) : (
        <>
          {sections.hero ? (
            <HeroSection
              site={site}
              nav={statusData?.nav}
              isLoggedIn={isLoggedIn}
              failed={status.isError}
              onRetry={() => void status.refetch()}
            />
          ) : null}
          {sections.live_feed ? <LiveFeedSection /> : null}
          {sections.features ? <FeaturesSection /> : null}
          {sections.stats ? <StatsSection /> : null}
          {sections.onboarding ? (
            <OnboardingSection
              serverAddress={site?.server_address}
              isLoggedIn={isLoggedIn}
            />
          ) : null}
        </>
      )}
    </>
  )
}
