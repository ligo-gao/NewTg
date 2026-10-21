import { useInfiniteQuery } from '@tanstack/react-query'

import { useI18n } from '@/shared/lib/i18n-context'
import { formatCredits } from '@/shared/lib/format'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'

import { formatYuan } from '../topup/money'
import { formatSubscriptionTime } from './format'
import { listSubscriptionEvents } from './subscription-api'
import type {
  AvailableSubscriptionPlan,
  SubscriptionEvent,
  UserSubscription,
} from './types'

/** 服务端单次窗口上限（handlers_subscriptions.go: subscriptionEventLimit）。 */
const EVENT_PAGE_LIMIT = 500

export const EVENTS_KEY = ['subscription', 'events'] as const

/**
 * kind → 词典键。生成类型只列出契约里的八种；`renewal_failed` 是 S4 自动
 * 续费失败事件（store 的 CHECK 白名单包含它），所以映射保持开放：未知值
 * 原样显示，而不是当成空事件或渲染 undefined。
 */
const EVENT_KIND_KEYS: Record<string, string> = {
  created: 'account.subscription.events.kind.created',
  renewed: 'account.subscription.events.kind.renewed',
  upgraded: 'account.subscription.events.kind.upgraded',
  downgraded: 'account.subscription.events.kind.downgraded',
  refunded: 'account.subscription.events.kind.refunded',
  cancelled: 'account.subscription.events.kind.cancelled',
  expired: 'account.subscription.events.kind.expired',
  reset: 'account.subscription.events.kind.reset',
  renewal_failed: 'account.subscription.events.kind.renewal_failed',
}

/** 自动续费失败的受控原因码（billing.AutoRenewFailureReason）。 */
const FAILURE_REASON_KEYS: Record<string, string> = {
  payment_method_deleted: 'account.subscription.events.reason.payment_method_deleted',
  payment_method_missing: 'account.subscription.events.reason.payment_method_missing',
  provider_rejected: 'account.subscription.events.reason.provider_rejected',
  provider_unavailable: 'account.subscription.events.reason.provider_unavailable',
  insufficient_funds: 'account.subscription.events.reason.insufficient_funds',
}

function asNumber(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined
}

function asString(value: unknown): string | undefined {
  return typeof value === 'string' && value.length > 0 ? value : undefined
}

function planTitle(
  plans: readonly AvailableSubscriptionPlan[],
  value: unknown,
): string | undefined {
  const id = asNumber(value)
  if (id === undefined) return undefined
  return plans.find((plan) => plan.id === id)?.title
}

function DetailRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt className="text-ink-3">{label}</dt>
      <dd className="text-right font-medium tabular-nums text-ink-2">{value}</dd>
    </div>
  )
}

/**
 * 单条事件的 detail 摘要。
 *
 * detail 是服务端写入的受控 jsonb（R-18/R-24/R-34）：只含 id 与金额。这里
 * 仍然按白名单逐键取，未知键一律不渲染——事件流是只读视图，不承担把任意
 * JSON 反射给用户的责任。
 */
function EventDetail({
  event,
  plans,
}: {
  event: SubscriptionEvent
  plans: readonly AvailableSubscriptionPlan[]
}) {
  const { t } = useI18n()
  const detail = event.detail as Record<string, unknown>
  const rows: { label: string; value: string }[] = []

  const amount = asNumber(detail.amount)
  if (amount !== undefined) {
    rows.push({ label: t('account.subscription.events.detail.amount'), value: formatYuan(amount) })
  }
  const pricePaid = asNumber(detail.price_paid_amount)
  if (pricePaid !== undefined) {
    rows.push({ label: t('account.subscription.events.detail.amount'), value: formatYuan(pricePaid) })
  }
  const priceDelta = asNumber(detail.price_delta)
  if (priceDelta !== undefined && priceDelta > 0) {
    rows.push({
      label: t('account.subscription.events.detail.price_delta'),
      value: formatYuan(priceDelta),
    })
  }
  const amountTotal = asNumber(detail.amount_total)
  if (amountTotal !== undefined) {
    rows.push({
      label: t('account.subscription.events.detail.amount_total'),
      value:
        amountTotal === 0
          ? t('account.subscription.mine.unlimited')
          : formatCredits(amountTotal),
    })
  }
  const creditsDelta = asNumber(detail.credits_delta)
  if (creditsDelta !== undefined && creditsDelta !== 0) {
    rows.push({
      label: t('account.subscription.events.detail.credits_delta'),
      value:
        creditsDelta < 0
          ? `−${formatCredits(-creditsDelta)}`
          : `+${formatCredits(creditsDelta)}`,
    })
  }
  const fromPlan = planTitle(plans, detail.from_plan_id)
  if (fromPlan !== undefined) {
    rows.push({ label: t('account.subscription.events.detail.from_plan'), value: fromPlan })
  }
  const toPlan = planTitle(plans, detail.to_plan_id)
  if (toPlan !== undefined) {
    rows.push({ label: t('account.subscription.events.detail.to_plan'), value: toPlan })
  }
  const plan = planTitle(plans, detail.plan_id)
  if (plan !== undefined) {
    rows.push({ label: t('account.subscription.events.detail.plan'), value: plan })
  }
  const reason = asString(detail.reason)
  if (reason !== undefined) {
    const reasonKey = FAILURE_REASON_KEYS[reason]
    rows.push({
      label: t('account.subscription.events.detail.reason'),
      value: reasonKey === undefined ? reason : t(reasonKey),
    })
  }
  const failures = asNumber(detail.failures)
  if (failures !== undefined) {
    rows.push({
      label: t('account.subscription.events.detail.failures'),
      value: String(failures),
    })
  }
  if (typeof detail.immediate === 'boolean') {
    rows.push({
      label: t('account.subscription.events.detail.effective'),
      value: detail.immediate
        ? t('account.subscription.events.detail.immediately')
        : t('account.subscription.events.detail.at_period_end'),
    })
  }
  if (detail.auto_renew_disabled === true) {
    rows.push({
      label: t('account.subscription.events.detail.auto_renew'),
      value: t('account.subscription.events.detail.auto_renew_off'),
    })
  }

  if (rows.length === 0) return null
  return (
    <dl className="mt-1 flex flex-col gap-0.5 text-xs">
      {rows.map((row) => (
        <DetailRow key={`${row.label}:${row.value}`} label={row.label} value={row.value} />
      ))}
    </dl>
  )
}

export interface SubscriptionEventsDialogProps {
  /** `null` closes the dialog. */
  subscription: UserSubscription | null
  plans: readonly AvailableSubscriptionPlan[]
  onOpenChange: (open: boolean) => void
}

/**
 * 订阅事件流（S1/R-19；`GET /api/subscriptions/{id}/events?after_seq=`）。
 *
 * 只读时间线：事件只增不减，按 seq 升序；单次最多 500 条，用本页最大 seq
 * 作为下一页游标（服务端保证 seq 从 1 连续无洞）。这里不做本地排序或去重——
 * 事件顺序是服务端契约的一部分，前端擅自重排会掩盖写入侧的乱序缺陷。
 */
export function SubscriptionEventsDialog({
  subscription,
  plans,
  onOpenChange,
}: SubscriptionEventsDialogProps) {
  const { t, locale } = useI18n()
  const subscriptionId = subscription?.id ?? null

  const query = useInfiniteQuery({
    queryKey: [...EVENTS_KEY, subscriptionId],
    queryFn: ({ pageParam }) =>
      listSubscriptionEvents(subscriptionId ?? 0, pageParam),
    initialPageParam: 0,
    getNextPageParam: (lastPage) => {
      // 只有「满一页」才可能还有后续；游标是最大 seq，而不是条数。
      if (lastPage.events.length < EVENT_PAGE_LIMIT) return undefined
      return lastPage.events[lastPage.events.length - 1]?.seq
    },
    enabled: subscriptionId !== null,
    // 事件流在弹窗内按需读取：每次打开都重新拉取，避免把上一次实例的
    // 终态缓存当成当前状态（订阅恰好在此期间被管理员取消/退款）。
    staleTime: 0,
  })

  if (subscription === null) return null

  const events = query.data?.pages.flatMap((page) => page.events) ?? []

  return (
    <Dialog
      open
      onOpenChange={onOpenChange}
      title={t('account.subscription.events.title')}
      description={t('account.subscription.events.description', {
        plan: subscription.plan_title_snapshot,
      })}
      panelClassName="max-w-[560px]"
      footer={
        <Button variant="ghost" onClick={() => onOpenChange(false)}>
          {t('account.subscription.events.close')}
        </Button>
      }
    >
      {query.isPending ? (
        <p role="status" aria-busy="true" className="text-sm text-ink-2">
          {t('account.subscription.events.loading')}
        </p>
      ) : query.isError ? (
        <div className="flex items-center gap-3 text-sm text-ink-2">
          <span role="alert">{t('account.subscription.events.error')}</span>
          <Button variant="secondary" onClick={() => void query.refetch()}>
            {t('account.subscription.events.retry')}
          </Button>
        </div>
      ) : events.length === 0 ? (
        <p className="text-sm text-ink-2">
          {t('account.subscription.events.empty')}
        </p>
      ) : (
        <div className="flex flex-col gap-3">
          <ol
            aria-label={t('account.subscription.events.timeline_label')}
            className="flex flex-col gap-3"
          >
            {events.map((event) => {
              const kindKey = EVENT_KIND_KEYS[event.kind]
              return (
                <li
                  key={event.seq}
                  className="border-l-2 border-border-hairline pl-3"
                >
                  <div className="flex flex-wrap items-baseline justify-between gap-2">
                    <span className="text-sm font-medium text-ink-1">
                      {kindKey === undefined ? event.kind : t(kindKey)}
                    </span>
                    <time
                      dateTime={event.created_at}
                      className="text-xs tabular-nums text-ink-3"
                    >
                      {formatSubscriptionTime(event.created_at, locale)}
                    </time>
                  </div>
                  <EventDetail event={event} plans={plans} />
                </li>
              )
            })}
          </ol>
          {query.hasNextPage ? (
            <Button
              variant="secondary"
              disabled={query.isFetchingNextPage}
              onClick={() => void query.fetchNextPage()}
            >
              {query.isFetchingNextPage
                ? t('account.subscription.events.loading_more')
                : t('account.subscription.events.load_more')}
            </Button>
          ) : null}
        </div>
      )}
    </Dialog>
  )
}
