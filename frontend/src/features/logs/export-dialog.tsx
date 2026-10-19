import { useEffect, useRef, useState } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'

import {
  downloadUsageLogs,
  ExportEmptyError,
  ExportRowLimitError,
  type ExportFormat,
  type ExportScope,
} from './export-api'
import type { UsageLogSearch } from './search'

export type ExportErrorKind =
  | 'row_limit'
  | 'empty'
  | 'offline'
  | 'rate_limited'
  | 'failed'
  | null

export interface ExportDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  search: UsageLogSearch
  /** 当前筛选范围的总行数（列表 total）。 */
  filteredTotal: number
  /** 工具栏敏感开关的当前值，作为导出默认值。 */
  revealSensitive: boolean
  /** `log.prompt.view` 门：无权限时不允许在导出里选择敏感内容。 */
  canRevealSensitive: boolean
  canFilterByUsername: boolean
  onExported?: (filename: string) => void
}

/**
 * F-05-07 导出对话框（modules-05 §7.2 / §7.5 / §7.7）。
 *
 * 服务端流式生成文件，前端只负责参数与下载：范围/格式/敏感内容三项都在
 * 这里显式呈现，避免「导出悄悄放宽了读取边界」的误解。422（超 10,000 行）
 * 与 204（0 行）都留在对话框内，并保持对话框打开让用户改范围后重试。
 */
export function ExportDialog({
  open,
  onOpenChange,
  search,
  filteredTotal,
  revealSensitive,
  canRevealSensitive,
  canFilterByUsername,
  onExported,
}: ExportDialogProps) {
  const { t, locale } = useI18n()
  const [scope, setScope] = useState<ExportScope>('filtered')
  const [format, setFormat] = useState<ExportFormat>('markdown')
  const [includeSensitive, setIncludeSensitive] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<ExportErrorKind>(null)
  const abortRef = useRef<AbortController | null>(null)

  // 每次打开重置为「当前筛选 + 跟随开关」，不保留上一次的跨次选择——
  // 导出是只读快照，用户不应被上一次的 scope/format 默默影响。
  useEffect(() => {
    if (!open) return
    setScope('filtered')
    setFormat('markdown')
    setIncludeSensitive(canRevealSensitive && revealSensitive)
    setError(null)
    setSubmitting(false)
  }, [open, canRevealSensitive, revealSensitive])

  useEffect(() => {
    return () => {
      abortRef.current?.abort()
    }
  }, [])

  const downloadDisabled =
    submitting || (scope === 'filtered' && filteredTotal === 0)

  function close(): void {
    abortRef.current?.abort()
    abortRef.current = null
    onOpenChange(false)
  }

  async function submit(): Promise<void> {
    if (downloadDisabled) return
    abortRef.current?.abort()
    const controller = new AbortController()
    abortRef.current = controller
    setSubmitting(true)
    setError(null)
    try {
      const result = await downloadUsageLogs({
        search,
        scope,
        format,
        // 无权限时永远不发送 1：服务端会重校验，但前端也不该先越过权限。
        revealSensitive: canRevealSensitive && includeSensitive,
        canFilterByUsername,
        signal: controller.signal,
      })
      onExported?.(result.filename)
      setSubmitting(false)
      onOpenChange(false)
    } catch (caught) {
      if (caught instanceof DOMException && caught.name === 'AbortError') {
        setSubmitting(false)
        return
      }
      if (caught instanceof ExportRowLimitError) {
        setError('row_limit')
      } else if (caught instanceof ExportEmptyError) {
        setError('empty')
      } else if (caught instanceof ApiError) {
        if (caught.kind === 'rate_limited') {
          setError('rate_limited')
        } else if (caught.kind === 'offline') {
          setError('offline')
        } else {
          setError('failed')
        }
      } else {
        setError('failed')
      }
      setSubmitting(false)
    }
  }

  const errorMessage =
    error === 'row_limit'
      ? t('logs.usage.export.row_limit')
      : error === 'empty'
        ? t('logs.usage.export.empty')
        : error === 'offline'
          ? t('logs.usage.export.offline')
          : error === 'rate_limited'
            ? t('logs.usage.export.rate_limited')
            : error === 'failed'
              ? t('logs.usage.export.failed')
              : null

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (next) onOpenChange(true)
        else close()
      }}
      title={t('logs.usage.export.title')}
      footer={
        <>
          <Button variant="secondary" onClick={close} data-testid="export-cancel">
            {t('logs.usage.export.cancel')}
          </Button>
          <Button
            variant="primary"
            data-testid="export-download"
            disabled={downloadDisabled}
            title={
              scope === 'filtered' && filteredTotal === 0
                ? t('logs.usage.export.empty')
                : undefined
            }
            onClick={() => {
              void submit()
            }}
          >
            {submitting
              ? t('logs.usage.export.generating')
              : t('logs.usage.export.download')}
          </Button>
        </>
      }
    >
      <fieldset className="flex flex-col gap-2">
        <legend className="text-[13px] font-semibold text-ink-1">
          {t('logs.usage.export.scope')}
        </legend>
        <label className="flex cursor-pointer items-start gap-2 rounded-lg border border-border-default p-2.5">
          <input
            type="radio"
            name="export-scope"
            value="filtered"
            data-testid="export-scope-filtered"
            checked={scope === 'filtered'}
            onChange={() => {
              setScope('filtered')
              setError(null)
            }}
          />
          <span>
            <span className="block text-sm text-ink-1">
              {t('logs.usage.export.scope_filtered')}
            </span>
            <span className="block text-xs text-ink-2">
              {t('logs.usage.export.scope_filtered_summary', {
                total: new Intl.NumberFormat(locale).format(filteredTotal),
              })}
            </span>
          </span>
        </label>
        <label className="flex cursor-pointer items-start gap-2 rounded-lg border border-border-default p-2.5">
          <input
            type="radio"
            name="export-scope"
            value="all"
            data-testid="export-scope-all"
            checked={scope === 'all'}
            onChange={() => {
              setScope('all')
              setError(null)
            }}
          />
          <span>
            <span className="block text-sm text-ink-1">
              {t('logs.usage.export.scope_all')}
            </span>
            <span className="block text-xs text-ink-2">
              {t('logs.usage.export.scope_all_summary')}
            </span>
          </span>
        </label>
      </fieldset>

      <fieldset className="mt-4 flex flex-col gap-2">
        <legend className="text-[13px] font-semibold text-ink-1">
          {t('logs.usage.export.format')}
        </legend>
        <label className="flex cursor-pointer items-center gap-2 text-sm text-ink-1">
          <input
            type="radio"
            name="export-format"
            value="txt"
            data-testid="export-format-txt"
            checked={format === 'txt'}
            onChange={() => {
              setFormat('txt')
              setError(null)
            }}
          />
          {t('logs.usage.export.format_txt')}
        </label>
        <label className="flex cursor-pointer items-center gap-2 text-sm text-ink-1">
          <input
            type="radio"
            name="export-format"
            value="markdown"
            data-testid="export-format-markdown"
            checked={format === 'markdown'}
            onChange={() => {
              setFormat('markdown')
              setError(null)
            }}
          />
          {t('logs.usage.export.format_markdown')}
        </label>
      </fieldset>

      <p className="mt-4 text-xs text-ink-3">
        {t('logs.usage.export.fields')}
      </p>

      {canRevealSensitive ? (
        <label className="mt-3 flex items-start gap-2 rounded-lg bg-warning-bg p-2.5 text-sm text-ink-1">
          <input
            type="checkbox"
            data-testid="export-sensitive"
            checked={includeSensitive}
            onChange={(event) => {
              setIncludeSensitive(event.target.checked)
              setError(null)
            }}
          />
          <span>
            <span className="block">
              {t('logs.usage.export.sensitive')}
            </span>
            {includeSensitive ? (
              <span className="block text-xs text-warning">
                {t('logs.usage.export.sensitive_notice')}
              </span>
            ) : (
              <span className="block text-xs text-ink-2">
                {t('logs.usage.export.sensitive_off')}
              </span>
            )}
          </span>
        </label>
      ) : (
        <p className="mt-3 text-xs text-ink-3">
          {t('logs.usage.export.sensitive_unavailable')}
        </p>
      )}

      {scope === 'filtered' && filteredTotal === 0 ? (
        <p className="mt-2 text-xs text-ink-3">
          {t('logs.usage.export.empty')}
        </p>
      ) : null}

      <div aria-live="polite" role="status" className="mt-3 min-h-5">
        {errorMessage !== null ? (          <p
            data-testid="export-error"
            role="alert"
            className="text-sm text-danger"
          >
            {errorMessage}
            {error !== 'row_limit' && error !== 'empty' ? (
              <button
                type="button"
                data-testid="export-retry"
                onClick={() => {
                  void submit()
                }}
                className="ml-2 font-medium text-brand underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              >
                {t('logs.usage.export.retry')}
              </button>
            ) : null}
          </p>
        ) : submitting ? (
          <p className="text-sm text-ink-2">
            {t('logs.usage.export.generating')}
          </p>
        ) : null}
      </div>
    </Dialog>
  )
}
