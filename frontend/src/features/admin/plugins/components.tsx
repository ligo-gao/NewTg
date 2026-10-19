import { useEffect, type ReactNode } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { StatusBadge } from '@/shared/ui/status-badge'
import { Tag } from '@/shared/ui/tag'

import type { TaskPluginRuntimeStatus } from './api'
import { RUNTIME_STATUS_VARIANTS, pluginInitial, runtimeStatusKey } from './utils'

/** 只读卡片容器：六个 Tab 共用同一视觉 token。 */
export function SectionCard({
  title,
  description,
  children,
  testId,
}: {
  title?: string
  description?: string
  children: ReactNode
  testId?: string
}) {
  return (
    <section
      data-testid={testId}
      className="rounded-xl border border-border-default bg-surface-card p-5"
    >
      {title !== undefined ? (
        <header className="mb-3">
          <h2 className="text-[15px] font-semibold leading-[22px] text-ink-1">
            {title}
          </h2>
          {description !== undefined ? (
            <p className="mt-0.5 text-xs text-ink-3">{description}</p>
          ) : null}
        </header>
      ) : null}
      {children}
    </section>
  )
}

export function EmptyState({
  title,
  action,
  testId,
}: {
  title: string
  action?: ReactNode
  testId?: string
}) {
  return (
    <div
      data-testid={testId}
      className="flex flex-col items-center gap-3 rounded-xl border border-dashed border-border-default px-4 py-10 text-center"
    >
      <p className="text-sm text-ink-3">{title}</p>
      {action}
    </div>
  )
}

export function ErrorState({
  message,
  onRetry,
  testId,
}: {
  message: string
  onRetry?: () => void
  testId?: string
}) {
  const { t } = useI18n()
  return (
    <div
      data-testid={testId}
      role="alert"
      className="flex flex-wrap items-center gap-3 rounded-lg border border-danger-line bg-danger-bg px-3 py-2 text-sm text-danger"
    >
      <span>{message}</span>
      {onRetry !== undefined ? (
        <Button variant="ghost" className="h-7 px-2 text-xs" onClick={onRetry}>
          {t('admin.task_plugins.common.retry')}
        </Button>
      ) : null}
    </div>
  )
}

export function SkeletonRows({ rows = 5 }: { rows?: number }) {
  return (
    <div data-testid="plugin-skeleton" aria-hidden="true" className="space-y-2">
      {Array.from({ length: rows }, (_, index) => (
        <div
          key={index}
          className="h-14 animate-pulse rounded-lg border border-border-hairline bg-surface-subtle"
        />
      ))}
    </div>
  )
}

export function RuntimeStatusBadge({
  status,
}: {
  status: TaskPluginRuntimeStatus
}) {
  const { t } = useI18n()
  return (
    <StatusBadge variant={RUNTIME_STATUS_VARIANTS[status]}>
      {t(runtimeStatusKey(status))}
    </StatusBadge>
  )
}

/** 来源是分类（内置/上传），用中性 Tag，不按来源分配颜色。 */
export function SourceTag({ source }: { source: 'builtin' | 'upload' }) {
  const { t } = useI18n()
  return <Tag variant="neutral">{t(`admin.task_plugins.source.${source}`)}</Tag>
}

export function PluginIcon({
  icon,
  pluginKey,
  size = 'lg',
}: {
  icon?: string | null
  pluginKey: string
  size?: 'sm' | 'lg'
}) {
  const classes =
    size === 'sm'
      ? 'size-7 rounded-lg text-xs'
      : 'size-12 rounded-[8px] text-base'
  return (
    <span
      aria-hidden="true"
      data-testid="plugin-icon"
      className={`inline-flex shrink-0 items-center justify-center bg-brand-subtle font-semibold text-brand ${classes}`}
    >
      {icon !== null && icon !== undefined && icon !== '' ? icon : pluginInitial(pluginKey)}
    </span>
  )
}

/** 行内启用开关（role=switch）；可逆操作不弹确认（§6.10）。 */
export function Switch({
  checked,
  disabled,
  label,
  onChange,
}: {
  checked: boolean
  disabled: boolean
  label: string
  onChange: (next: boolean) => void
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={[
        'relative inline-flex h-5 w-9 items-center rounded-full border transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50',
        checked
          ? 'border-brand bg-brand'
          : 'border-border-default bg-surface-subtle',
      ].join(' ')}
    >
      <span
        aria-hidden="true"
        className={[
          'inline-block size-3.5 rounded-full bg-surface-card transition-transform',
          checked ? 'translate-x-[18px]' : 'translate-x-[3px]',
        ].join(' ')}
      />
    </button>
  )
}

export function OfflineNote({ visible }: { visible: boolean }) {
  const { t } = useI18n()
  if (!visible) return null
  return <p className="text-xs text-ink-3">{t('admin.task_plugins.common.offline')}</p>
}

export function Pagination({
  page,
  pageSize,
  total,
  onPageChange,
  testId,
}: {
  page: number
  pageSize: number
  total: number
  onPageChange: (page: number) => void
  testId?: string
}) {
  const { t } = useI18n()
  const pages = Math.max(1, Math.ceil(total / pageSize))
  const from = total === 0 ? 0 : (page - 1) * pageSize + 1
  const to = Math.min(page * pageSize, total)
  return (
    <nav
      data-testid={testId}
      aria-label={t('admin.task_plugins.common.pagination_label')}
      className="flex flex-wrap items-center justify-between gap-3 text-xs text-ink-3"
    >
      <span>
        {t('admin.task_plugins.common.page_summary', { from, to, total })}
      </span>
      <span className="flex items-center gap-2">
        <span>{t('admin.task_plugins.common.page_of', { page, pages })}</span>
        <Button
          variant="secondary"
          className="h-7 px-2 text-xs"
          disabled={page <= 1}
          onClick={() => onPageChange(page - 1)}
        >
          {t('admin.task_plugins.common.previous')}
        </Button>
        <Button
          variant="secondary"
          className="h-7 px-2 text-xs"
          disabled={page >= pages}
          onClick={() => onPageChange(page + 1)}
        >
          {t('admin.task_plugins.common.next')}
        </Button>
      </span>
    </nav>
  )
}

export interface ConfirmDialogProps {
  open: boolean
  title: string
  description?: string
  confirmLabel: string
  cancelLabel?: string
  onCancel: () => void
  onConfirm: () => void
  children?: ReactNode
  dangerous?: boolean
  confirmDisabled?: boolean
}

/**
 * 危险确认（§4 DangerConfirm）：`role=alertdialog`，初始焦点落在「取消」上，
 * 避免回车误触不可逆操作（Dialog 基元先把焦点放到面板，故等一帧再显式聚焦）。
 */
export function ConfirmDialog({
  open,
  title,
  description,
  confirmLabel,
  cancelLabel,
  onCancel,
  onConfirm,
  children,
  dangerous = true,
  confirmDisabled = false,
}: ConfirmDialogProps) {
  const { t } = useI18n()

  useEffect(() => {
    if (!open) return
    const timer = globalThis.setTimeout(() => {
      const candidates = document.querySelectorAll<HTMLElement>('[data-danger-cancel]')
      candidates[candidates.length - 1]?.focus()
    }, 0)
    return () => globalThis.clearTimeout(timer)
  }, [open])

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) onCancel()
      }}
      title={title}
      description={description}
      role="alertdialog"
      footer={
        <>
          <Button variant="secondary" data-danger-cancel onClick={onCancel}>
            {cancelLabel ?? t('admin.task_plugins.common.cancel')}
          </Button>
          <Button
            variant={dangerous ? 'danger' : 'primary'}
            disabled={confirmDisabled}
            onClick={onConfirm}
          >
            {confirmLabel}
          </Button>
        </>
      }
    >
      {children}
    </Dialog>
  )
}
