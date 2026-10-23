import { useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { ApiError } from '@/api/errors'
import { fetchSettingsSection } from '@/features/admin/settings/api'
import { meQueryOptions } from '@/lib/session'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'
import { Tag } from '@/shared/ui/tag'

import { listDeliveries, listTemplates, previewTemplate } from './api'
import { DeliveryDetailDrawer } from './delivery-detail-drawer'
import { PreviewDialog } from './preview-dialog'
import {
  NOTIFICATION_CATEGORY_KEYS,
  NOTIFICATION_CATEGORIES,
  categoryLabelKey,
  isLockedTemplate,
} from './registry'
import { RestoreAllDialog, RestoreSingleDialog } from './restore-dialogs'
import {
  categoriesFromSearch,
  categoriesToSearch,
  type NotificationTemplatesSearch,
  type NotificationTemplatesTab,
} from './search'
import { TemplateDrawer } from './template-drawer'
import { TestSendDialog } from './test-send-dialog'
import {
  DELIVERY_STATUSES,
  NOTIFICATION_LOCALES,
  isNotificationLocale,
  type DeliveryStatus,
  type DeliveryTimeRange,
  type NotificationDelivery,
  type NotificationDeliveryChannel,
  type NotificationTemplate,
  type NotificationTemplateChannel,
} from './types'
import { deliveryBounds, maskRecipient } from './utils'

const TEMPLATE_PAGE_SIZE = 20
const DELIVERY_PAGE_SIZE = 20

const DELIVERY_STATUS_VARIANTS: Record<DeliveryStatus | 'queued', StatusBadgeVariant> = {
  // queued 是 DB 口径的旧值；API 已映射为 pending，两者都按「排队中」渲染。
  queued: 'neutral',
  pending: 'neutral',
  sent: 'success',
  failed: 'danger',
  bounced: 'warning',
  suppressed: 'neutral',
}

export interface NotificationTemplatesPageProps {
  search: NotificationTemplatesSearch
  onSearchChange: (patch: Partial<NotificationTemplatesSearch>) => void
}

function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.detail ?? error.title
  return error instanceof Error ? error.message : String(error)
}

function isForbidden(error: unknown): boolean {
  return error instanceof ApiError && error.kind === 'forbidden'
}

function settingsValue(
  section: { items: { key: string; value: unknown }[] },
  key: string,
): unknown {
  return section.items.find((item) => item.key === key)?.value
}

function nonEmptyString(value: unknown): boolean {
  return typeof value === 'string' && value.trim() !== ''
}

/**
 * 通知模板管理页（modules-08 §6.9，F-08-24～F-08-27）。
 *
 * 路由只负责把 URL search 与导航传进来；筛选、分页与六态都在这里实现，
 * 便于直接渲染测试（不经 RouterProvider）。
 *
 * 分类多选是前端过滤：冻结契约的 `GET /api/notification-templates` 只接受
 * 单个 `category`（`query.Get` 会折叠重复参数），而 channel / locale /
 * `modified` 是单值，交给服务端过滤。这样最多一次读取 126 行注册表全集，
 * 计数与分页都在本地完成。
 */
export function NotificationTemplatesPage({
  search,
  onSearchChange,
}: NotificationTemplatesPageProps) {
  const { t, locale: uiLocale } = useI18n()
  const queryClient = useQueryClient()

  const me = useQuery(meQueryOptions())
  const canManage =
    me.data?.permissions.some(
      (permission) => permission === '*' || permission === 'settings:manage',
    ) ?? false

  // 测试发送依赖 SMTP：host 与 from_email 都非空才算配置（F-08-19 口径）。
  // 读取失败时保持 null（不禁用），由服务端 422 兜底，绝不臆断「未配置」。
  const smtp = useQuery({
    queryKey: ['settings-section', 'notify.smtp'],
    queryFn: ({ signal }) => fetchSettingsSection('notify.smtp', signal),
    enabled: canManage,
  })
  const smtpConfigured =
    smtp.data === undefined
      ? null
      : nonEmptyString(settingsValue(smtp.data, 'notify.smtp.host')) &&
        nonEmptyString(settingsValue(smtp.data, 'notify.smtp.from_email'))

  const tab: NotificationTemplatesTab = search.tab === 'deliveries' ? 'deliveries' : 'templates'
  const categories = useMemo(
    () => categoriesFromSearch(search.category),
    [search.category],
  )

  const [drawerTarget, setDrawerTarget] = useState<NotificationTemplate | null>(null)
  const [previewTarget, setPreviewTarget] = useState<NotificationTemplate | null>(null)
  const [testTarget, setTestTarget] = useState<NotificationTemplate | null>(null)
  const [restoreTarget, setRestoreTarget] = useState<NotificationTemplate | null>(null)
  const [restoreAllOpen, setRestoreAllOpen] = useState(false)
  const [deliveryTarget, setDeliveryTarget] = useState<NotificationDelivery | null>(null)

  const templateQuery = useQuery({
    queryKey: ['notification-templates', search.channel, search.locale, search.modified === true],
    queryFn: ({ signal }) =>
      listTemplates(
        {
          channel: search.channel,
          locale: search.locale,
          modifiedOnly: search.modified === true,
        },
        signal,
      ),
    enabled: tab === 'templates',
  })

  const filteredRows = useMemo(() => {
    const items = templateQuery.data?.items ?? []
    if (categories.length === 0) return items
    const selected = new Set(categories)
    return items.filter((item) => selected.has(item.category))
  }, [templateQuery.data, categories])

  const pageCount = Math.max(1, Math.ceil(filteredRows.length / TEMPLATE_PAGE_SIZE))
  const requestedPage = search.page ?? 1
  const page = Math.min(requestedPage, pageCount)
  const pageRows = filteredRows.slice(
    (page - 1) * TEMPLATE_PAGE_SIZE,
    page * TEMPLATE_PAGE_SIZE,
  )
  const modifiedCount = filteredRows.filter((row) => row.customized).length

  // 时间范围在客户端换算成 RFC3339；自定义范围非法或超过 90 天时阻断查询，
  // 与设计「时间跨度 >90 天 → 阻断并提示缩小范围」一致。
  const deliveryWindow = useMemo(
    () => deliveryBounds(search.d_range, search.d_from, search.d_to),
    [search.d_range, search.d_from, search.d_to],
  )
  const deliveryRangeError = deliveryWindow.error

  const deliveryQuery = useQuery({
    queryKey: [
      'notification-deliveries',
      search.d_category,
      search.d_channel,
      search.d_status,
      search.d_range,
      search.d_from,
      search.d_to,
      search.d_page,
    ],
    queryFn: ({ signal }) =>
      listDeliveries(
        {
          page: search.d_page ?? 1,
          pageSize: DELIVERY_PAGE_SIZE,
          category: search.d_category,
          channel: search.d_channel,
          status: search.d_status,
          from: deliveryWindow.from,
          to: deliveryWindow.to,
        },
        signal,
      ),
    enabled: tab === 'deliveries' && deliveryRangeError === undefined,
  })

  const previewQuery = useQuery({
    queryKey: [
      'notification-template-preview',
      previewTarget?.key,
      previewTarget?.channel,
      previewTarget?.locale,
    ],
    queryFn: ({ signal }) =>
      previewTemplate(
        previewTarget?.key ?? '',
        {
          locale: previewTarget?.locale ?? 'en',
          channel: previewTarget?.channel ?? 'email',
          variables: {},
        },
        signal,
      ),
    enabled: previewTarget !== null,
  })

  function refreshAll(): void {
    if (tab === 'templates') {
      void templateQuery.refetch()
    } else {
      void deliveryQuery.refetch()
    }
  }

  function switchTab(next: NotificationTemplatesTab): void {
    if (next === tab) return
    onSearchChange({ tab: next === 'templates' ? undefined : next })
  }

  function templatePageHref(nextPage: number): void {
    onSearchChange({ page: nextPage <= 1 ? undefined : nextPage })
  }

  const deliveries = deliveryQuery.data?.items ?? []
  const deliveryTotal = deliveryQuery.data?.total ?? 0
  const deliveryPage = search.d_page ?? 1
  const deliveryPageCount = Math.max(1, Math.ceil(deliveryTotal / DELIVERY_PAGE_SIZE))

  return (
    <div className="mx-auto flex max-w-[1520px] flex-col gap-4" data-testid="notification-templates-page">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold text-ink-1">
            {t('settings.notification_templates.page.title')}
          </h1>
          <p className="mt-1 text-sm text-ink-2">
            {t('settings.notification_templates.page.subtitle')}
          </p>
        </div>
        <Button variant="secondary" onClick={refreshAll}>
          {t('settings.notification_templates.page.refresh')}
        </Button>
      </header>

      {!canManage && !me.isPending ? (
        <FormAlert tone="info">
          {t('settings.notification_templates.page.read_only')}
        </FormAlert>
      ) : null}

      <div
        role="tablist"
        aria-label={t('settings.notification_templates.tabs.label')}
        className="flex gap-1 border-b border-border-hairline"
      >
        {(['templates', 'deliveries'] as const).map((entry) => {
          const selected = entry === tab
          return (
            <button
              key={entry}
              type="button"
              role="tab"
              aria-selected={selected}
              onClick={() => switchTab(entry)}
              className={[
                'h-10 rounded-t-lg px-3 text-[13px]',
                selected
                  ? 'border-b-2 border-brand font-medium text-ink-1'
                  : 'text-ink-2 hover:bg-surface-hover hover:text-ink-1',
              ].join(' ')}
            >
              {t(`settings.notification_templates.tabs.${entry}`)}
            </button>
          )
        })}
      </div>

      {tab === 'templates' ? (
        <section className="space-y-3" aria-label={t('settings.notification_templates.tabs.templates')}>
          <div className="flex flex-wrap items-end gap-3">
            <label className="flex flex-col gap-1 text-xs text-ink-2">
              {t('settings.notification_templates.filters.category')}
              <select
                multiple
                size={4}
                aria-label={t('settings.notification_templates.filters.category')}
                value={categories}
                onChange={(event) => {
                  const next = Array.from(event.target.selectedOptions, (option) => option.value)
                  onSearchChange({
                    category: categoriesToSearch(next),
                    // 筛选变化必须回到第 1 页，否则会落在不存在的页码上。
                    page: undefined,
                  })
                }}
                className="min-w-44 rounded-lg border border-border-default bg-surface-card px-2 py-1 text-sm text-ink-1"
              >
                {NOTIFICATION_CATEGORIES.map((category) => (
                  <option key={category.key} value={category.key}>
                    {t(categoryLabelKey(category.key))}
                  </option>
                ))}
              </select>
            </label>

            <label className="flex flex-col gap-1 text-xs text-ink-2">
              {t('settings.notification_templates.filters.channel')}
              <select
                aria-label={t('settings.notification_templates.filters.channel')}
                value={search.channel ?? ''}
                onChange={(event) => {
                  const value = event.target.value
                  onSearchChange({
                    channel:
                      value === 'email' || value === 'inapp'
                        ? (value as NotificationTemplateChannel)
                        : undefined,
                    page: undefined,
                  })
                }}
                className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
              >
                <option value="">{t('settings.notification_templates.filters.all_channels')}</option>
                <option value="email">{t('settings.notification_templates.channel.email')}</option>
                <option value="inapp">{t('settings.notification_templates.channel.inapp')}</option>
              </select>
            </label>

            <label className="flex flex-col gap-1 text-xs text-ink-2">
              {t('settings.notification_templates.filters.locale')}
              <select
                aria-label={t('settings.notification_templates.filters.locale')}
                value={search.locale ?? ''}
                onChange={(event) => {
                  const value = event.target.value
                  onSearchChange({
                    locale: isNotificationLocale(value) ? value : undefined,
                    page: undefined,
                  })
                }}
                className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
              >
                <option value="">{t('settings.notification_templates.filters.all_locales')}</option>
                {NOTIFICATION_LOCALES.map((entry) => (
                  <option key={entry} value={entry}>
                    {entry}
                  </option>
                ))}
              </select>
            </label>

            <label className="flex h-9 items-center gap-2 text-sm text-ink-2">
              <input
                type="checkbox"
                checked={search.modified === true}
                onChange={(event) => {
                  onSearchChange({
                    modified: event.target.checked ? true : undefined,
                    page: undefined,
                  })
                }}
              />
              {t('settings.notification_templates.filters.modified_only')}
            </label>
          </div>

          <div className="flex flex-wrap items-center justify-between gap-2">
            <p className="text-xs text-ink-3" role="status">
              {t('settings.notification_templates.toolbar.rows', {
                count: filteredRows.length,
              })}
              {' · '}
              {t('settings.notification_templates.toolbar.modified_rows', {
                count: modifiedCount,
              })}
            </p>
            {canManage ? (
              <Button
                variant="danger"
                className="h-8 px-3 text-xs"
                disabled={filteredRows.length === 0}
                onClick={() => setRestoreAllOpen(true)}
              >
                {t('settings.notification_templates.toolbar.restore_all')}
              </Button>
            ) : null}
          </div>

          {templateQuery.isPending ? (
            <div className="space-y-2" aria-hidden="true">
              {Array.from({ length: 8 }, (_, index) => (
                <div key={index} className="h-12 rounded-lg bg-surface-subtle" />
              ))}
            </div>
          ) : null}

          {templateQuery.isError ? (
            <div className="space-y-3">
              <FormAlert tone="danger">
                {isForbidden(templateQuery.error)
                  ? t('settings.notification_templates.page.forbidden')
                  : `${t('settings.notification_templates.page.load_error')} ${errorMessage(templateQuery.error)}`}
              </FormAlert>
              <Button variant="secondary" onClick={() => void templateQuery.refetch()}>
                {t('settings.notification_templates.page.retry')}
              </Button>
            </div>
          ) : null}

          {templateQuery.isSuccess ? (
            filteredRows.length === 0 ? (
              <p className="rounded-xl border border-dashed border-border-default px-4 py-10 text-center text-sm text-ink-3">
                {categories.length === 0 &&
                search.channel === undefined &&
                search.locale === undefined &&
                search.modified !== true
                  ? t('settings.notification_templates.table.not_seeded')
                  : t('settings.notification_templates.table.empty')}
              </p>
            ) : (
              <>
                <div className="overflow-x-auto rounded-xl border border-border-default">
                  <table className="w-full text-left text-sm">
                    <thead className="bg-surface-subtle text-xs text-ink-3">
                      <tr>
                        <th className="px-3 py-2 font-medium">
                          {t('settings.notification_templates.table.key')}
                        </th>
                        <th className="px-3 py-2 font-medium">
                          {t('settings.notification_templates.table.category')}
                        </th>
                        <th className="px-3 py-2 font-medium">
                          {t('settings.notification_templates.table.channel')}
                        </th>
                        <th className="px-3 py-2 font-medium">
                          {t('settings.notification_templates.table.locale')}
                        </th>
                        <th className="px-3 py-2 font-medium">
                          {t('settings.notification_templates.table.enabled')}
                        </th>
                        <th className="px-3 py-2 font-medium">
                          {t('settings.notification_templates.table.modified')}
                        </th>
                        <th className="hidden px-3 py-2 font-medium lg:table-cell">
                          {t('settings.notification_templates.table.updated_at')}
                        </th>
                        <th className="px-3 py-2 font-medium">
                          {t('settings.notification_templates.table.actions')}
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {pageRows.map((row) => {
                        const locked = isLockedTemplate(row.key)
                        return (
                          <tr
                            key={`${row.key}|${row.channel}|${row.locale}`}
                            data-testid="notification-template-row"
                            className="border-t border-border-hairline align-middle"
                          >
                            <td className="px-3 py-2 font-mono text-xs text-ink-1">{row.key}</td>
                            <td className="px-3 py-2">
                              <Tag variant="neutral">{t(categoryLabelKey(row.category))}</Tag>
                            </td>
                            <td className="px-3 py-2">
                              <Tag variant="neutral">
                                {t(`settings.notification_templates.channel.${row.channel}`)}
                              </Tag>
                            </td>
                            <td className="px-3 py-2 text-xs text-ink-2">{row.locale}</td>
                            <td className="px-3 py-2">
                              {locked ? (
                                <span className="text-xs text-ink-2">
                                  {row.enabled
                                    ? t('settings.notification_templates.drawer.locked_enabled')
                                    : t('settings.notification_templates.table.disabled')}
                                </span>
                              ) : row.enabled ? (
                                <StatusBadge variant="success">
                                  {t('settings.notification_templates.table.enabled_value')}
                                </StatusBadge>
                              ) : (
                                <StatusBadge variant="neutral">
                                  {t('settings.notification_templates.table.disabled')}
                                </StatusBadge>
                              )}
                            </td>
                            <td className="px-3 py-2">
                              {row.customized ? (
                                <Tag variant="warning">
                                  {t('settings.notification_templates.table.modified_value')}
                                </Tag>
                              ) : (
                                <Tag variant="neutral">
                                  {t('settings.notification_templates.table.default')}
                                </Tag>
                              )}
                            </td>
                            <td className="hidden px-3 py-2 text-xs text-ink-3 lg:table-cell">
                              {row.updated_at === null ? (
                                '—'
                              ) : (
                                <span title={row.updated_at}>
                                  {formatRelativeTime(row.updated_at, uiLocale)}
                                </span>
                              )}
                            </td>
                            <td className="px-3 py-2">
                              <div className="flex flex-wrap gap-1">
                                <Button
                                  variant="ghost"
                                  className="h-7 px-2 text-xs"
                                  onClick={() => setDrawerTarget(row)}
                                >
                                  {t('settings.notification_templates.table.edit')}
                                </Button>
                                <Button
                                  variant="ghost"
                                  className="h-7 px-2 text-xs"
                                  onClick={() => setPreviewTarget(row)}
                                >
                                  {t('settings.notification_templates.table.preview')}
                                </Button>
                                {canManage ? (
                                  <Button
                                    variant="ghost"
                                    className="h-7 px-2 text-xs"
                                    disabled={
                                      row.channel !== 'email' || smtpConfigured === false
                                    }
                                    title={
                                      row.channel !== 'email'
                                        ? t('settings.notification_templates.test.email_only')
                                        : smtpConfigured === false
                                          ? t('settings.notification_templates.test.smtp_missing')
                                          : undefined
                                    }
                                    onClick={() => setTestTarget(row)}
                                  >
                                    {t('settings.notification_templates.table.test')}
                                  </Button>
                                ) : null}
                                {canManage ? (
                                  <Button
                                    variant="ghost"
                                    className="h-7 px-2 text-xs text-danger hover:text-danger"
                                    onClick={() => setRestoreTarget(row)}
                                  >
                                    {t('settings.notification_templates.table.restore')}
                                  </Button>
                                ) : null}
                              </div>
                            </td>
                          </tr>
                        )
                      })}
                    </tbody>
                  </table>
                </div>

                <nav
                  aria-label={t('settings.notification_templates.pagination.label')}
                  className="flex items-center justify-between text-xs text-ink-3"
                >
                  <span>
                    {t('settings.notification_templates.pagination.summary', {
                      from: (page - 1) * TEMPLATE_PAGE_SIZE + 1,
                      to: Math.min(page * TEMPLATE_PAGE_SIZE, filteredRows.length),
                      total: filteredRows.length,
                    })}
                  </span>
                  <span className="flex items-center gap-2">
                    <Button
                      variant="secondary"
                      className="h-7 px-2 text-xs"
                      disabled={page <= 1}
                      onClick={() => templatePageHref(page - 1)}
                    >
                      {t('settings.notification_templates.pagination.previous')}
                    </Button>
                    <span>
                      {t('settings.notification_templates.pagination.page', {
                        page,
                        pages: pageCount,
                      })}
                    </span>
                    <Button
                      variant="secondary"
                      className="h-7 px-2 text-xs"
                      disabled={page >= pageCount}
                      onClick={() => templatePageHref(page + 1)}
                    >
                      {t('settings.notification_templates.pagination.next')}
                    </Button>
                  </span>
                </nav>
              </>
            )
          ) : null}
        </section>
      ) : (
        <section className="space-y-3" aria-label={t('settings.notification_templates.tabs.deliveries')}>
          <div className="flex flex-wrap items-end gap-3">
            <label className="flex flex-col gap-1 text-xs text-ink-2">
              {t('settings.notification_templates.filters.category')}
              <select
                aria-label={t('settings.notification_templates.filters.category')}
                value={search.d_category ?? ''}
                onChange={(event) => {
                  const value = event.target.value
                  onSearchChange({
                    d_category: NOTIFICATION_CATEGORY_KEYS.includes(value)
                      ? value
                      : undefined,
                    d_page: undefined,
                  })
                }}
                className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
              >
                <option value="">{t('settings.notification_templates.filters.all_categories')}</option>
                {NOTIFICATION_CATEGORIES.map((category) => (
                  <option key={category.key} value={category.key}>
                    {t(categoryLabelKey(category.key))}
                  </option>
                ))}
              </select>
            </label>

            <label className="flex flex-col gap-1 text-xs text-ink-2">
              {t('settings.notification_templates.filters.channel')}
              <select
                aria-label={t('settings.notification_templates.filters.channel')}
                value={search.d_channel ?? ''}
                onChange={(event) => {
                  const value = event.target.value
                  onSearchChange({
                    d_channel:
                      value === 'email' || value === 'webhook'
                        ? (value as NotificationDeliveryChannel)
                        : undefined,
                    d_page: undefined,
                  })
                }}
                className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
              >
                <option value="">{t('settings.notification_templates.filters.all_channels')}</option>
                <option value="email">{t('settings.notification_templates.channel.email')}</option>
                <option value="webhook">{t('settings.notification_templates.channel.webhook')}</option>
              </select>
            </label>

            <label className="flex flex-col gap-1 text-xs text-ink-2">
              {t('settings.notification_templates.deliveries.status')}
              <select
                aria-label={t('settings.notification_templates.deliveries.status')}
                value={search.d_status ?? ''}
                onChange={(event) => {
                  const value = event.target.value
                  onSearchChange({
                    d_status: DELIVERY_STATUSES.includes(value as DeliveryStatus)
                      ? (value as DeliveryStatus)
                      : undefined,
                    d_page: undefined,
                  })
                }}
                className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
              >
                <option value="">{t('settings.notification_templates.deliveries.all_statuses')}</option>
                {DELIVERY_STATUSES.map((entry) => (
                  <option key={entry} value={entry}>
                    {t(`settings.notification_templates.deliveries.status_${entry}`)}
                  </option>
                ))}
              </select>
            </label>

            <label className="flex flex-col gap-1 text-xs text-ink-2">
              {t('settings.notification_templates.deliveries.time_range')}
              <select
                aria-label={t('settings.notification_templates.deliveries.time_range')}
                value={search.d_range ?? ''}
                onChange={(event) => {
                  const value = event.target.value
                  const range: DeliveryTimeRange | undefined =
                    value === '24h' || value === '7d' || value === '30d' || value === 'custom'
                      ? (value as DeliveryTimeRange)
                      : undefined
                  onSearchChange({
                    d_range: range,
                    // 离开自定义模式时清掉日期，避免 URL 留下不可见筛选。
                    d_from: range === 'custom' ? search.d_from : undefined,
                    d_to: range === 'custom' ? search.d_to : undefined,
                    d_page: undefined,
                  })
                }}
                className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
              >
                <option value="">{t('settings.notification_templates.deliveries.range_all')}</option>
                <option value="24h">{t('settings.notification_templates.deliveries.range_24h')}</option>
                <option value="7d">{t('settings.notification_templates.deliveries.range_7d')}</option>
                <option value="30d">{t('settings.notification_templates.deliveries.range_30d')}</option>
                <option value="custom">{t('settings.notification_templates.deliveries.range_custom')}</option>
              </select>
            </label>

            {search.d_range === 'custom' ? (
              <>
                <label className="flex flex-col gap-1 text-xs text-ink-2">
                  {t('settings.notification_templates.deliveries.from')}
                  <input
                    type="date"
                    aria-label={t('settings.notification_templates.deliveries.from')}
                    value={search.d_from ?? ''}
                    onChange={(event) =>
                      onSearchChange({ d_from: event.target.value || undefined, d_page: undefined })
                    }
                    className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
                  />
                </label>
                <label className="flex flex-col gap-1 text-xs text-ink-2">
                  {t('settings.notification_templates.deliveries.to')}
                  <input
                    type="date"
                    aria-label={t('settings.notification_templates.deliveries.to')}
                    value={search.d_to ?? ''}
                    onChange={(event) =>
                      onSearchChange({ d_to: event.target.value || undefined, d_page: undefined })
                    }
                    className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
                  />
                </label>
              </>
            ) : null}
          </div>

          {deliveryRangeError !== undefined ? (
            <FormAlert tone="danger">
              {deliveryRangeError === 'too_large'
                ? t('settings.notification_templates.deliveries.time_range_too_large')
                : t('settings.notification_templates.deliveries.time_range_invalid')}
            </FormAlert>
          ) : null}

          {deliveryQuery.isPending && deliveryRangeError === undefined ? (
            <div className="space-y-2" aria-hidden="true">
              {Array.from({ length: 5 }, (_, index) => (
                <div key={index} className="h-12 rounded-lg bg-surface-subtle" />
              ))}
            </div>
          ) : null}

          {deliveryQuery.isError ? (
            <div className="space-y-3">
              <FormAlert tone="danger">
                {isForbidden(deliveryQuery.error)
                  ? t('settings.notification_templates.page.forbidden')
                  : `${t('settings.notification_templates.deliveries.load_error')} ${errorMessage(deliveryQuery.error)}`}
              </FormAlert>
              <Button variant="secondary" onClick={() => void deliveryQuery.refetch()}>
                {t('settings.notification_templates.page.retry')}
              </Button>
            </div>
          ) : null}

          {deliveryQuery.isSuccess ? (
            deliveries.length === 0 ? (
              <p className="rounded-xl border border-dashed border-border-default px-4 py-10 text-center text-sm text-ink-3">
                {t('settings.notification_templates.deliveries.empty')}
              </p>
            ) : (
              <>
                <div className="overflow-x-auto rounded-xl border border-border-default">
                  <table className="w-full text-left text-sm">
                    <thead className="bg-surface-subtle text-xs text-ink-3">
                      <tr>
                        <th className="px-3 py-2 font-medium">
                          {t('settings.notification_templates.deliveries.time')}
                        </th>
                        <th className="px-3 py-2 font-medium">
                          {t('settings.notification_templates.table.category')}
                        </th>
                        <th className="px-3 py-2 font-medium">
                          {t('settings.notification_templates.table.channel')}
                        </th>
                        <th className="px-3 py-2 font-medium">
                          {t('settings.notification_templates.deliveries.recipient')}
                        </th>
                        <th className="px-3 py-2 font-medium">
                          {t('settings.notification_templates.table.locale')}
                        </th>
                        <th className="px-3 py-2 font-medium">
                          {t('settings.notification_templates.deliveries.status')}
                        </th>
                        <th className="px-3 py-2 font-medium">
                          {t('settings.notification_templates.deliveries.attempt')}
                        </th>
                        <th className="px-3 py-2 font-medium">
                          {t('settings.notification_templates.deliveries.error')}
                        </th>
                        <th className="px-3 py-2 font-medium">
                          {t('settings.notification_templates.table.actions')}
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {deliveries.map((delivery: NotificationDelivery) => (
                        <tr
                          key={delivery.id}
                          data-testid="notification-delivery-row"
                          className="border-t border-border-hairline align-top"
                        >
                          <td className="whitespace-nowrap px-3 py-2 text-xs text-ink-3">
                            <span title={delivery.created_at}>
                              {formatRelativeTime(delivery.created_at, uiLocale)}
                            </span>
                          </td>
                          <td className="px-3 py-2">
                            <Tag variant="neutral">{t(categoryLabelKey(delivery.category))}</Tag>
                          </td>
                          <td className="px-3 py-2">
                            <Tag variant="neutral">
                              {t(`settings.notification_templates.channel.${delivery.channel}`)}
                            </Tag>
                          </td>
                          <td className="px-3 py-2 text-xs text-ink-2">
                            {maskRecipient(delivery.recipient)}
                          </td>
                          <td className="px-3 py-2 text-xs text-ink-2">{delivery.locale ?? '—'}</td>
                          <td className="px-3 py-2">
                            <StatusBadge variant={DELIVERY_STATUS_VARIANTS[delivery.status]}>
                              {t(
                                `settings.notification_templates.deliveries.status_${
                                  delivery.status === 'queued' ? 'pending' : delivery.status
                                }`,
                              )}
                            </StatusBadge>
                          </td>
                          <td className="px-3 py-2 text-xs text-ink-2">{delivery.attempt}</td>
                          <td className="max-w-64 truncate px-3 py-2 font-mono text-xs text-ink-3">
                            {delivery.error ?? '—'}
                          </td>
                          <td className="px-3 py-2">
                            <Button
                              variant="ghost"
                              className="h-7 px-2 text-xs"
                              onClick={() => setDeliveryTarget(delivery)}
                            >
                              {t('settings.notification_templates.deliveries.details')}
                            </Button>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>

                <nav
                  aria-label={t('settings.notification_templates.pagination.label')}
                  className="flex items-center justify-between text-xs text-ink-3"
                >
                  <span>
                    {t('settings.notification_templates.pagination.summary', {
                      from: (deliveryPage - 1) * DELIVERY_PAGE_SIZE + 1,
                      to: Math.min(deliveryPage * DELIVERY_PAGE_SIZE, deliveryTotal),
                      total: deliveryTotal,
                    })}
                  </span>
                  <span className="flex items-center gap-2">
                    <Button
                      variant="secondary"
                      className="h-7 px-2 text-xs"
                      disabled={deliveryPage <= 1}
                      onClick={() =>
                        onSearchChange({
                          d_page: deliveryPage - 1 <= 1 ? undefined : deliveryPage - 1,
                        })
                      }
                    >
                      {t('settings.notification_templates.pagination.previous')}
                    </Button>
                    <span>
                      {t('settings.notification_templates.pagination.page', {
                        page: deliveryPage,
                        pages: deliveryPageCount,
                      })}
                    </span>
                    <Button
                      variant="secondary"
                      className="h-7 px-2 text-xs"
                      disabled={deliveryPage >= deliveryPageCount}
                      onClick={() =>
                        onSearchChange({ d_page: deliveryPage + 1 })
                      }
                    >
                      {t('settings.notification_templates.pagination.next')}
                    </Button>
                  </span>
                </nav>
              </>
            )
          ) : null}
        </section>
      )}

      <TemplateDrawer
        open={drawerTarget !== null}
        onOpenChange={(open) => {
          if (!open) setDrawerTarget(null)
        }}
        template={drawerTarget}
        canManage={canManage}
        onSaved={() => {
          void queryClient.invalidateQueries({ queryKey: ['notification-templates'] })
        }}
      />

      <PreviewDialog
        open={previewTarget !== null}
        onOpenChange={(open) => {
          if (!open) setPreviewTarget(null)
        }}
        channel={previewTarget?.channel ?? 'email'}
        subject={previewQuery.data?.subject ?? ''}
        body={previewQuery.data?.body ?? ''}
        loading={previewTarget !== null && previewQuery.isPending}
        error={
          previewQuery.isError
            ? `${t('settings.notification_templates.preview.error')} ${errorMessage(previewQuery.error)}`
            : null
        }
        onRetry={() => void previewQuery.refetch()}
      />

      <TestSendDialog
        open={testTarget !== null}
        onOpenChange={(open) => {
          if (!open) setTestTarget(null)
        }}
        template={testTarget}
        defaultRecipient={me.data?.email ?? ''}
        smtpConfigured={smtpConfigured}
      />

      <RestoreSingleDialog
        open={restoreTarget !== null}
        onOpenChange={(open) => {
          if (!open) setRestoreTarget(null)
        }}
        template={restoreTarget}
        onRestored={() => {
          void queryClient.invalidateQueries({ queryKey: ['notification-templates'] })
          void queryClient.invalidateQueries({ queryKey: ['notification-template-detail'] })
        }}
      />

      <RestoreAllDialog
        open={restoreAllOpen}
        onOpenChange={setRestoreAllOpen}
        count={filteredRows.length}
        categories={categories}
        channel={search.channel}
        locale={search.locale}
        modifiedOnly={search.modified === true}
        onRestored={() => {
          void queryClient.invalidateQueries({ queryKey: ['notification-templates'] })
          void queryClient.invalidateQueries({ queryKey: ['notification-template-detail'] })
        }}
      />

      <DeliveryDetailDrawer
        open={deliveryTarget !== null}
        onOpenChange={(open) => {
          if (!open) setDeliveryTarget(null)
        }}
        delivery={deliveryTarget}
      />
    </div>
  )
}
