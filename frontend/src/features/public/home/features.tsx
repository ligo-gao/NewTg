import type { ComponentType } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

import {
  Coins01Icon,
  CubeIcon,
  DashboardSpeed01Icon,
  NeuralNetworkIcon,
  RadioIcon,
  ShieldKeyIcon,
} from '../public-icons'

interface FeatureCard {
  id: string
  Icon: ComponentType<{ size?: number; strokeWidth?: number }>
}

/**
 * F-09-03 six capability cards. Every claim maps to an implemented architecture
 * capability (routes / integer billing / protocol translation / quotas /
 * stateless instances / credential hashing); no marketing-only claims.
 *
 * Layout: a two-column register separated by hairlines, not six floating
 * cards. The section note says explicitly that these are shipped features.
 */
const FEATURES: readonly FeatureCard[] = [
  { id: 'multi_upstream', Icon: NeuralNetworkIcon },
  { id: 'billing', Icon: Coins01Icon },
  { id: 'streaming', Icon: RadioIcon },
  { id: 'ratelimit', Icon: DashboardSpeed01Icon },
  { id: 'stateless', Icon: CubeIcon },
  { id: 'credentials', Icon: ShieldKeyIcon },
]

export function FeaturesSection() {
  const { t } = useI18n()

  return (
    <section className="caps">
      <div className="wrap">
        <div className="section-head">
          <h2 className="section-title">{t('public.home.features.title')}</h2>
          <p className="note">{t('public.home.features.lede')}</p>
        </div>
        <ul className="caps-grid">
          {FEATURES.map(({ id, Icon }) => (
            <li key={id} className="cap">
              <Icon size={20} strokeWidth={1.5} />
              <div>
                <h3>{t(`public.home.features.${id}.title`)}</h3>
                <p>{t(`public.home.features.${id}.body`)}</p>
              </div>
            </li>
          ))}
        </ul>
      </div>
    </section>
  )
}
