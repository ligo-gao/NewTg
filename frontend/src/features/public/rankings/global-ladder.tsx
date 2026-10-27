/**
 * 全球性能测评榜：与「实际调用排行」并列的第二张榜。
 *
 * 数据来自本地静态样例（`benchmarks.ts`），面板 meta 必须带示例数据标记。
 * 每一行都可点击打开详情弹窗：`li` 承担按钮语义（`role="button"`、
 * 键盘 Enter/Space、`aria-haspopup="dialog"`），行内不嵌套第二个交互元素。
 */

import type { KeyboardEvent, MouseEvent } from 'react'

import type { TranslateFn } from '@/shared/lib/i18n-context'

import {
  GLOBAL_BENCHMARK_LEADERBOARD,
  GLOBAL_BENCHMARK_UPDATED_AT,
  thirdBenchmark,
  type GlobalBenchmarkEntry,
} from './benchmarks'
import { formatExact, formatScore } from './format'

export interface GlobalBenchmarkLadderProps {
  locale: string
  t: TranslateFn
  onSelect: (modelId: string) => void
}

function entryLabel(entry: GlobalBenchmarkEntry, t: TranslateFn): string {
  return t('public.rankings.benchmark.view_detail', { model: entry.displayName })
}

export function GlobalBenchmarkLadder({
  locale,
  t,
  onSelect,
}: GlobalBenchmarkLadderProps) {
  const onKeyDown = (
    event: KeyboardEvent<HTMLLIElement>,
    entry: GlobalBenchmarkEntry,
  ) => {
    if (event.key !== 'Enter' && event.key !== ' ') return
    event.preventDefault()
    onSelect(entry.modelId)
  }

  // 行是 `li[role=button]`：先把它变成焦点，弹窗关闭后焦点才回得到这一行。
  const onClick = (
    event: MouseEvent<HTMLLIElement>,
    entry: GlobalBenchmarkEntry,
  ) => {
    event.currentTarget.focus()
    onSelect(entry.modelId)
  }

  return (
    <section
      className="ranking-panel benchmark"
      aria-labelledby="global-benchmark-title"
    >
      <div className="ranking-header">
        <h2 id="global-benchmark-title" className="ranking-title">
          {t('public.rankings.benchmark.title')}
        </h2>
        <span className="ranking-meta">
          {t('public.rankings.benchmark.meta', {
            source: t('public.rankings.benchmark.sample'),
            date: GLOBAL_BENCHMARK_UPDATED_AT,
          })}
        </span>
      </div>

      <ol className="ranking-list" data-testid="benchmark-ladder">
        {GLOBAL_BENCHMARK_LEADERBOARD.map((entry) => {
          const third = thirdBenchmark(entry)
          return (
            <li
              key={entry.modelId}
              className={`ranking-row ${
                entry.rank <= 3 ? `top-${entry.rank}` : ''
              }`.trim()}
              role="button"
              tabIndex={0}
              aria-haspopup="dialog"
              aria-label={entryLabel(entry, t)}
              data-model={entry.modelId}
              onClick={(event) => onClick(event, entry)}
              onKeyDown={(event) => onKeyDown(event, entry)}
            >
              <div className="rank-num">{entry.rank}</div>
              <div className="rank-main">
                <span className="rank-model">{entry.displayName}</span>
                <div className="benchmark-scores">
                  <span className="benchmark-badge">
                    MMLU <strong>{formatScore(entry.mmlu, locale)}</strong>
                  </span>
                  <span className="benchmark-badge">
                    HumanEval <strong>{formatScore(entry.humaneval, locale)}</strong>
                  </span>
                  <span className="benchmark-badge">
                    {third.name}{' '}
                    <strong>
                      {third.name === 'Arena'
                        ? formatExact(third.value, locale)
                        : formatScore(third.value, locale)}
                    </strong>
                  </span>
                </div>
              </div>
              <div className="rank-value">
                {formatExact(entry.eloScore, locale)}
                <small>{t('public.rankings.benchmark.elo')}</small>
              </div>
            </li>
          )
        })}
      </ol>
    </section>
  )
}
