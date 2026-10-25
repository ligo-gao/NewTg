/**
 * 排名梯子：每行一个模型，条形长度就是它在本期榜内的占比。
 *
 * 两条轴都来自响应本身——token 轴的分母是上榜模型 token 之和，请求轴
 * 的分母是上榜模型请求之和；切换只是换分母，行与顺序完全不变。占比由
 * `metricShare` 计算，零分母返回 0（不会是 NaN），条形宽度与右侧读数
 * 共用同一个值，读数与图形永不脱节。
 */

import { useState } from 'react'

import type { TranslateFn } from '@/shared/lib/i18n-context'

import type { RankingsModel } from './api'
import { formatExact, formatPercent } from './format'
import {
  metricShare,
  tokensPerRequest,
  type RankingsMetric,
} from './summary'

export interface ModelLadderProps {
  models: readonly RankingsModel[]
  /**
   * 初始口径。签名没有回调参数，因此切换状态由梯子自身持有；父级传入的
   * 值只在挂载时作为默认口径。
   */
  metric: RankingsMetric
  locale: string
  t: TranslateFn
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
    <section className="rank-ladder-block">
      <div className="rank-ladder-head">
        <h2>{t('public.rankings.ladder.title')}</h2>
        <div
          className="rank-seg"
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

      <ol className="rank-ladder" data-testid="rank-ladder">
        {models.map((model, index) => {
          const value =
            metric === 'tokens' ? model.total_tokens : model.requests
          const total = metric === 'tokens' ? totalTokens : totalRequests
          const share = metricShare(value, total)
          const average = tokensPerRequest(model.total_tokens, model.requests)
          const sub =
            metric === 'tokens'
              ? t('public.rankings.ladder.sub_tokens', {
                  tokens: formatExact(model.total_tokens, locale),
                  requests: formatExact(model.requests, locale),
                })
              : average === null
                ? `${formatExact(model.requests, locale)} ${t('public.rankings.ladder.requests')}`
                : t('public.rankings.ladder.sub_requests', {
                    requests: formatExact(model.requests, locale),
                    average: formatExact(average, locale),
                  })
          return (
            <li
              key={`${model.rank}-${model.model}`}
              className={`rank-lane${index === 0 ? ' rank-lane--lead' : ''}`}
              data-model={model.model}
            >
              <span className="rank">{model.rank}</span>
              <div className="rank-lane-main">
                <span className="rank-name" title={model.model}>
                  {model.display_name}
                </span>
                <div className="rank-bar" aria-hidden="true">
                  <i style={{ width: `${Math.min(Math.max(share, 0), 100)}%` }} />
                </div>
                <span className="rank-sub">{sub}</span>
              </div>
              <span className="rank-val">
                {formatPercent(share, locale)}
                <small>{t('public.rankings.ladder.share')}</small>
              </span>
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
