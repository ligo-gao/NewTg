import type { ReactNode } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

/**
 * 监控断线时的顶部提示条。它是持久提示（`role="status"`），不自动消失：
 * 只要数据源仍不可用，用户就需要一直看到「模型数据不受影响」的边界说明。
 */

/**
 * 把译文里的 `{code}` 占位符渲染成真正的 `<code>` 元素。`t()` 只接受
 * 字符串，插值后无法再包标签，所以这里在未插值的模板上切分——占位符两侧
 * 的文案仍然由词典提供，代码片段的样式则与 demo 一致。
 */
export function codePhrase(template: string, code: string): ReactNode {
  const parts = template.split('{code}')
  if (parts.length !== 2) return template

  return (
    <>
      {parts[0]}
      <code className="rounded bg-surface-card px-1 py-0.5 font-mono text-badge text-ink-2">
        {code}
      </code>
      {parts[1]}
    </>
  )
}

export interface DegradedBannerProps {
  visible: boolean
}

export function DegradedBanner({ visible }: DegradedBannerProps) {
  const { t } = useI18n()

  if (!visible) return null

  return (
    <div
      role="status"
      data-testid="performance-degraded-banner"
      className="grid gap-1 rounded-lg border border-warning-line bg-warning-bg px-3.5 py-3"
    >
      <p className="text-meta font-semibold text-ink-1">
        {t('admin.performance.banner.title')}
      </p>
      <p className="text-badge text-ink-2">
        {codePhrase(
          t('admin.performance.banner.note'),
          t('admin.performance.banner.code'),
        )}
      </p>
    </div>
  )
}
