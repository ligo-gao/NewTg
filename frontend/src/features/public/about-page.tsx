import { usePublicStatus } from '@/lib/public-status'
import { useI18n } from '@/shared/lib/i18n-context'

/** Capability cells reuse the homepage copy; this page only re-lays them out. */
const CAPABILITIES = [
  'multi_upstream',
  'streaming',
  'ratelimit',
  'stateless',
  'credentials',
  'billing',
] as const

/** Same six stages as the homepage instrument, in request order. */
const TRACE_STAGES = ['auth', 'quota', 'route', 'translate', 'stream', 'settle'] as const

const ENDPOINTS = ['v1', 'v1beta', 'pg'] as const

/**
 * F-09-07 about page.
 *
 * The page is intentionally static: hero, facts, capabilities, request trace
 * and endpoints. The only live value is the advertised interface-language
 * count from `GET /api/status`; when that probe fails the cell is omitted
 * rather than placeholdered, and the rest of the page still renders.
 */
export function AboutPage() {
  const { t } = useI18n()
  const status = usePublicStatus()
  const languageCount = status.data?.languages.length

  return (
    <article className="about-wrap">
      <section className="about-hero">
        <h1>
          <span>{t('public.home.hero.line1')}</span>
          <span>{t('public.home.hero.line2')}</span>
          <span>{t('public.home.hero.line3')}</span>
        </h1>
        <p className="lede">{t('public.about.hero.lede')}</p>
        <div className="about-specs">
          <span>{t('public.home.hero.spec_protocol')}</span>
          <span>{t('public.home.hero.spec_stream')}</span>
          <span>{t('public.home.hero.spec_translate')}</span>
        </div>
      </section>

      <div className="about-facts">
        {languageCount !== undefined ? (
          <span>
            <b>{languageCount}</b>
            {t('public.about.stats.languages')}
          </span>
        ) : null}
        <span>
          <b>{ENDPOINTS.length}</b>
          {t('public.about.fact.endpoints')}
        </span>
        <span>
          <b>{TRACE_STAGES.length}</b>
          {t('public.about.fact.stages')}
        </span>
      </div>

      <section className="about-sec" aria-labelledby="about-capabilities">
        <h2 id="about-capabilities">{t('public.about.section.capabilities')}</h2>
        <p className="hint">{t('public.about.section.capabilities_hint')}</p>
        <div className="about-caps">
          {CAPABILITIES.map((key) => (
            <div key={key}>
              <h3>{t(`public.home.features.${key}.title`)}</h3>
              <p>{t(`public.home.features.${key}.body`)}</p>
            </div>
          ))}
        </div>
      </section>

      <section className="about-sec" aria-labelledby="about-trace">
        <h2 id="about-trace">{t('public.about.section.trace')}</h2>
        <p className="hint">{t('public.about.section.trace_hint')}</p>
        <div className="about-trace">
          <div className="about-trace-rail">
            {TRACE_STAGES.map((stage) => (
              <div key={stage} className="about-trace-step">
                <h4>{t(`public.home.trace.stage_${stage}`)}</h4>
                <p>{t(`public.home.trace.body_${stage}`)}</p>
              </div>
            ))}
          </div>
          <div className="about-trace-notes">
            <span>{t('public.home.trace.failover')}</span>
            <span>{t('public.home.trace.ledger')}</span>
          </div>
        </div>
      </section>

      <section className="about-sec" aria-labelledby="about-endpoints">
        <h2 id="about-endpoints">{t('public.about.section.endpoints')}</h2>
        <p className="hint">{t('public.about.section.endpoints_hint')}</p>
        <div className="about-endpoints">
          {ENDPOINTS.map((endpoint) => (
            <div key={endpoint}>
              <code>{`/${endpoint}`}</code>
              <span>{t(`public.about.endpoint.${endpoint}`)}</span>
            </div>
          ))}
        </div>
      </section>
    </article>
  )
}
