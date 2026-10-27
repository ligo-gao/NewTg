/**
 * 实际调用排行：每行一个模型，右侧读数是本期榜内的占比。
 *
 * 两条轴都来自响应本身——token 轴的分母是上榜模型 token 之和，请求轴
 * 的分母是上榜模型请求之和；切换只是换分母，行与顺序完全不变。占比由
 * `metricShare` 计算，零分母返回 0（不会是 NaN），读数与图形共用同一个值。
 * 厂商名取 `model.author`（渠道档案为空时上游已折成 Unknown）。
 */

import { useState } from 'react'

import type { TranslateFn } from '@/shared/lib/i18n-context'

import type { RankingsModel } from './api'
import {
  formatCompact,
  formatExact,
  formatPercent,
} from './format'
import {
  metricShare,
  tokensPerRequest,
  type RankingsMetric,
} from './summary'

export interface ModelLadderProps {
  models: readonly RankingsModel[]
  /** 初始口径；之后的切换状态由组件自身持有。 */
  metric: RankingsMetric
  locale: string
  t: TranslateFn
}

/** 前三名的名次色与字号由这个类名驱动（见 home.css）。 */
function rankClass(rank: number): string {
  return rank >= 1 && rank <= 3 ? `top-${rank}` : ''
}

export function ModelLadder({
  models,
  metric: initialMetric,
  locale,
  t,
}: ModelLadderProps) {
  const [metric, setMetric] = useState<RankingsMetric>(initialMetric)

  const totalTokens = models.reduce((sum, model) => sum + model.total_tokens, 0)
  const totalRequests = models.reduce((sum, model) => sum + model.requests, 0)

  return (
    <section className="ranking-panel" aria-labelledby="actual-ranking-title">
      <div className="ranking-header">
        <h2 id="actual-ranking-title" className="ranking-title">
          {t('public.rankings.ladder.title')}
        </h2>
        <div
          className="seg"
          role="group"
          aria-label={t('public.rankings.ladder.title')}
        >
          <button
            type="button"
            aria-pressed={metric === 'tokens'}
            onClick={() => setMetric('tokens')}
          >
            {t('public.rankings.ladder.by_tokens')}
          </button>
          <button
            type="button"
            aria-pressed={metric === 'requests'}
            onClick={() => setMetric('requests')}
          >
            {t('public.rankings.ladder.by_requests')}
          </button>
        </div>
      </div>

      <ol className="ranking-list" data-testid="rank-ladder">
        {models.map((model) => {
          const value =
            metric === 'tokens' ? model.total_tokens : model.requests
          const total = metric === 'tokens' ? totalTokens : totalRequests
          const share = metricShare(value, total)
          const average = tokensPerRequest(model.total_tokens, model.requests)
          const meta =
            average === null
              ? `${model.author} · ${t('public.rankings.ladder.meta_no_average', {
                  requests: formatExact(model.requests, locale),
                })}`
              : `${model.author} · ${t('public.rankings.ladder.meta', {
                  requests: formatExact(model.requests, locale),
                  average: formatExact(average, locale),
                })}`
          return (
            <li
              key={`${model.rank}-${model.model}`}
              className={`ranking-row ${rankClass(model.rank)}`.trim()}
              data-model={model.model}
            >
              <div className="rank-num">{model.rank}</div>
              <div className="rank-main">
                <span className="rank-model" title={model.model}>
                  {model.display_name}
                </span>
                <span className="rank-meta">{meta}</span>
              </div>
              <div className="rank-value">
                {formatCompact(value, locale)}
                <small>{formatPercent(share, locale)}</small>
              </div>
            </li>
          )
        })}
      </ol>

      {models.length === 0 ? (
        <p className="rank-empty">{t('public.rankings.ladder.empty')}</p>
      ) : null}
    </section>
  )
}
