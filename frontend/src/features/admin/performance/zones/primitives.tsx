import { ApiError } from '@/api/errors'
import type { TranslateFn } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'

import { KNOWN_PROBLEM_CODES, problemCode } from '../utils'

/**
 * 各区共用的展示与错误文案原语。
 *
 * 维护动作区与最近操作区都需要同一套骨架屏、内联错误条与错误文案口径；
 * 放在独立模块里，避免只读区反向依赖维护动作区。
 */

export interface ZoneRailProps {
  titleId: string
  title: string
  sub?: string
  note?: string
}

/**
 * 每个观测区的可见标题栏（demo 的 `pf-rail`）：`h2` + 可选两行小字。
 *
 * `titleId` 由调用方的 `useId()` 提供，`<section aria-labelledby>` 指向它，
 * 这样区块的无障碍名来自可见标题而不是另一份重复文案。`sub` 是规模副标题
 * （例如模型数），`note` 是更次要的节奏说明；两者缺席时不渲染空节点。
 */
export function ZoneRail({ titleId, title, sub, note }: ZoneRailProps) {
  return (
    <div className="mb-3 flex flex-wrap items-baseline gap-x-3 gap-y-1">
      <h2
        id={titleId}
        className="text-card-title font-semibold leading-[22px] text-ink-1"
      >
        {title}
      </h2>
      {sub === undefined ? null : (
        <p className="text-meta text-ink-3">{sub}</p>
      )}
      {note === undefined ? null : (
        <p className="text-badge text-ink-3">{note}</p>
      )}
    </div>
  )
}

/** 加载骨架：固定行数，`aria-hidden` 让读屏跳过占位。 */
export function SkeletonLines({
  rows,
  testId,
}: {
  rows: number
  testId: string
}) {
  return (
    <div data-testid={testId} aria-hidden="true" className="space-y-2">
      {Array.from({ length: rows }, (_, index) => (
        <div key={index} className="h-4 w-full rounded bg-surface-subtle" />
      ))}
    </div>
  )
}

/** 内联错误条：带原地重试按钮，不整块替换所在卡片。 */
export function InlineError({
  message,
  retryLabel,
  onRetry,
  testId,
}: {
  message: string
  retryLabel: string
  onRetry: () => void
  testId: string
}) {
  return (
    <div
      data-testid={testId}
      role="alert"
      className="flex flex-wrap items-center gap-3 rounded-lg border border-danger-line bg-danger-bg px-3 py-2 text-body text-danger"
    >
      <span>{message}</span>
      <Button variant="ghost" className="h-7 px-2 text-body" onClick={onRetry}>
        {retryLabel}
      </Button>
    </div>
  )
}

/**
 * 统一错误文案：已知 Problem 码本地化，429 原地提示，传输失败单独说明。
 * 维护动作与最近操作（只读表格的重试）共用同一口径。
 */
export function describePerformanceError(
  error: unknown,
  t: TranslateFn,
): string {
  if (error instanceof ApiError) {
    // 传输失败没有 Problem 码可映射，必须使用本地化的网络文案，而不是
    // 让它落到「操作失败」——后者会误导用户去重复提交。
    if (error.kind === 'offline') {
      return t('admin.performance.errors.offline')
    }
    if (error.status === 429) {
      return error.retry_after !== undefined
        ? t('admin.performance.errors.rate_limited_retry', {
            seconds: error.retry_after,
          })
        : t('admin.performance.errors.rate_limited')
    }
    const code = problemCode(error)
    if (KNOWN_PROBLEM_CODES.has(code)) {
      return t(`admin.performance.errors.${code}`)
    }
  }
  return t('admin.performance.errors.generic')
}
