/**
 * 向导第 4 步：结果汇总。
 *
 * 只做展示：四个计数（新建 / 更新 / 跳过 / 失败）与逐行结果，失败行带上
 * 原因。写库由容器负责，本组件只通过回调请求「重试失败行」或「返回渠道
 * 表单」。
 *
 * 校验失败的行不可重试——输入没改，重试只会再失败一次。
 */
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'

import type { RowResult, RowResultAction } from './model-profile-wizard-logic'

const ACTIONS: readonly RowResultAction[] = [
  'created',
  'updated',
  'skipped',
  'failed',
]

const ACTION_CLASSES: Record<RowResultAction, string> = {
  created: 'border-success-line bg-success-bg text-success',
  updated: 'border-warning-line bg-warning-bg text-warning',
  skipped: 'border-border-default bg-surface-subtle text-ink-3',
  failed: 'border-danger-line bg-danger-bg text-danger',
}

export interface ModelProfileWizardResultProps {
  results: RowResult[]
  busy: boolean
  onRetryFailed: () => void
  onBackToForm: () => void
}

export function ModelProfileWizardResult({
  results,
  busy,
  onRetryFailed,
  onBackToForm,
}: ModelProfileWizardResultProps) {
  const { t } = useI18n()
  const counts: Record<RowResultAction, number> = {
    created: 0,
    updated: 0,
    skipped: 0,
    failed: 0,
  }
  for (const result of results) counts[result.action] += 1
  const retryable = results.some(
    (result) => result.action === 'failed' && result.retryable,
  )

  return (
    <section
      aria-label={t('common.channels.profileWizard.stepResult')}
      className="flex flex-col gap-3"
    >
      <h3 className="text-body font-semibold text-ink-1">
        {t('common.channels.profileWizard.result.done')}
      </h3>

      <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
        {ACTIONS.map((action) => (
          <div
            key={action}
            className={`rounded-lg border px-3 py-2 ${ACTION_CLASSES[action]}`}
          >
            <span className="text-meta">{t(`common.channels.profileWizard.result.${action}`)} {counts[action]}</span>
          </div>
        ))}
      </div>

      <ul
        aria-label={t('common.channels.profileWizard.stepResult')}
        className="divide-y divide-border-hairline rounded-lg border border-border-hairline"
      >
        {results.map((result) => (
          <li
            key={result.id}
            className="flex flex-wrap items-baseline gap-2 px-3 py-2 text-meta"
          >
            <span className="font-mono text-ink-1">{result.id}</span>
            <span
              className={`inline-flex h-[21px] items-center rounded border px-1.5 text-badge whitespace-nowrap ${ACTION_CLASSES[result.action]}`}
            >
              {t(`common.channels.profileWizard.result.${result.action}`)}
            </span>
            {result.message === undefined ? null : (
              <span className="basis-full text-meta text-danger sm:basis-auto">
                {result.message}
              </span>
            )}
          </li>
        ))}
      </ul>

      <div className="flex flex-wrap items-center gap-2 border-t border-border-hairline pt-3">
        <Button
          variant="secondary"
          onClick={onRetryFailed}
          disabled={busy || !retryable}
        >
          {t('common.channels.profileWizard.result.retryFailed')}
        </Button>
        <Button
          variant="primary"
          className="ml-auto"
          onClick={onBackToForm}
          disabled={busy}
        >
          {t('common.channels.profileWizard.back')}
        </Button>
      </div>
    </section>
  )
}
