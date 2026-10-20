import { useEffect, useState, type ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert } from '@/shared/ui/form'
import { StatusBadge } from '@/shared/ui/status-badge'
import { useToastStore } from '@/shared/ui/toast'

import { invalidateCatalog } from './ModelMutateSheet'

type ModelSyncPreview = components['schemas']['ModelSyncPreview']
type ModelSyncApplyRequest = components['schemas']['ModelSyncApplyRequest']

const SYNC_PREVIEW_KEY = ['admin-models-sync-preview'] as const

export interface SyncUpstreamDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
}

/**
 * F-06-25 上游元数据同步：preview → apply 两段。
 *
 * - preview 把上游清单的 `source_version`（内容 sha256）与分类结果一起返回；
 * - apply 必须原样回传该版本，否则服务端 409 并整体回滚；
 * - `blocked`（本地 `sync_official=false`）永不可勾选，前端置灰、后端仍校验；
 * - 客户端提交的条目**只用于选择**，写入值取自服务端缓存的清单（防篡改）。
 *
 * 版本冲突后不会自动重新预览：重新预览会改变管理员眼前的勾选依据，必须由
 * 人确认后再应用（任务书 §1「409 提示重新预览」）。
 */
export function SyncUpstreamDialog({
  open,
  onOpenChange,
}: SyncUpstreamDialogProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)

  const [selectedModels, setSelectedModels] = useState<ReadonlySet<string>>(new Set())
  const [selectedVendors, setSelectedVendors] = useState<ReadonlySet<string>>(new Set())
  const [conflict, setConflict] = useState(false)
  const [applyError, setApplyError] = useState<string | undefined>(undefined)

  const preview = useQuery({
    queryKey: SYNC_PREVIEW_KEY,
    enabled: open,
    retry: false,
    queryFn: async () => {
      return requireData(await apiClient.GET('/api/models/sync/preview'))
    },
  })

  // 关闭（含应用成功后的自动关闭）即丢弃缓存：下一次打开必须重新预览。
  // 否则 React Query 会先把旧清单当成 `data` 渲染出来，管理员可能在
  // 「看起来还能用」的过期勾选上做决定（任务书 §1「409 提示重新预览」）。
  useEffect(() => {
    if (!open) {
      queryClient.removeQueries({ queryKey: SYNC_PREVIEW_KEY })
    }
  }, [open, queryClient])

  // 新预览默认勾选全部可写条目（create + update）；blocked 不进入选择集。
  useEffect(() => {
    const data: ModelSyncPreview | undefined = preview.data
    if (data === undefined) return
    setSelectedModels(
      new Set([
        ...data.groups.create.map((item) => item.model_name),
        ...data.groups.update.map((item) => item.model_name),
      ]),
    )
    setSelectedVendors(
      new Set([
        ...data.groups.vendors.create,
        ...data.groups.vendors.update.map((item) => item.name),
      ]),
    )
    setConflict(false)
    setApplyError(undefined)
  }, [preview.data])

  const apply = useMutation({
    mutationFn: async (data: ModelSyncPreview) => {
      const body: ModelSyncApplyRequest = {
        source_version: data.source_version,
        models: [...selectedModels].map((model_name) => ({ model_name })),
        vendors: [...selectedVendors].map((name) => ({ name })),
      }
      return requireData(await apiClient.POST('/api/models/sync', { body }))
    },
    retry: false,
    onSuccess: async (result) => {
      push({
        tone: 'success',
        message: t('admin.models.sync_success', {
          models: result.created_models.length + result.updated_models.length,
          vendors: result.created_vendors.length + result.updated_vendors.length,
        }),
      })
      await invalidateCatalog(queryClient)
      onOpenChange(false)
    },
    onError: (error) => {
      if (error instanceof ApiError && error.kind === 'conflict') {
        // 版本过期 / 清单已变：界面上的预览不再可信，要求重新预览。
        setConflict(true)
        return
      }
      setApplyError(
        error instanceof ApiError && error.kind === 'forbidden'
          ? t('admin.models.forbidden')
          : t('admin.models.sync_apply_error'),
      )
    },
  })

  function toggleModel(name: string, checked: boolean): void {
    setSelectedModels((current) => {
      const next = new Set(current)
      if (checked) next.add(name)
      else next.delete(name)
      return next
    })
  }

  function toggleVendor(name: string, checked: boolean): void {
    setSelectedVendors((current) => {
      const next = new Set(current)
      if (checked) next.add(name)
      else next.delete(name)
      return next
    })
  }

  const data = preview.data
  const totalSelected = selectedModels.size + selectedVendors.size

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      variant="sheet"
      panelClassName="!max-w-[640px] xl:!max-w-[720px]"
      title={t('admin.models.sync_title')}
      description={t('admin.models.sync_source_hint')}
    >
      <div className="flex flex-col gap-4">
        <div className="rounded-lg border border-border-hairline bg-surface-subtle px-3 py-2 text-xs text-ink-2">
          <code className="font-mono text-ink-1">model_sync.source_url</code>
          <span className="ml-2">{t('admin.models.sync_source_value_hint')}</span>
        </div>

        {conflict ? (
          <FormAlert tone="warning">{t('admin.models.sync_conflict')}</FormAlert>
        ) : null}
        {applyError !== undefined ? (
          <FormAlert tone="danger">{applyError}</FormAlert>
        ) : null}

        {preview.isPending ? (
          <div className="space-y-2" aria-busy="true">
            {[0, 1, 2, 3].map((index) => (
              <div key={index} className="h-10 animate-pulse rounded bg-surface-subtle" />
            ))}
          </div>
        ) : preview.isError ? (
          <div className="flex flex-col items-start gap-2">
            <p className="text-sm text-ink-2">
              {preview.error instanceof ApiError && preview.error.status === 503
                ? t('admin.models.sync_unconfigured')
                : t('admin.models.sync_preview_error')}
            </p>
            <Button variant="secondary" onClick={() => void preview.refetch()}>
              {t('common.action.retry')}
            </Button>
          </div>
        ) : data === undefined ? null : (
          <>
            <div className="flex items-center gap-2 text-xs text-ink-2">
              <span>{t('admin.models.sync_version')}</span>
              <code className="font-mono text-ink-1">{data.source_version.slice(0, 16)}…</code>
            </div>

            <SyncGroup title={t('admin.models.sync_group_create')} count={data.groups.create.length}>
              {data.groups.create.map((item) => (
                <label key={item.model_name} className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={selectedModels.has(item.model_name)}
                    onChange={(event) => {
                      toggleModel(item.model_name, event.target.checked)
                    }}
                  />
                  <span className="font-mono text-ink-1">{item.model_name}</span>
                </label>
              ))}
            </SyncGroup>

            <SyncGroup title={t('admin.models.sync_group_update')} count={data.groups.update.length}>
              {data.groups.update.map((item) => (
                <div key={item.model_name} className="flex flex-col gap-1">
                  <label className="flex items-center gap-2 text-sm">
                    <input
                      type="checkbox"
                      checked={selectedModels.has(item.model_name)}
                      onChange={(event) => {
                        toggleModel(item.model_name, event.target.checked)
                      }}
                    />
                    <span className="font-mono text-ink-1">{item.model_name}</span>
                  </label>
                  <table className="ml-6 text-xs">
                    <tbody>
                      {Object.entries(item.changes ?? {}).map(([field, change]) => (
                        <tr key={field}>
                          <td className="pr-2 font-mono text-ink-3">{field}</td>
                          <td className="pr-2 text-ink-2">{formatSyncValue(change.local)}</td>
                          <td className="pr-2 text-ink-3">→</td>
                          <td className="text-ink-1">{formatSyncValue(change.upstream)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              ))}
            </SyncGroup>

            <SyncGroup title={t('admin.models.sync_group_blocked')} count={data.groups.blocked.length}>
              {data.groups.blocked.map((name) => (
                <label key={name} className="flex items-center gap-2 text-sm text-ink-3">
                  <input type="checkbox" disabled checked={false} readOnly />
                  <span className="font-mono">{name}</span>
                  <StatusBadge variant="neutral">{t('admin.models.sync_blocked_tag')}</StatusBadge>
                </label>
              ))}
            </SyncGroup>

            <SyncGroup title={t('admin.models.sync_group_unchanged')} count={data.groups.unchanged.length}>
              <p className="text-xs text-ink-3">
                {data.groups.unchanged.length === 0
                  ? t('admin.models.sync_none')
                  : data.groups.unchanged.map((name) => (
                      <span key={name} className="mr-2 inline-block font-mono">
                        {name}
                      </span>
                    ))}
              </p>
            </SyncGroup>

            <SyncGroup
              title={t('admin.models.sync_group_missing_upstream')}
              count={data.groups.missing_upstream.length}
            >
              <p className="text-xs text-ink-3">
                {data.groups.missing_upstream.length === 0
                  ? t('admin.models.sync_none')
                  : data.groups.missing_upstream.join(', ')}
              </p>
            </SyncGroup>

            <SyncGroup
              title={t('admin.models.sync_group_missing_vendor')}
              count={data.groups.missing_vendor.length}
            >
              <p className="text-xs text-ink-3">
                {data.groups.missing_vendor.length === 0
                  ? t('admin.models.sync_none')
                  : data.groups.missing_vendor.join(', ')}
              </p>
            </SyncGroup>

            <SyncGroup
              title={t('admin.models.sync_group_vendors')}
              count={data.groups.vendors.create.length + data.groups.vendors.update.length}
            >
              {data.groups.vendors.create.map((name) => (
                <label key={name} className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={selectedVendors.has(name)}
                    onChange={(event) => {
                      toggleVendor(name, event.target.checked)
                    }}
                  />
                  <span className="text-ink-1">{name}</span>
                  <StatusBadge variant="success">{t('admin.models.sync_group_create')}</StatusBadge>
                </label>
              ))}
              {data.groups.vendors.update.map((item) => (
                <label key={item.name} className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={selectedVendors.has(item.name)}
                    onChange={(event) => {
                      toggleVendor(item.name, event.target.checked)
                    }}
                  />
                  <span className="text-ink-1">{item.name}</span>
                  <StatusBadge variant="warning">{t('admin.models.sync_group_update')}</StatusBadge>
                </label>
              ))}
            </SyncGroup>

            {data.groups.missing_vendor.length > 0 ? (
              <p className="text-xs text-warning">{t('admin.models.sync_missing_vendor_hint')}</p>
            ) : null}

            {conflict ? (
              <Button
                variant="secondary"
                disabled={preview.isFetching}
                onClick={() => void preview.refetch()}
              >
                {t('admin.models.sync_repreview')}
              </Button>
            ) : null}

            <div className="flex justify-end gap-2">
              <Button variant="secondary" onClick={() => onOpenChange(false)}>
                {t('common.action.cancel')}
              </Button>
              <Button
                disabled={
                  apply.isPending || preview.isFetching || totalSelected === 0 || conflict
                }
                onClick={() => apply.mutate(data)}
              >
                {t('admin.models.sync_apply', { count: totalSelected })}
              </Button>
            </div>
          </>
        )}
      </div>
    </Dialog>
  )
}

function SyncGroup({
  title,
  count,
  children,
}: {
  title: string
  count: number
  children: ReactNode
}) {
  return (
    <section className="rounded-lg border border-border-hairline">
      <header className="flex items-center justify-between border-b border-border-hairline px-3 py-2">
        <h3 className="text-[13px] font-medium text-ink-1">{title}</h3>
        <span className="font-mono text-xs tabular-nums text-ink-3">{count}</span>
      </header>
      <div className="flex flex-col gap-2 px-3 py-2">{children}</div>
    </section>
  )
}

function formatSyncValue(value: unknown): string {
  if (value === null || value === undefined) return '—'
  if (typeof value === 'string') return value
  return JSON.stringify(value)
}
