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
  /** 列表或其会话探测仍在加载：行数未知时不允许导出。 */
  listLoading?: boolean
  /** 403（角色越界）：关闭对话框后由页面走标准 403 路径。 */
  onForbidden?: () => void
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
  listLoading = false,
  onForbidden,
  onExported,
}: ExportDialogProps) {
  const { t, locale } = useI18n()
  const [scope, setScope] = useState<ExportScope>('filtered')
  const [format, setFormat] = useState<ExportFormat>('markdown')
  const [includeSensitive, setIncludeSensitive] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<ExportErrorKind>(null)
  // 惰性初始化：对话框可能一挂载就是 open（路由/状态恢复），此时首帧
  // 必须已经知道离线，不能等 effect 跑完才禁用 Download。
  const [offline, setOffline] = useState(
    () => typeof navigator !== 'undefined' && navigator.onLine === false,
  )
  const abortRef = useRef<AbortController | null>(null)
  const wasOpenRef = useRef(false)

  // 只在「打开」这一瞬间重置。父组件因无关原因重渲染（例如列表刷新导致
  // revealSensitive 变化）不能把用户已经选好的范围/格式清掉。
  useEffect(() => {
    if (open && !wasOpenRef.current) {
      setScope('filtered')
      setFormat('markdown')
      setIncludeSensitive(canRevealSensitive && revealSensitive)
      setError(null)
      setSubmitting(false)
    }
    wasOpenRef.current = open
  }, [open, canRevealSensitive, revealSensitive])

  // 打开时先读一次连接状态，并持续跟随 online/offline 事件：离线时下载
  // 按钮从第一帧就禁用，而不是等请求失败后才报错（UI §7.5）。
  useEffect(() => {
    if (!open) return
    const sync = (): void => {
      setOffline(
        typeof navigator !== 'undefined' && navigator.onLine === false,
      )
    }
    sync()
    window.addEventListener('online', sync)
    window.addEventListener('offline', sync)
    return () => {
      window.removeEventListener('online', sync)
      window.removeEventListener('offline', sync)
    }
  }, [open])

  useEffect(() => {
    return () => {
      abortRef.current?.abort()
    }
  }, [])

  const downloadDisabled =
    submitting ||
    listLoading ||
    offline ||
    (scope === 'filtered' && filteredTotal === 0)

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
        } else if (caught.kind === 'forbidden') {
          // 角色越界：关闭对话框，交给页面的标准 403 路径，而不是留一个
          // 无法重试的行内错误。
          setSubmitting(false)
          close()
          onForbidden?.()
          return
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
  // 离线提示优先于「暂无错误」：按钮已禁用，用户需要看到原因。
  const statusMessage =
    errorMessage ?? (offline ? t('logs.usage.export.offline') : null)

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
        <legend className="text-meta font-semibold text-ink-1">
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
            <span className="block text-body text-ink-1">
              {t('logs.usage.export.scope_filtered')}
            </span>
            <span className="block text-meta text-ink-2">
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
            <span className="block text-body text-ink-1">
              {t('logs.usage.export.scope_all')}
            </span>
            <span className="block text-meta text-ink-2">
              {t('logs.usage.export.scope_all_summary')}
            </span>
          </span>
        </label>
      </fieldset>

      <fieldset className="mt-4 flex flex-col gap-2">
        <legend className="text-meta font-semibold text-ink-1">
          {t('logs.usage.export.format')}
        </legend>
        <label className="flex cursor-pointer items-center gap-2 text-body text-ink-1">
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
        <label className="flex cursor-pointer items-center gap-2 text-body text-ink-1">
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

      <p className="mt-4 text-meta text-ink-3">
        {t('logs.usage.export.fields')}
      </p>

      {canRevealSensitive ? (
        <label className="mt-3 flex items-start gap-2 rounded-lg bg-warning-bg p-2.5 text-body text-ink-1">
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
              <span className="block text-meta text-warning">
                {t('logs.usage.export.sensitive_notice')}
              </span>
            ) : (
              <span className="block text-meta text-ink-2">
                {t('logs.usage.export.sensitive_off')}
              </span>
            )}
          </span>
        </label>
      ) : (
        <p className="mt-3 text-meta text-ink-3">
          {t('logs.usage.export.sensitive_unavailable')}
        </p>
      )}

      {scope === 'filtered' && filteredTotal === 0 ? (
        <p className="mt-2 text-meta text-ink-3">
          {t('logs.usage.export.empty')}
        </p>
      ) : null}

      <div aria-live="polite" role="status" className="mt-3 min-h-5">
        {statusMessage !== null ? (
          <p
            data-testid="export-error"
            role={offline && error === null ? 'status' : 'alert'}
            className="text-body text-danger"
          >
            {statusMessage}
            {error !== 'row_limit' && error !== 'empty' && !offline ? (
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
          <p className="text-body text-ink-2">
            {t('logs.usage.export.generating')}
          </p>
        ) : null}
      </div>
    </Dialog>
  )
}
