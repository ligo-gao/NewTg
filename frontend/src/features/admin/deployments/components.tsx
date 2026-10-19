import { useEffect, type ReactNode } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import type { DeployPriceEstimation } from './api'
import { SecretField } from './secret-field'
import { formatAmount, type EnvRow } from './utils'

/** 只读卡片容器：部署页的三个 Tab 共用同一视觉 token。 */
export function SectionCard({
  title,
  description,
  children,
  testId,
}: {
  title: string
  description?: string
  children: ReactNode
  testId?: string
}) {
  return (
    <section
      data-testid={testId}
      className="rounded-xl border border-border-default bg-surface-card p-5"
    >
      <header className="mb-3">
        <h2 className="text-[15px] font-semibold leading-[22px] text-ink-1">
          {title}
        </h2>
        {description !== undefined ? (
          <p className="mt-0.5 text-xs text-ink-3">{description}</p>
        ) : null}
      </header>
      {children}
    </section>
  )
}

/**
 * 危险确认的初始焦点：落在「取消」上。Dialog 基元会在挂载后把焦点移到面板，
 * 而 autoFocus 在提交阶段就会触发，因此这里等一帧后显式聚焦取消按钮。
 */
export function useDangerCancelFocus(open: boolean): void {
  useEffect(() => {
    if (!open) return
    const timer = globalThis.setTimeout(() => {
      const candidates = document.querySelectorAll<HTMLElement>('[data-danger-cancel]')
      candidates[candidates.length - 1]?.focus()
    }, 0)
    return () => globalThis.clearTimeout(timer)
  }, [open])
}

/**
 * 危险确认（UI §4 / §6.13）。`role=alertdialog` 由 Dialog 负责；焦点默认
 * 落在「取消」上，避免回车误触不可逆操作。
 */
export function ConfirmDialog({
  open,
  title,
  description,
  confirmLabel,
  onCancel,
  onConfirm,
  children,
  dangerous = true,
  confirmDisabled = false,
}: {
  open: boolean
  title: string
  description?: string
  confirmLabel: string
  onCancel: () => void
  onConfirm: () => void
  children?: ReactNode
  dangerous?: boolean
  confirmDisabled?: boolean
}) {
  const { t } = useI18n()
  useDangerCancelFocus(open)
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
            {t('admin.deployments.confirm.cancel')}
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

/**
 * 费用估算卡（F-08-54）。金额与币种来自 provider，原样展示；分解项只在
 * provider 实际返回时渲染（缺失项不用 0 伪造，§3.20.3）。
 * 「provider 估算，非用户扣费」常驻在金额旁。
 */
export function PriceEstimateCard({
  estimate,
  loading,
  error,
  onRetry,
}: {
  estimate: DeployPriceEstimation | null
  loading: boolean
  error: string | null
  onRetry?: () => void
}) {
  const { t } = useI18n()
  const breakdown = estimate?.price_breakdown
  const items: { key: string; labelKey: string; amount: number }[] = []
  const push = (key: keyof NonNullable<typeof breakdown>, labelKey: string) => {
    const value = breakdown?.[key]
    if (typeof value === 'number' && Number.isFinite(value)) {
      items.push({ key, labelKey, amount: value })
    }
  }
  push('compute_cost', 'admin.deployments.price.compute')
  push('network_cost', 'admin.deployments.price.network')
  push('storage_cost', 'admin.deployments.price.storage')
  push('hourly_rate', 'admin.deployments.price.hourly')
  push('ionet_fee', 'admin.deployments.price.ionet_fee')
  push('currency_conversion_fee', 'admin.deployments.price.conversion_fee')
  push('discount', 'admin.deployments.price.discount')

  return (
    <div
      className="rounded-lg border border-border-default bg-surface-subtle p-4"
      data-testid="price-estimate-card"
      role="status"
      aria-live="polite"
    >
      <p className="text-xs font-medium text-ink-3">
        {t('admin.deployments.price.title')}
      </p>
      {loading ? (
        <p className="mt-1 text-sm text-ink-2">
          {t('admin.deployments.price.estimating')}
        </p>
      ) : null}
      {error !== null && !loading ? (
        <div className="mt-1 flex flex-wrap items-center gap-2">
          <p className="text-sm text-danger">
            {t('admin.deployments.price.unavailable')}
          </p>
          {onRetry !== undefined ? (
            <Button
              variant="ghost"
              className="h-7 px-2 text-xs"
              onClick={onRetry}
            >
              {t('admin.deployments.price.retry')}
            </Button>
          ) : null}
        </div>
      ) : null}
      {estimate !== null && !loading ? (
        <div className="mt-1">
          <p className="font-mono text-lg font-semibold text-ink-1">
            {formatAmount(estimate.estimated_cost, estimate.currency)}
          </p>
          {items.length > 0 ? (
            <dl className="mt-2 grid grid-cols-1 gap-1 text-xs text-ink-2 sm:grid-cols-2">
              {items.map((item) => (
                <div key={item.key} className="flex justify-between gap-2">
                  <dt>{t(item.labelKey)}</dt>
                  <dd className="font-mono">
                    {formatAmount(item.amount, estimate.currency)}
                  </dd>
                </div>
              ))}
            </dl>
          ) : null}
        </div>
      ) : null}
      {/* 常驻提示：估算金额不是用户扣费，无论是否拿到报价都要显示。 */}
      <p className="mt-1 text-xs text-ink-3">
        {t('admin.deployments.price.provider_hint')}
      </p>
    </div>
  )
}

/**
 * 环境变量行编辑器。`secret` 模式使用 SecretField（单次输入、不回显、
 * 可切换明文）；普通模式是键/值文本行。空键在提交时被丢弃。
 */
export function EnvEditor({
  idPrefix,
  label,
  rows,
  onChange,
  secret = false,
  disabled = false,
  hint,
}: {
  idPrefix: string
  label: string
  rows: EnvRow[]
  onChange: (rows: EnvRow[]) => void
  secret?: boolean
  disabled?: boolean
  hint?: string
}) {
  const { t } = useI18n()
  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center justify-between gap-3">
        <p className="text-[13px] font-medium text-ink-1">{label}</p>
        <Button
          variant="ghost"
          className="h-7 px-2 text-xs"
          disabled={disabled}
          onClick={() => onChange([...rows, { key: '', value: '' }])}
        >
          {t('admin.deployments.env.add')}
        </Button>
      </div>
      {hint !== undefined ? <p className="text-xs text-ink-3">{hint}</p> : null}
      {rows.length === 0 ? (
        <p className="text-xs text-ink-3">{t('admin.deployments.env.empty')}</p>
      ) : null}
      {rows.map((row, index) => (
        <div key={index} className="flex items-start gap-2">
          <input
            aria-label={t('admin.deployments.env.key')}
            value={row.key}
            disabled={disabled}
            onChange={(event) => {
              const next = [...rows]
              next[index] = { ...row, key: event.target.value }
              onChange(next)
            }}
            className="h-10 w-1/3 rounded-lg border border-border-default bg-surface-card px-3 font-mono text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-50"
            placeholder="KEY"
          />
          <div className="flex-1">
            {secret ? (
              <SecretField
                id={`${idPrefix}-value-${index}`}
                label={t('admin.deployments.env.value')}
                value={row.value}
                showBadge={false}
                disabled={disabled}
                onChange={(value) => {
                  const next = [...rows]
                  next[index] = { ...row, value: value ?? '' }
                  onChange(next)
                }}
              />
            ) : (
              <input
                aria-label={t('admin.deployments.env.value')}
                value={row.value}
                disabled={disabled}
                onChange={(event) => {
                  const next = [...rows]
                  next[index] = { ...row, value: event.target.value }
                  onChange(next)
                }}
                className="h-10 w-full rounded-lg border border-border-default bg-surface-card px-3 font-mono text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-50"
                placeholder="value"
              />
            )}
          </div>
          <Button
            variant="ghost"
            className="h-10 px-2 text-xs"
            disabled={disabled}
            aria-label={t('admin.deployments.env.remove')}
            onClick={() => onChange(rows.filter((_, i) => i !== index))}
          >
            ×
          </Button>
        </div>
      ))}
    </div>
  )
}
