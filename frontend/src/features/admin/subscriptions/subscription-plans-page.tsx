import { useEffect, useState } from 'react'
import {
  keepPreviousData,
  useMutation,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components, operations } from '@/api/schema'
import { formatCredits } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'
import { meQueryOptions } from '@/lib/session'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert, TextField } from '@/shared/ui/form'
import { StatusBadge } from '@/shared/ui/status-badge'
import { useToastStore } from '@/shared/ui/toast'

import { formatDateTime, formatPlanPrice } from './format'
import { SubscriptionPlanDrawer } from './subscription-plan-drawer'
import { useOnlineStatus } from './use-online'

type SubscriptionPlan = components['schemas']['SubscriptionPlan']
type PlanStatus = SubscriptionPlan['status']
type ListQuery = NonNullable<operations['listSubscriptionPlans']['parameters']['query']>

interface Filters {
  keyword: string
  status: '' | PlanStatus
}

const EMPTY_FILTERS: Filters = { keyword: '', status: '' }
const PAGE_SIZES = [10, 20, 50] as const

interface EditTarget {
  mode: 'create' | 'edit'
  plan?: SubscriptionPlan
}

/**
 * 管理侧订阅套餐页（billing-08 §16.2 F-H-01/02）。
 *
 * 路由登记为 `/_console/admin/subscriptions`（`/admin/subscriptions`），
 * 与原文 `/subscriptions` 的偏差见交付说明：管理页统一挂在 `_console/admin`
 * 角色守卫之下，避免与用户侧 `/wallet` 的订阅区混淆。
 *
 * 六态：
 *   - loading：骨架行；
 *   - empty：无套餐（区分「无筛选」与「筛选无结果」）；
 *   - error：行内错误 + 重试；429 时显示限流文案与 Retry-After；
 *   - 403：页面再次校验 `settings:manage`（路由 beforeLoad 已拦直链）；
 *   - 429：读列表行内提示，写操作在抽屉/对话框内行内提示；
 *   - offline：全部写入口禁用并显示提示，读请求保持可用。
 */
export function SubscriptionPlansPage() {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const offline = !useOnlineStatus()

  const meQuery = useQuery(meQueryOptions())
  // 角色层由 `_console/admin` 守卫；这里补能力层。`["*"]` 代表全量管理员。
  const canManage =
    meQuery.data?.permissions?.some(
      (permission) => permission === '*' || permission === 'settings:manage',
    ) ?? false

  const [draft, setDraft] = useState<Filters>(EMPTY_FILTERS)
  const [applied, setApplied] = useState<Filters>(EMPTY_FILTERS)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState<number>(20)
  const [editTarget, setEditTarget] = useState<EditTarget | null>(null)
  const [resetTarget, setResetTarget] = useState<SubscriptionPlan | null>(null)
  const [advanceReset, setAdvanceReset] = useState(false)
  const [resetError, setResetError] = useState<string | undefined>(undefined)
  const [statusTarget, setStatusTarget] = useState<SubscriptionPlan | null>(null)
  const [statusError, setStatusError] = useState<string | undefined>(undefined)
  const [deleteTarget, setDeleteTarget] = useState<SubscriptionPlan | null>(null)
  const [deleteError, setDeleteError] = useState<string | undefined>(undefined)

  const plansQuery = useQuery({
    queryKey: ['admin-subscription-plans', applied, page, pageSize],
    enabled: canManage,
    placeholderData: keepPreviousData,
    queryFn: async () => {
      const query: ListQuery = { p: page, page_size: pageSize }
      if (applied.keyword.trim() !== '') query.keyword = applied.keyword.trim()
      if (applied.status !== '') query.status = applied.status
      return requireData(
        await apiClient.GET('/api/subscription-plans', { params: { query } }),
      )
    },
  })

  const items = plansQuery.data?.items ?? []
  const total = plansQuery.data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / pageSize))
  const hasFilters = applied.keyword.trim() !== '' || applied.status !== ''

  useEffect(() => {
    // total=0 也要回退到第 1 页：删除最后一行后页码不能停在越界值。
    if (!plansQuery.isFetching && page > pages) setPage(pages)
  }, [plansQuery.isFetching, page, pages])

  function actionErrorText(error: unknown): string {
    if (error instanceof ApiError) {
      if (error.kind === 'rate_limited') {
        return error.retry_after === undefined
          ? t('admin.subscriptions.rate_limited_generic')
          : t('admin.subscriptions.rate_limited', { seconds: error.retry_after })
      }
      if (error.kind === 'offline') return t('admin.subscriptions.offline_notice')
      if (error.kind === 'conflict') return t('admin.subscriptions.action_conflict')
      if (error.kind === 'not_found') return t('admin.subscriptions.action_gone')
      if (error.kind === 'forbidden') return t('admin.subscriptions.load_forbidden')
    }
    return t('admin.subscriptions.action_error')
  }

  const setStatus = useMutation({
    mutationFn: async ({
      plan,
      status,
    }: {
      plan: SubscriptionPlan
      status: PlanStatus
    }) => {
      if (status === 'enabled') {
        await apiClient.POST('/api/subscription-plans/{id}/enable', {
          params: { path: { id: plan.id } },
        })
      } else {
        await apiClient.POST('/api/subscription-plans/{id}/disable', {
          params: { path: { id: plan.id } },
        })
      }
    },
    onSuccess: async (_data, variables) => {
      setStatusError(undefined)
      push({
        tone: 'success',
        message:
          variables.status === 'enabled'
            ? t('admin.subscriptions.enabled_success')
            : t('admin.subscriptions.disabled_success'),
      })
      setStatusTarget(null)
      await queryClient.invalidateQueries({ queryKey: ['admin-subscription-plans'] })
    },
    onError: (error) => {
      // Keep the confirm dialog open so the failure stays next to the action
      // that caused it; closing it would drop the operator back to a page
      // where the only feedback is a transient banner.
      setStatusError(actionErrorText(error))
      if (
        error instanceof ApiError &&
        (error.kind === 'conflict' || error.kind === 'not_found')
      ) {
        void queryClient.invalidateQueries({ queryKey: ['admin-subscription-plans'] })
      }
    },
  })

  const resetQuota = useMutation({
    mutationFn: async ({
      plan,
      advance,
    }: {
      plan: SubscriptionPlan
      advance: boolean
    }) =>
      requireData(
        await apiClient.POST('/api/subscription-plans/{id}/reset-quota', {
          params: { path: { id: plan.id } },
          body: { advance_next_reset: advance },
        }),
      ),
    onSuccess: async (result) => {
      setResetError(undefined)
      push({
        tone: 'success',
        message: t('admin.subscriptions.reset_success', { count: result.affected }),
      })
      setResetTarget(null)
      await queryClient.invalidateQueries({ queryKey: ['admin-subscription-plans'] })
    },
    onError: (error) => {
      setResetError(actionErrorText(error))
    },
  })

  const remove = useMutation({
    mutationFn: async (plan: SubscriptionPlan) => {
      await apiClient.DELETE('/api/subscription-plans/{id}', {
        params: { path: { id: plan.id } },
      })
    },
    onSuccess: async () => {
      setDeleteError(undefined)
      push({ tone: 'success', message: t('admin.subscriptions.delete_success') })
      setDeleteTarget(null)
      await queryClient.invalidateQueries({ queryKey: ['admin-subscription-plans'] })
    },
    onError: (error) => {
      if (error instanceof ApiError && error.kind === 'conflict') {
        // `reference_count` is instances + unclosed orders (backend
        // ErrPlanReferenced); `order_reference_count` is the order part, so
        // the instance count must be the difference. Only show the precise
        // split when both members arrived — otherwise say "referenced".
        const total = error.reference_count
        const orders = error.order_reference_count
        setDeleteError(
          total !== undefined && orders !== undefined && total >= orders
            ? t('admin.subscriptions.delete_referenced', {
                subscriptions: total - orders,
                orders,
              })
            : t('admin.subscriptions.delete_referenced_generic'),
        )
        void queryClient.invalidateQueries({ queryKey: ['admin-subscription-plans'] })
        return
      }
      setDeleteError(actionErrorText(error))
    },
  })

  if (meQuery.isPending) {
    return <PageSkeleton title={t('admin.subscriptions.title')} />
  }

  if (meQuery.isError) {
    return (
      <div className="mx-auto flex max-w-[1520px] flex-col gap-3">
        <h1 className="text-xl font-semibold text-ink-1">
          {t('admin.subscriptions.title')}
        </h1>
        <section className="rounded-xl border border-border-hairline bg-surface-card p-5">
          <div className="flex items-center gap-3 text-sm text-ink-2">
            <span>{t('admin.subscriptions.error')}</span>
            <Button variant="secondary" onClick={() => void meQuery.refetch()}>
              {t('common.action.retry')}
            </Button>
          </div>
        </section>
      </div>
    )
  }

  if (!canManage) {
    return (
      <div className="mx-auto max-w-[1520px] rounded-xl border border-border-hairline bg-surface-card p-8">
        <h1 className="text-xl font-semibold text-ink-1">
          {t('common.error403.title')}
        </h1>
        <p className="mt-2 text-sm text-ink-2">{t('admin.subscriptions.load_forbidden')}</p>
      </div>
    )
  }

  const writeDisabled = offline || setStatus.isPending || remove.isPending

  return (
    <div className="mx-auto flex max-w-[1520px] flex-col gap-3">
      <div className="flex flex-wrap items-end justify-between gap-2">
        <div>
          <h1 className="text-xl font-semibold text-ink-1">
            {t('admin.subscriptions.title')}
          </h1>
          <p className="mt-1 text-sm text-ink-2">{t('admin.subscriptions.subtitle')}</p>
        </div>
        <Button
          disabled={writeDisabled}
          title={offline ? t('admin.subscriptions.offline_notice') : undefined}
          onClick={() => setEditTarget({ mode: 'create' })}
        >
          {t('admin.subscriptions.new_plan')}
        </Button>
      </div>

      {offline ? (
        <p
          role="status"
          className="rounded-lg border border-warning-line bg-warning-bg px-3 py-2 text-sm text-warning"
        >
          {t('admin.subscriptions.offline_notice')}
        </p>
      ) : null}

      <section className="rounded-xl border border-border-hairline bg-surface-subtle p-4">
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <TextField
            label={t('admin.subscriptions.keyword')}
            value={draft.keyword}
            maxLength={128}
            onChange={(event) => {
              setDraft((current) => ({ ...current, keyword: event.target.value }))
            }}
          />
          <div className="flex flex-col gap-1.5">
            <label
              htmlFor="subscription-filter-status"
              className="text-[13px] font-medium text-ink-1"
            >
              {t('admin.subscriptions.status')}
            </label>
            <select
              id="subscription-filter-status"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              value={draft.status}
              onChange={(event) => {
                setDraft((current) => ({
                  ...current,
                  status: event.target.value as Filters['status'],
                }))
              }}
            >
              <option value="">{t('admin.subscriptions.all')}</option>
              <option value="enabled">{t('admin.subscriptions.status_enabled')}</option>
              <option value="disabled">{t('admin.subscriptions.status_disabled')}</option>
            </select>
          </div>
        </div>
        <div className="mt-3 flex justify-end gap-2">
          <Button
            variant="secondary"
            onClick={() => {
              setDraft(EMPTY_FILTERS)
              setApplied(EMPTY_FILTERS)
              setPage(1)
            }}
          >
            {t('admin.subscriptions.reset')}
          </Button>
          <Button
            onClick={() => {
              setApplied(draft)
              setPage(1)
            }}
          >
            {t('admin.subscriptions.search')}
          </Button>
        </div>
      </section>

      <section className="rounded-xl border border-border-hairline bg-surface-card">
        <div className="flex items-center justify-between border-b border-border-hairline px-5 py-3">
          <span className="text-[13px] text-ink-2">
            {t('admin.subscriptions.total', { count: total })}
          </span>
        </div>

        {plansQuery.isPending ? (
          <div className="space-y-2 p-5" aria-busy="true">
            {[0, 1, 2, 3, 4].map((index) => (
              <div key={index} className="h-10 animate-pulse rounded bg-surface-subtle" />
            ))}
          </div>
        ) : plansQuery.isError ? (
          <div className="flex items-center gap-3 p-5 text-sm text-ink-2">
            <span>
              {plansQuery.error instanceof ApiError &&
              plansQuery.error.kind === 'offline'
                ? t('admin.subscriptions.offline_read_error')
                : plansQuery.error instanceof ApiError &&
                    plansQuery.error.kind === 'forbidden'
                  ? t('admin.subscriptions.load_forbidden')
                  : plansQuery.error instanceof ApiError &&
                      plansQuery.error.kind === 'rate_limited'
                    ? plansQuery.error.retry_after === undefined
                      ? t('admin.subscriptions.rate_limited_generic')
                      : t('admin.subscriptions.rate_limited', {
                          seconds: plansQuery.error.retry_after,
                        })
                    : t('admin.subscriptions.error')}
            </span>
            <Button variant="secondary" onClick={() => void plansQuery.refetch()}>
              {t('common.action.retry')}
            </Button>
          </div>
        ) : items.length === 0 ? (
          <div className="flex flex-col items-start gap-2 p-5">
            <p className="text-sm text-ink-2">
              {hasFilters
                ? t('admin.subscriptions.empty_filtered')
                : t('admin.subscriptions.empty')}
            </p>
            {hasFilters ? (
              <Button
                variant="secondary"
                onClick={() => {
                  setDraft(EMPTY_FILTERS)
                  setApplied(EMPTY_FILTERS)
                  setPage(1)
                }}
              >
                {t('admin.subscriptions.clear_filters')}
              </Button>
            ) : null}
          </div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[1180px] text-sm">
              <thead>
                <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.subscriptions.column_plan_key')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.subscriptions.column_title')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.subscriptions.column_period')}
                  </th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">
                    {t('admin.subscriptions.column_price')}
                  </th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">
                    {t('admin.subscriptions.column_grant_credits')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.subscriptions.column_model_scope')}
                  </th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">
                    {t('admin.subscriptions.column_grace')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.subscriptions.column_status')}
                  </th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">
                    {t('admin.subscriptions.column_sort')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.subscriptions.column_updated')}
                  </th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">
                    {t('admin.subscriptions.actions')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {items.map((plan) => (
                  <tr
                    key={plan.id}
                    className="border-b border-border-hairline last:border-0 hover:bg-surface-hover"
                  >
                    <td className="px-4 py-3 font-mono text-ink-1">{plan.plan_key}</td>
                    <td className="px-4 py-3 text-ink-1">{plan.title}</td>
                    <td className="px-4 py-3 text-ink-2">
                      {t('admin.subscriptions.period_days', { count: plan.period_days })}
                    </td>
                    <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                      {formatPlanPrice(plan.price_amount, locale)}
                    </td>
                    <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                      {plan.grant_credits === 0
                        ? t('admin.subscriptions.unlimited')
                        : formatCredits(plan.grant_credits, locale)}
                    </td>
                    <td
                      className="max-w-[220px] truncate px-4 py-3 text-ink-2"
                      title={
                        plan.model_scope === null || plan.model_scope === undefined
                          ? t('admin.subscriptions.all_models')
                          : plan.model_scope.join(', ')
                      }
                    >
                      {plan.model_scope === null || plan.model_scope === undefined
                        ? t('admin.subscriptions.all_models')
                        : plan.model_scope.length === 0
                          ? t('admin.subscriptions.no_models')
                          : plan.model_scope.join(', ')}
                    </td>
                    <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-2">
                      {plan.grace_days === 0 ? '—' : plan.grace_days}
                    </td>
                    <td className="px-4 py-3">
                      <StatusBadge
                        variant={plan.status === 'enabled' ? 'success' : 'neutral'}
                      >
                        {plan.status === 'enabled'
                          ? t('admin.subscriptions.status_enabled')
                          : t('admin.subscriptions.status_disabled')}
                      </StatusBadge>
                    </td>
                    <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-2">
                      {plan.sort_order}
                    </td>
                    <td className="px-4 py-3 text-xs text-ink-2" title={plan.updated_at}>
                      {formatDateTime(plan.updated_at, locale)}
                    </td>
                    <td className="px-4 py-3">
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="ghost"
                          className="h-8 px-2 text-[13px]"
                          disabled={offline}
                          onClick={() => setEditTarget({ mode: 'edit', plan })}
                        >
                          {t('admin.subscriptions.edit')}
                        </Button>
                        <Button
                          variant="ghost"
                          className="h-8 px-2 text-[13px]"
                          disabled={writeDisabled}
                          onClick={() => {
                            setResetError(undefined)
                            setAdvanceReset(false)
                            setResetTarget(plan)
                          }}
                        >
                          {t('admin.subscriptions.reset_quota')}
                        </Button>
                        <Button
                          variant="ghost"
                          className="h-8 px-2 text-[13px]"
                          disabled={writeDisabled}
                          onClick={() => {
                            setStatusError(undefined)
                            setStatusTarget(plan)
                          }}
                        >
                          {plan.status === 'enabled'
                            ? t('admin.subscriptions.disable')
                            : t('admin.subscriptions.enable')}
                        </Button>
                        <Button
                          variant="ghost"
                          className="h-8 px-2 text-[13px] text-danger hover:bg-danger-bg"
                          disabled={writeDisabled}
                          onClick={() => {
                            setDeleteError(undefined)
                            setDeleteTarget(plan)
                          }}
                        >
                          {t('admin.subscriptions.delete')}
                        </Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        <div className="flex items-center justify-end gap-3 border-t border-border-hairline px-5 py-3 text-[13px] text-ink-2">
          <label htmlFor="subscription-page-size" className="sr-only">
            {t('admin.subscriptions.page_size')}
          </label>
          <select
            id="subscription-page-size"
            className="h-8 rounded-md border border-border-default bg-surface-card px-2 text-xs text-ink-1"
            value={pageSize}
            onChange={(event) => {
              setPageSize(Number(event.target.value))
              setPage(1)
            }}
          >
            {PAGE_SIZES.map((size) => (
              <option key={size} value={size}>
                {size}
              </option>
            ))}
          </select>
          <Button
            variant="secondary"
            className="h-8 px-2 text-[13px]"
            disabled={page <= 1}
            onClick={() => setPage((current) => Math.max(1, current - 1))}
          >
            {t('admin.subscriptions.pagination_previous')}
          </Button>
          <span className="font-mono tabular-nums">
            {t('admin.subscriptions.pagination_page', { page, pages })}
          </span>
          <Button
            variant="secondary"
            className="h-8 px-2 text-[13px]"
            disabled={page >= pages}
            onClick={() => setPage((current) => current + 1)}
          >
            {t('admin.subscriptions.pagination_next')}
          </Button>
        </div>
      </section>

      <SubscriptionPlanDrawer
        open={editTarget !== null}
        mode={editTarget?.mode ?? 'create'}
        plan={editTarget?.plan}
        onOpenChange={(open) => {
          if (!open) setEditTarget(null)
        }}
        onSaved={() => setEditTarget(null)}
      />

      <Dialog
        open={resetTarget !== null}
        onOpenChange={(open) => {
          if (!open) setResetTarget(null)
        }}
        title={t('admin.subscriptions.reset_title')}
        description={t('admin.subscriptions.reset_body')}
        footer={
          <>
            <Button variant="secondary" onClick={() => setResetTarget(null)}>
              {t('common.action.cancel')}
            </Button>
            <Button
              disabled={resetQuota.isPending || offline}
              onClick={() => {
                if (resetTarget !== null) {
                  resetQuota.mutate({ plan: resetTarget, advance: advanceReset })
                }
              }}
            >
              {resetQuota.isPending
                ? t('admin.subscriptions.reset_pending')
                : t('admin.subscriptions.reset_quota')}
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-3">
          <label className="flex items-start gap-2 text-sm text-ink-1">
            <input
              type="checkbox"
              className="mt-0.5 size-4 accent-brand"
              checked={advanceReset}
              onChange={(event) => setAdvanceReset(event.target.checked)}
            />
            <span>
              {t('admin.subscriptions.reset_advance')}
              <span className="block text-xs text-ink-3">
                {t('admin.subscriptions.reset_advance_hint')}
              </span>
            </span>
          </label>
          {resetError !== undefined ? <FormAlert>{resetError}</FormAlert> : null}
        </div>
      </Dialog>

      <Dialog
        open={statusTarget !== null}
        onOpenChange={(open) => {
          if (!open) setStatusTarget(null)
        }}
        title={
          statusTarget?.status === 'enabled'
            ? t('admin.subscriptions.disable_title')
            : t('admin.subscriptions.enable_title')
        }
        description={
          statusTarget?.status === 'enabled'
            ? t('admin.subscriptions.disable_body')
            : t('admin.subscriptions.enable_body')
        }
        footer={
          <>
            <Button
              variant="secondary"
              onClick={() => {
                setStatusError(undefined)
                setStatusTarget(null)
              }}
            >
              {statusError !== undefined
                ? t('common.action.close')
                : t('common.action.cancel')}
            </Button>
            <Button
              disabled={setStatus.isPending || offline}
              onClick={() => {
                if (statusTarget === null) return
                setStatus.mutate({
                  plan: statusTarget,
                  status: statusTarget.status === 'enabled' ? 'disabled' : 'enabled',
                })
              }}
            >
              {statusTarget?.status === 'enabled'
                ? t('admin.subscriptions.disable')
                : t('admin.subscriptions.enable')}
            </Button>
          </>
        }
      >
        {statusError !== undefined ? <FormAlert>{statusError}</FormAlert> : null}
      </Dialog>

      <Dialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) setDeleteTarget(null)
        }}
        role="alertdialog"
        initialFocus="first"
        title={t('admin.subscriptions.delete_title')}
        description={t('admin.subscriptions.delete_body')}
        footer={
          <>
            <Button variant="secondary" onClick={() => setDeleteTarget(null)}>
              {t('common.action.cancel')}
            </Button>
            <Button
              variant="danger"
              disabled={remove.isPending || offline}
              onClick={() => {
                if (deleteTarget !== null) remove.mutate(deleteTarget)
              }}
            >
              {t('admin.subscriptions.delete')}
            </Button>
          </>
        }
      >
        {deleteError !== undefined ? <FormAlert>{deleteError}</FormAlert> : null}
      </Dialog>
    </div>
  )
}

function PageSkeleton({ title }: { title: string }) {
  return (
    <div className="mx-auto flex max-w-[1520px] flex-col gap-3">
      <h1 className="text-xl font-semibold text-ink-1">{title}</h1>
      <section className="rounded-xl border border-border-hairline bg-surface-card">
        <div className="space-y-2 p-5" aria-busy="true">
          {[0, 1, 2, 3, 4].map((index) => (
            <div key={index} className="h-10 animate-pulse rounded bg-surface-subtle" />
          ))}
        </div>
      </section>
    </div>
  )
}
