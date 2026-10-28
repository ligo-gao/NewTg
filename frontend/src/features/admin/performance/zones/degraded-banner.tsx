import { Fragment, type ReactNode } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

/**
 * 监控断线时的顶部提示条。它是持久提示（`role="status"`），不自动消失：
 * 只要数据源仍不可用，用户就需要一直看到「模型数据不受影响」的边界说明。
 */

/**
 * 把译文里的 `{code}` 占位符渲染成真正的 `<code>` 元素。`t()` 只接受
 * 字符串，插值后无法再包标签，所以这里在未插值的模板上切分——占位符两侧
 * 的文案仍然由词典提供，代码片段的样式则与 demo 一致。
 *
 * 模板里的占位符可以出现 0 次、1 次或多次：0 次按纯文本返回，每一处
 * `{code}` 都渲染成一个 `<code>`。旧实现只接受恰好一次，遇到多占位符的
 * 语言文件会把 `{code}` 字面量原样漏到界面上。
 */
export function codePhrase(template: string, code: string): ReactNode {
  const parts = template.split('{code}')
  if (parts.length === 1) return template

  return (
    <>
      {parts.map((part, index) => (
        <Fragment key={index}>
          {index > 0 ? (
            <code className="rounded bg-surface-card px-1 py-0.5 font-mono text-badge text-ink-2">
              {code}
            </code>
          ) : null}
          {part}
        </Fragment>
      ))}
    </>
  )
}

/** 后端数据源三态；`unknown` 表示 overview/manifest 里缺该数据源条目。 */
export type DegradedStatus = 'ok' | 'unavailable' | 'error' | 'unknown'

export interface DegradedBannerProps {
  /**
   * Prometheus 数据源状态。`undefined`（overview 还没到）与 `ok` 都不渲染；
   * `error`（后端连上了、Prometheus 自己报错/超时）与 `unavailable`
   * （后端根本没连上）是两种不同的故障，文案不能混用。
   */
  status: DegradedStatus | undefined
}

export function DegradedBanner({ status }: DegradedBannerProps) {
  const { t } = useI18n()

  if (status === undefined || status === 'ok') return null

  const failed = status === 'error'
  const title = failed
    ? t('admin.performance.banner.title_error')
    : t('admin.performance.banner.title')
  const note = failed
    ? t('admin.performance.banner.note_error')
    : t('admin.performance.banner.note')

  return (
    <div
      role="status"
      data-testid="performance-degraded-banner"
      className="grid gap-1 rounded-lg border border-warning-line bg-warning-bg px-3.5 py-3"
    >
      <p className="text-meta font-semibold text-ink-1">{title}</p>
      <p className="text-badge text-ink-2">
        {codePhrase(note, t('admin.performance.banner.code'))}
      </p>
    </div>
  )
}
