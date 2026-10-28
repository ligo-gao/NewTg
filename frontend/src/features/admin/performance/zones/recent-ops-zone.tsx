import { useId } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { StatusBadge } from '@/shared/ui/status-badge'

import type {
  CleanupMode,
  PerformanceRecentOperation,
  PerformanceRecentOperations,
} from '../api'
import { formatBytes, formatCount, formatDateTime, formatMilliseconds } from '../utils'
import {
  InlineError,
  SkeletonLines,
  describePerformanceError,
} from './primitives'

/**
 * 最近操作（只读）区（Task 18 / adjudications §5）。
 *
 * 从旧 `performance-page.tsx` 整段搬出，`performance-recent-row` 保留。
 * 本区不发起请求：数据、pending、错误与重试全部由页面注入，轮询行为也与
 * 旧页面一致（无 `refetchInterval`）。
 */

function actionKey(action: PerformanceRecentOperation['action']): string {
  return `admin.performance.recent.action_${action.replace(/\./g, '_')}`
}

function modeKey(mode: CleanupMode): string {
  return `admin.performance.logs.mode_${mode}`
}

export interface RecentOpsZoneProps {
  recent: PerformanceRecentOperations | undefined
  pending: boolean
  error: unknown
  retry: () => void
}

export function RecentOpsZone({
  recent,
  pending,
  error,
  retry,
}: RecentOpsZoneProps) {
  const { t, locale } = useI18n()
  const titleId = useId()

  return (
    <section
      data-testid="performance-recent-card"
      aria-labelledby={titleId}
      className="rounded-xl border border-border-default bg-surface-card p-5"
    >
      <h2
        id={titleId}
        className="mb-3 text-card-title font-semibold leading-[22px] text-ink-1"
      >
        {t('admin.performance.recent.title')}
      </h2>
      {pending ? (
        <SkeletonLines rows={3} testId="performance-recent-skeleton" />
      ) : recent === undefined ? (
        <InlineError
          testId="performance-recent-error"
          message={describePerformanceError(error, t)}
          retryLabel={t('admin.performance.common.retry')}
          onRetry={retry}
        />
      ) : recent.items.length === 0 ? (
        <p className="text-body text-ink-3">
          {t('admin.performance.recent.empty')}
        </p>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[760px] border-collapse text-left text-body">
            <thead>
              <tr className="border-b border-border-hairline text-meta text-ink-3">
                <th scope="col" className="px-2 py-2 font-medium">
                  {t('admin.performance.recent.time')}
                </th>
                <th scope="col" className="px-2 py-2 font-medium">
                  {t('admin.performance.recent.actor')}
                </th>
                <th scope="col" className="px-2 py-2 font-medium">
                  {t('admin.performance.recent.action')}
                </th>
                <th scope="col" className="px-2 py-2 font-medium">
                  {t('admin.performance.recent.params')}
                </th>
                <th scope="col" className="px-2 py-2 font-medium">
                  {t('admin.performance.recent.result')}
                </th>
                <th scope="col" className="px-2 py-2 font-medium">
                  {t('admin.performance.recent.amount')}
                </th>
              </tr>
            </thead>
            <tbody>
              {recent.items.map((item, index) => (
                <tr
                  key={`${item.time}-${index}`}
                  data-testid="performance-recent-row"
                  className="border-b border-border-hairline last:border-0"
                >
                  <td className="whitespace-nowrap px-2 py-2 font-mono text-meta text-ink-2">
                    {formatDateTime(item.time, locale)}
                  </td>
                  <td className="px-2 py-2 text-ink-2">{item.actor}</td>
                  <td className="px-2 py-2 text-ink-2">
                    {t(actionKey(item.action))}
                  </td>
                  <td className="px-2 py-2 text-ink-2">
                    {item.mode === undefined
                      ? '—'
                      : t('admin.performance.recent.params_summary', {
                          mode: t(modeKey(item.mode)),
                          value: item.value ?? '—',
                        })}
                  </td>
                  <td className="px-2 py-2">
                    <StatusBadge
                      variant={item.result === 'success' ? 'success' : 'danger'}
                    >
                      {t(
                        item.result === 'success'
                          ? 'admin.performance.recent.success'
                          : 'admin.performance.recent.failed',
                      )}
                    </StatusBadge>
                  </td>
                  <td className="px-2 py-2 font-mono text-meta text-ink-2">
                    <span>
                      {item.action === 'system.gc.run'
                        ? formatMilliseconds(item.duration_ms, locale)
                        : t('admin.performance.recent.deleted', {
                            count: formatCount(item.deleted_count, locale),
                            size: formatBytes(item.freed_bytes, locale),
                          })}
                    </span>
                    {item.failed_count > 0 ? (
                      <span className="ml-2 text-warning">
                        {t('admin.performance.recent.failed_count', {
                          count: formatCount(item.failed_count, locale),
                        })}
                      </span>
                    ) : null}
                    {item.skipped_active ? (
                      <span className="ml-2 text-ink-3">
                        {t('admin.performance.recent.skipped_active')}
                      </span>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
