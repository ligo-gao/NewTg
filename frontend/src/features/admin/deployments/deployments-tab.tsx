import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'

import { listDeployments, type DeployDeployment } from './api'
import { DeploymentDetailDrawer } from './detail-drawer'
import { ExtendDialog } from './extend-dialog'
import { RenameDialog } from './rename-dialog'
import { TerminateDialog } from './terminate-dialog'
import { UpdateDialog } from './update-dialog'
import { DeploymentStatusBadge, DEPLOYMENT_STATUSES, statusMeta } from './status'
import { useOnlineStatus } from './use-online'
import {
  errorMessage,
  formatDateTime,
  formatRemainingMinutes,
  isForbidden,
} from './utils'

const PAGE_SIZE = 20

export type DeploymentActionKind = 'extend' | 'rename' | 'update' | 'terminate'

export interface DeploymentActionTarget {
  kind: DeploymentActionKind
  id: string
  name: string
  status: string
}

export interface DeploymentsTabProps {
  canManage: boolean
  /** 设置读取失败时保持 `undefined`：不臆断「未启用」。 */
  enabled: boolean | undefined
  hasSecret: boolean | undefined
  onGoSettings: () => void
  onCreate: () => void
}

function RowMenu({
  row,
  canManage,
  online,
  enabled,
  onAction,
  onDetail,
}: {
  row: DeployDeployment
  canManage: boolean
  online: boolean
  enabled: boolean | undefined
  onAction: (target: DeploymentActionTarget) => void
  onDetail: () => void
}) {
  const { t } = useI18n()
  // 已销毁/终止中的部署不再提供写操作；后端仍会以 409 兜底。
  const status = row.status.trim().toLowerCase()
  const writable = canManage && online && enabled !== false && status !== 'destroyed'
  const target = (kind: DeploymentActionKind): DeploymentActionTarget => ({
    kind,
    id: row.id,
    name: row.name,
    status: row.status,
  })

  return (
    <div className="flex flex-wrap gap-1">
      <Button variant="ghost" className="h-7 px-2 text-xs" onClick={onDetail}>
        {t('admin.deployments.actions.detail')}
      </Button>
      {writable ? (
        <>
          <Button
            variant="ghost"
            className="h-7 px-2 text-xs"
            onClick={() => onAction(target('extend'))}
          >
            {t('admin.deployments.actions.extend')}
          </Button>
          <Button
            variant="ghost"
            className="h-7 px-2 text-xs"
            onClick={() => onAction(target('rename'))}
          >
            {t('admin.deployments.actions.rename')}
          </Button>
          <Button
            variant="ghost"
            className="h-7 px-2 text-xs"
            onClick={() => onAction(target('update'))}
          >
            {t('admin.deployments.actions.update')}
          </Button>
          <Button
            variant="ghost"
            className="h-7 px-2 text-xs text-danger"
            onClick={() => onAction(target('terminate'))}
          >
            {t('admin.deployments.actions.terminate')}
          </Button>
        </>
      ) : null}
    </div>
  )
}

/**
 * 部署 Tab（F-08-52）：关键词 + 状态筛选、状态计数条、表格与 offset 分页。
 *
 * 搜索与状态都由**服务端**在全结果集上执行；`status_counts` 是扫描全量
 * 后的计数，不受当前页影响。`search_truncated` 时显式提示结果可能不完整。
 */
export function DeploymentsTab({
  canManage,
  enabled,
  hasSecret,
  onGoSettings,
  onCreate,
}: DeploymentsTabProps) {
  const { t, locale } = useI18n()
  const online = useOnlineStatus()
  const [keywordInput, setKeywordInput] = useState('')
  const [keyword, setKeyword] = useState('')
  const [status, setStatus] = useState('')
  const [page, setPage] = useState(1)
  const [detail, setDetail] = useState<{ id: string; name: string } | null>(null)
  const [action, setAction] = useState<DeploymentActionTarget | null>(null)

  const query = useQuery({
    queryKey: ['deployments', keyword, status, page],
    queryFn: ({ signal }) =>
      listDeployments(
        {
          keyword: keyword === '' ? undefined : keyword,
          status: status === '' ? undefined : status,
          p: page,
          page_size: PAGE_SIZE,
        },
        signal,
      ),
    enabled: canManage && enabled !== false,
    // 部署写操作与列表都不自动重试：provider 控制面的失败往往是稳定结果
    // （禁用、限流），重试只会放大审计与费用风险。
    retry: false,
  })

  const data = query.data
  const total = data?.total ?? 0
  const pageCount = Math.max(1, Math.ceil(total / (data?.page_size ?? PAGE_SIZE)))
  const currentPage = data?.page ?? page
  const rows = data?.items ?? []
  const counts = data?.status_counts ?? {}
  const goSettings = (): void => onGoSettings()

  const applySearch = (): void => {
    setKeyword(keywordInput.trim())
    setPage(1)
  }

  const selectStatus = (next: string): void => {
    setStatus(next)
    setPage(1)
  }

  const disabledReason =
    enabled === false || hasSecret === false
      ? null
      : !online
        ? t('admin.deployments.common.offline')
        : !canManage
          ? t('admin.deployments.common.forbidden')
          : null

  return (
    <section
      className="space-y-3"
      aria-label={t('admin.deployments.page.tab_deployments')}
      data-testid="deployments-tab"
    >
      {enabled === false || hasSecret === false ? (
        <FormAlert tone="info">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span>
              {enabled === false
                ? t('admin.deployments.list.banner_disabled')
                : t('admin.deployments.list.banner_no_secret')}
            </span>
            <Button variant="secondary" className="h-7 px-2 text-xs" onClick={goSettings}>
              {t('admin.deployments.list.go_settings')}
            </Button>
          </div>
        </FormAlert>
      ) : null}

      <div className="flex flex-wrap items-end justify-between gap-3">
        <form
          className="flex flex-wrap items-end gap-2"
          onSubmit={(event) => {
            event.preventDefault()
            applySearch()
          }}
        >
          <label className="flex flex-col gap-1 text-xs text-ink-2">
            {t('admin.deployments.list.search_label')}
            <input
              aria-label={t('admin.deployments.list.search_label')}
              value={keywordInput}
              onChange={(event) => setKeywordInput(event.target.value)}
              placeholder={t('admin.deployments.list.search_placeholder')}
              className="h-9 w-56 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            />
          </label>
          {keyword !== '' ? (
            <Button
              variant="ghost"
              className="h-9 px-2 text-xs"
              onClick={() => {
                setKeywordInput('')
                setKeyword('')
                setPage(1)
              }}
            >
              {t('admin.deployments.common.clear')}
            </Button>
          ) : null}
          <label className="flex flex-col gap-1 text-xs text-ink-2">
            {t('admin.deployments.list.status_filter')}
            <select
              aria-label={t('admin.deployments.list.status_filter')}
              value={status}
              onChange={(event) => selectStatus(event.target.value)}
              className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
            >
              <option value="">{t('admin.deployments.list.all_statuses')}</option>
              {DEPLOYMENT_STATUSES.map((entry) => (
                <option key={entry} value={entry}>
                  {t(statusMeta(entry)?.key ?? entry)}
                </option>
              ))}
            </select>
          </label>
          <Button type="submit" variant="secondary" className="h-9">
            {t('admin.deployments.common.search')}
          </Button>
        </form>
        <p className="text-xs text-ink-3" role="status">
          {t('admin.deployments.list.total', { total })}
        </p>
      </div>

      <div className="flex flex-wrap gap-1" data-testid="deployment-status-counts">
        {[{ value: '', count: counts.all ?? total }, ...DEPLOYMENT_STATUSES.map((entry) => ({ value: entry, count: counts[entry] ?? 0 }))].map(
          (entry) => {
            const selected = status === entry.value
            return (
              <button
                key={entry.value === '' ? 'all' : entry.value}
                type="button"
                aria-pressed={selected}
                onClick={() => selectStatus(entry.value)}
                className={[
                  'rounded-full border px-2.5 py-1 text-xs',
                  selected
                    ? 'border-brand bg-brand-subtle text-brand'
                    : 'border-border-default text-ink-2 hover:bg-surface-hover',
                ].join(' ')}
              >
                {entry.value === ''
                  ? t('admin.deployments.status.all')
                  : t(statusMeta(entry.value)?.key ?? entry.value)}{' '}
                <span className="font-mono">{entry.count}</span>
              </button>
            )
          },
        )}
      </div>

      {data?.search_truncated === true ? (
        <FormAlert tone="warning">
          {t('admin.deployments.list.truncated', { n: 1000 })}
        </FormAlert>
      ) : null}

      {query.isPending && enabled !== false ? (
        <div className="space-y-2" aria-hidden="true">
          {Array.from({ length: 5 }, (_, index) => (
            <div key={index} className="h-14 rounded-lg bg-surface-subtle" />
          ))}
        </div>
      ) : null}

      {query.isError ? (
        <div className="space-y-2">
          <FormAlert tone="danger">
            {isForbidden(query.error)
              ? t('admin.deployments.common.forbidden')
              : `${t('admin.deployments.list.load_error')} ${errorMessage(query.error)}`}
          </FormAlert>
          {data !== undefined ? (
            <p className="text-xs text-warning" role="status">
              {t('admin.deployments.list.provider_stale')}
            </p>
          ) : null}
          <Button variant="secondary" onClick={() => void query.refetch()}>
            {t('admin.deployments.common.retry')}
          </Button>
        </div>
      ) : null}

      {enabled === false ? (
        <p className="rounded-xl border border-dashed border-border-default px-4 py-10 text-center text-sm text-ink-3">
          {t('admin.deployments.list.empty')}
        </p>
      ) : null}

      {query.isSuccess && rows.length === 0 ? (
        <div className="rounded-xl border border-dashed border-border-default px-4 py-10 text-center text-sm text-ink-3">
          <p>
            {keyword !== '' || status !== ''
              ? t('admin.deployments.list.empty_filtered')
              : t('admin.deployments.list.empty')}
          </p>
          {keyword !== '' || status !== '' ? (
            <Button
              variant="secondary"
              className="mt-3 h-7 px-2 text-xs"
              onClick={() => {
                setKeywordInput('')
                setKeyword('')
                setStatus('')
                setPage(1)
              }}
            >
              {t('admin.deployments.common.clear')}
            </Button>
          ) : canManage && enabled !== false ? (
            <Button className="mt-3" disabled={hasSecret === false} onClick={onCreate}>
              {t('admin.deployments.list.create')}
            </Button>
          ) : null}
        </div>
      ) : null}

      {query.isSuccess && rows.length > 0 ? (
        <>
          <div className="overflow-x-auto rounded-xl border border-border-default">
            <table className="w-full text-left text-sm">
              <thead className="bg-surface-subtle text-xs text-ink-3">
                <tr>
                  <th className="sticky left-0 bg-surface-subtle px-3 py-2 font-medium">
                    {t('admin.deployments.list.col_name')}
                  </th>
                  <th className="px-3 py-2 font-medium">
                    {t('admin.deployments.list.col_status')}
                  </th>
                  <th className="px-3 py-2 font-medium">
                    {t('admin.deployments.list.col_hardware')}
                  </th>
                  <th className="px-3 py-2 font-medium">
                    {t('admin.deployments.list.col_gpu_replica')}
                  </th>
                  <th className="hidden px-3 py-2 font-medium xl:table-cell">
                    {t('admin.deployments.list.col_location')}
                  </th>
                  <th className="px-3 py-2 font-medium">
                    {t('admin.deployments.list.col_remaining')}
                  </th>
                  <th className="hidden px-3 py-2 font-medium xl:table-cell">
                    {t('admin.deployments.list.col_created')}
                  </th>
                  <th className="px-3 py-2 font-medium">
                    {t('admin.deployments.list.col_actions')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {rows.map((row) => (
                  <tr
                    key={row.id}
                    data-testid="deployment-row"
                    className="border-t border-border-hairline align-middle"
                  >
                    <td className="sticky left-0 bg-surface-card px-3 py-2">
                      <span className="font-medium text-ink-1">{row.name}</span>
                      <span className="ml-2 font-mono text-[11px] text-ink-3">
                        {row.id}
                      </span>
                    </td>
                    <td className="px-3 py-2">
                      <DeploymentStatusBadge status={row.status} />
                    </td>
                    <td className="px-3 py-2 font-mono text-xs text-ink-2">
                      {row.brand_name !== undefined && row.brand_name !== ''
                        ? `${row.brand_name} ${row.hardware_name ?? ''}`.trim()
                        : (row.hardware_name ?? '—')}
                    </td>
                    <td className="px-3 py-2 font-mono text-xs text-ink-2">
                      {row.gpu_count}/{row.replica_count}
                    </td>
                    <td className="hidden px-3 py-2 text-xs text-ink-3 xl:table-cell">
                      {/* 列表契约无地域字段（T4 冻结形状）；详情抽屉里展示。 */}
                      —
                    </td>
                    <td className="px-3 py-2 font-mono text-xs text-ink-2">
                      {formatRemainingMinutes(row.compute_minutes_remaining)}
                    </td>
                    <td className="hidden px-3 py-2 text-xs text-ink-3 xl:table-cell">
                      {formatDateTime(row.created_at, locale)}
                    </td>
                    <td className="px-3 py-2">
                      <RowMenu
                        row={row}
                        canManage={canManage}
                        online={online}
                        enabled={enabled}
                        onAction={setAction}
                        onDetail={() => setDetail({ id: row.id, name: row.name })}
                      />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          {disabledReason !== null ? (
            <p className="text-xs text-ink-3">{disabledReason}</p>
          ) : null}

          <nav
            aria-label={t('admin.deployments.list.pagination_label')}
            className="flex items-center justify-between text-xs text-ink-3"
          >
            <span>
              {t('admin.deployments.list.page_summary', {
                from: total === 0 ? 0 : (currentPage - 1) * PAGE_SIZE + 1,
                to: Math.min(currentPage * PAGE_SIZE, total),
                total,
              })}
            </span>
            <span className="flex items-center gap-2">
              <Button
                variant="secondary"
                className="h-7 px-2 text-xs"
                disabled={currentPage <= 1 || query.isFetching}
                onClick={() => setPage(Math.max(1, currentPage - 1))}
              >
                {t('admin.deployments.common.previous')}
              </Button>
              <span>
                {t('admin.deployments.list.page_of', {
                  page: currentPage,
                  pages: pageCount,
                })}
              </span>
              <Button
                variant="secondary"
                className="h-7 px-2 text-xs"
                disabled={currentPage >= pageCount || query.isFetching}
                onClick={() => setPage(currentPage + 1)}
              >
                {t('admin.deployments.common.next')}
              </Button>
            </span>
          </nav>
        </>
      ) : null}

      <DeploymentDetailDrawer
        deploymentId={detail?.id ?? null}
        deploymentName={detail?.name}
        canManage={canManage}
        enabled={enabled}
        onClose={() => setDetail(null)}
        onAction={setAction}
      />

      {action !== null ? (
        <>
          <ExtendDialog
            target={action.kind === 'extend' ? action : null}
            onClose={() => setAction(null)}
          />
          <RenameDialog
            target={action.kind === 'rename' ? action : null}
            onClose={() => setAction(null)}
          />
          <UpdateDialog
            target={action.kind === 'update' ? action : null}
            onClose={() => setAction(null)}
          />
          <TerminateDialog
            target={action.kind === 'terminate' ? action : null}
            onClose={() => setAction(null)}
          />
        </>
      ) : null}
    </section>
  )
}
