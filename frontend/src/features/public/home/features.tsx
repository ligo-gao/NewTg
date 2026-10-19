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
 * Copy is front-end static content: no network dependency, so a status outage
 * cannot blank this section. The layout is a single outlined grid with hairline
 * separators rather than six floating cards (UI design §0.1).
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
    <section className="bg-surface-card">
      <div className="mx-auto w-full max-w-[1200px] px-4 py-16 md:px-6 md:py-20">
        <h2 className="text-base font-semibold leading-6 text-ink-1">
          {t('public.home.features.title')}
        </h2>
        <ul className="mt-8 grid grid-cols-1 overflow-hidden rounded-xl border border-border-default sm:grid-cols-2 lg:grid-cols-3">
          {FEATURES.map(({ id, Icon }) => (
            <li
              key={id}
              className="border-b border-border-hairline p-5 last:border-b-0 sm:border-r sm:[&:nth-child(2n)]:border-r-0 sm:[&:nth-child(n+5)]:border-b-0 lg:[&:nth-child(2n)]:border-r lg:[&:nth-child(3n)]:border-r-0 lg:[&:nth-child(n+4)]:border-b-0"
            >
              <Icon size={24} strokeWidth={1.5} />
              <h3 className="mt-3 text-[15px] font-semibold leading-[22px] text-ink-1">
                {t(`public.home.features.${id}.title`)}
              </h3>
              <p className="mt-1 text-sm leading-[22px] text-ink-2">
                {t(`public.home.features.${id}.body`)}
              </p>
            </li>
          ))}
        </ul>
      </div>
    </section>
  )
}
