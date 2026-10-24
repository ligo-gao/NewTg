import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert } from '@/shared/ui/form'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'
import { Tag } from '@/shared/ui/tag'

import { getDelivery } from './api'
import { categoryLabelKey } from './registry'
import type { DeliveryStatus, NotificationDelivery } from './types'
import { maskRecipient } from './utils'

const STATUS_VARIANTS: Record<DeliveryStatus | 'queued', StatusBadgeVariant> = {
  queued: 'neutral',
  pending: 'neutral',
  sent: 'success',
  failed: 'danger',
  bounced: 'warning',
  suppressed: 'neutral',
}

export interface DeliveryDetailDrawerProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** 列表行作为初始数据；打开后按 id 刷新一次权威详情。 */
  delivery: NotificationDelivery | null
}

function absoluteTime(value: string, locale: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeStyle: 'medium',
  }).format(date)
}

function DetailRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="grid grid-cols-[9rem_1fr] gap-2 py-1.5">
      <dt className="text-meta text-ink-3">{label}</dt>
      <dd className="break-all text-body text-ink-1">{children}</dd>
    </div>
  )
}

/**
 * 投递记录详情抽屉（§6.9）。只读展示排障字段：attempt、next_retry_at、
 * dedupe_key、provider_message_id 与脱敏后的 last_error。
 *
 * MVP 不提供手动重试（设计 §6.9 明确「重试由 worker 按 1/5/25 分钟执行」），
 * 因此这里刻意没有重试按钮——不渲染一个调用不存在能力的假入口。
 */
export function DeliveryDetailDrawer({
  open,
  onOpenChange,
  delivery,
}: DeliveryDetailDrawerProps) {
  const { t, locale } = useI18n()
  const query = useQuery({
    queryKey: ['notification-delivery-detail', delivery?.id],
    queryFn: ({ signal }) => getDelivery(delivery?.id ?? 0, signal),
    enabled: open && delivery !== null,
    // 列表行已带全部字段，详情刷新失败时仍可展示；不要用空占位覆盖。
    initialData: delivery ?? undefined,
    staleTime: 0,
  })
  const data = query.data ?? delivery
  if (data === null) return null

  const statusKey = data.status === 'queued' ? 'pending' : data.status

  return (
    <Dialog
      open={open && delivery !== null}
      onOpenChange={onOpenChange}
      title={t('settings.notification_templates.deliveries.detail_title')}
      description={data.dedupe_key}
      variant="sheet"
      panelClassName="sm:max-w-[560px]"
      footer={
        <div className="flex justify-end">
          <Button variant="secondary" onClick={() => onOpenChange(false)}>
            {t('settings.notification_templates.deliveries.detail_close')}
          </Button>
        </div>
      }
    >
      <div className="space-y-3" data-testid="notification-delivery-drawer">
        {query.isError ? (
          <FormAlert tone="danger">
            {t('settings.notification_templates.deliveries.detail_refresh_error')}
          </FormAlert>
        ) : null}
        <dl className="divide-y divide-border-hairline">
          <DetailRow label={t('settings.notification_templates.deliveries.time')}>
            <span title={data.created_at}>{absoluteTime(data.created_at, locale)}</span>
          </DetailRow>
          <DetailRow label={t('settings.notification_templates.table.category')}>
            <Tag variant="neutral">{t(categoryLabelKey(data.category))}</Tag>
          </DetailRow>
          <DetailRow label={t('settings.notification_templates.table.channel')}>
            <Tag variant="neutral">
              {t(`settings.notification_templates.channel.${data.channel}`)}
            </Tag>
          </DetailRow>
          <DetailRow label={t('settings.notification_templates.deliveries.recipient')}>
            {maskRecipient(data.recipient)}
          </DetailRow>
          <DetailRow label={t('settings.notification_templates.table.locale')}>
            {data.locale ?? '—'}
          </DetailRow>
          <DetailRow label={t('settings.notification_templates.deliveries.status')}>
            <StatusBadge variant={STATUS_VARIANTS[data.status]}>
              {t(`settings.notification_templates.deliveries.status_${statusKey}`)}
            </StatusBadge>
          </DetailRow>
          <DetailRow label={t('settings.notification_templates.deliveries.detail_attempt')}>
            {data.attempt}
          </DetailRow>
          <DetailRow label={t('settings.notification_templates.deliveries.detail_next_retry')}>
            {data.next_retry_at === null ? '—' : <span title={data.next_retry_at}>{absoluteTime(data.next_retry_at, locale)}</span>}
          </DetailRow>
          <DetailRow label={t('settings.notification_templates.deliveries.detail_dedupe_key')}>
            <code className="font-mono text-meta">{data.dedupe_key}</code>
          </DetailRow>
          <DetailRow label={t('settings.notification_templates.deliveries.detail_provider_message_id')}>
            {data.provider_message_id === undefined || data.provider_message_id === ''
              ? '—'
              : <code className="font-mono text-meta">{data.provider_message_id}</code>}
          </DetailRow>
          <DetailRow label={t('settings.notification_templates.deliveries.error')}>
            {data.error === undefined || data.error === '' ? (
              '—'
            ) : (
              <span className="whitespace-pre-wrap font-mono text-meta text-ink-2">
                {data.error}
              </span>
            )}
          </DetailRow>
        </dl>
      </div>
    </Dialog>
  )
}
