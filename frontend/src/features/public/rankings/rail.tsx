/**
 * 排行榜底部信息栏的前两格：供应商分布、本周动态。
 *
 * 两格都只呈现响应携带的值——份额直接读 `share_pct`，环比只在 `movers`
 * 非空时出现。厂商名按 API 给的标签原样显示，空档案已在上游归一化为
 * Unknown，这里不做第二次猜测。第三格「统计详情」需要周期（刷新节奏），
 * 由页面渲染。
 */

import type { TranslateFn } from '@/shared/lib/i18n-context'

import type {
  RankingsMover,
  RankingsMovers,
  RankingsProvider,
} from './api'
import { ShareBar } from './charts'
import { formatPercent } from './format'
import { foldTopProviders } from './share'

export interface RankingsRailProps {
  providers: readonly RankingsProvider[]
  movers: RankingsMovers
  locale: string
  t: TranslateFn
}

function ArrowIcon({ direction }: { direction: 'up' | 'down' }) {
  return (
    <svg
      width="14"
      height="14"
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      aria-hidden="true"
    >
      {direction === 'up' ? (
        <path
          d="M8 12V4M4 8l4-4 4 4"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      ) : (
        <path
          d="M8 4v8M12 8l-4 4-4-4"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      )}
    </svg>
  )
}

/** 环比读数：正值带 `+`，负值由 Intl 自带 `-`，零显示 `0.0%`。 */
function formatDelta(value: number, locale: string): string {
  const text = formatPercent(value, locale)
  return value > 0 ? `+${text}` : text
}

function MoverGroup({
  direction,
  items,
  locale,
  t,
}: {
  direction: 'up' | 'down'
  items: readonly RankingsMover[]
  locale: string
  t: TranslateFn
}) {
  return (
    <div className="mover-group" data-direction={direction}>
      <h4>
        <ArrowIcon direction={direction} />
        {t(
          direction === 'up'
            ? 'public.rankings.rail.rising'
            : 'public.rankings.rail.falling',
        )}
      </h4>
      <ul className="mover-list">
        {items.map((item) => (
          <li className="mover-item" key={item.model}>
            <span className="mover-name">{item.model}</span>
            <span className={`mover-change ${direction}`}>
              {formatDelta(item.delta_pct, locale)}
            </span>
          </li>
        ))}
      </ul>
    </div>
  )
}

export function RankingsRail({
  providers,
  movers,
  locale,
  t,
}: RankingsRailProps) {
  const rows = foldTopProviders(providers, t('public.rankings.trend.other'))
  const hasMovers = movers.up.length > 0 || movers.down.length > 0

  return (
    <>
      <div className="info-panel" data-testid="rank-providers">
        <h3>{t('public.rankings.rail.providers')}</h3>
        {providers.length === 0 ? (
          <p className="rank-empty">
            {t('public.rankings.rail.providers_empty')}
          </p>
        ) : (
          <>
            <ShareBar
              providers={providers}
              locale={locale}
              ariaLabel={t('public.rankings.rail.providers')}
              otherLabel={t('public.rankings.trend.other')}
            />
            <ul className="provider-list">
              {rows.map((row) => (
                <li className="provider-row" key={row.key}>
                  <span className="provider-name">{row.label}</span>
                  <span className="provider-share">
                    {formatPercent(row.share, locale)}
                  </span>
                </li>
              ))}
            </ul>
          </>
        )}
      </div>

      <div className="info-panel" data-testid="rank-movers">
        <h3>{t('public.rankings.rail.movers')}</h3>
        {hasMovers ? (
          <>
            {movers.up.length > 0 ? (
              <MoverGroup
                direction="up"
                items={movers.up}
                locale={locale}
                t={t}
              />
            ) : null}
            {movers.down.length > 0 ? (
              <MoverGroup
                direction="down"
                items={movers.down}
                locale={locale}
                t={t}
              />
            ) : null}
          </>
        ) : (
          <p className="rank-empty">{t('public.rankings.rail.movers_empty')}</p>
        )}
      </div>
    </>
  )
}
