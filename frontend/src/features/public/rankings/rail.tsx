/**
 * 排名页右栏：上游分布、每次请求平均 token、环比。
 *
 * 三段都只呈现响应携带的值——份额直接读 `share_pct`，平均值由
 * `tokensPerRequest` 计算（零请求返回 null，渲染成破折号而不是 0），
 * 环比只在 `movers` 非空时出现。厂商名按 API 给的标签原样显示，空档案
 * 已在上游归一化为 Unknown，这里不做第二次猜测。
 */

import type { TranslateFn } from '@/shared/lib/i18n-context'

import type {
  RankingsModel,
  RankingsMover,
  RankingsMovers,
  RankingsProvider,
} from './api'
import { ShareBar } from './charts'
import { formatExact, formatPercent } from './format'
import { foldTopProviders } from './share'
import { tokensPerRequest } from './summary'

export interface RankingsRailProps {
  providers: readonly RankingsProvider[]
  models: readonly RankingsModel[]
  movers: RankingsMovers
  locale: string
  t: TranslateFn
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
    <div className="rank-mover-group" data-direction={direction}>
      <h3>
        {t(
          direction === 'up'
            ? 'public.rankings.rail.rising'
            : 'public.rankings.rail.falling',
        )}
      </h3>
      <ul>
        {items.map((item) => (
          <li key={item.model}>
            <span className="n">{item.model}</span>
            <span className="p">{formatPercent(item.delta_pct, locale)}</span>
          </li>
        ))}
      </ul>
    </div>
  )
}

export function RankingsRail({
  providers,
  models,
  movers,
  locale,
  t,
}: RankingsRailProps) {
  const rows = foldTopProviders(providers, t('public.rankings.trend.other'))
  const averages = models.map((model) =>
    tokensPerRequest(model.total_tokens, model.requests),
  )
  const maxAverage = Math.max(0, ...averages.map((value) => value ?? 0))
  const hasMovers = movers.up.length > 0 || movers.down.length > 0

  return (
    <>
      <div className="rank-panel">
        <div className="rank-dist">
          <h2>{t('public.rankings.rail.providers')}</h2>
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
              <ul className="rank-dist-rows" data-testid="rank-providers">
                {rows.map((row) => (
                  <li className="rank-prov" key={row.key}>
                    <span className="n">{row.label}</span>
                    <span className="p">
                      {formatPercent(row.share, locale)}
                    </span>
                    <span className="t">
                      {formatExact(row.tokens, locale)}{' '}
                      {t('public.rankings.ladder.tokens')}
                    </span>
                  </li>
                ))}
              </ul>
            </>
          )}
          <p className="rank-note">
            {t('public.rankings.rail.providers_note')}
          </p>
        </div>
      </div>

      <div className="rank-panel">
        <div className="rank-eff">
          <h2>{t('public.rankings.rail.efficiency')}</h2>
          {models.length === 0 ? (
            <p className="rank-empty">
              {t('public.rankings.rail.efficiency_empty')}
            </p>
          ) : (
            <ul className="rank-eff-rows" data-testid="rank-efficiency">
              {models.map((model, index) => {
                const average = averages[index] ?? null
                const width =
                  average === null || maxAverage === 0
                    ? 0
                    : (average / maxAverage) * 100
                return (
                  <li className="rank-eff-row" key={`${model.rank}-${model.model}`}>
                    <span className="n">{model.display_name}</span>
                    <span className="p">
                      {average === null ? '—' : formatExact(average, locale)}
                    </span>
                    <span className="rank-eff-track" aria-hidden="true">
                      <i style={{ width: `${Math.min(Math.max(width, 0), 100)}%` }} />
                    </span>
                  </li>
                )
              })}
            </ul>
          )}
          <p className="rank-note">
            {t('public.rankings.rail.efficiency_note')}
          </p>
        </div>
      </div>

      <div className="rank-panel">
        <div className="rank-moments" data-testid="rank-movers">
          <h2>{t('public.rankings.rail.movers')}</h2>
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
            <div className="rank-moments-empty">
              {t('public.rankings.rail.movers_empty')}
            </div>
          )}
        </div>
      </div>
    </>
  )
}
