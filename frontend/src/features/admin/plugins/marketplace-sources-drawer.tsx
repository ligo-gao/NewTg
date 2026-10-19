import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert } from '@/shared/ui/form'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'

import {
  checkPluginMarketplaceSource,
  listPluginMarketplaceSources,
  replacePluginMarketplaceSources,
  type MarketplaceSource,
} from './api'
import { ConfirmDialog, ErrorState, SkeletonRows } from './components'
import { notifyError } from './notify'
import { errorMessage } from './utils'

const CHECK_VARIANTS: Record<
  MarketplaceSource['last_check_status'],
  StatusBadgeVariant
> = {
  pending: 'neutral',
  ok: 'success',
  failed: 'danger',
}

/** 新增行没有 id（服务端按 URL 复用已有行）；已存在行原样回传 id。 */
interface SourcePayload {
  id?: string
  name: string
  url: string
  enabled: boolean
}

function sourcePayload(source: MarketplaceSource): SourcePayload {
  return {
    id: source.id,
    name: source.name,
    url: source.url,
    enabled: source.enabled,
  }
}

export interface MarketplaceSourcesDrawerProps {
  open: boolean
  canManage: boolean
  online: boolean
  onClose: () => void
  onChanged: () => void
}

/**
 * 市场源管理抽屉（F-08-34）。
 *
 * 契约是「整份替换 + 保存后立即试拉每个启用源」：新增/启用切换/删除都提交
 * 整份列表，服务端写回每行 ok/failed。URL 仅接受 https（R-M6D-03）——前端
 * 先做同一判定，服务端仍是权威；试拉失败也允许保存（源可临时不可达）。
 *
 * 「删除」与「新增」都走危险确认；新增的确认文案要求管理员信任来源。
 */
export function MarketplaceSourcesDrawer({
  open,
  canManage,
  online,
  onClose,
  onChanged,
}: MarketplaceSourcesDrawerProps) {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()

  const [name, setName] = useState('')
  const [url, setUrl] = useState('')
  const [formError, setFormError] = useState<string | null>(null)
  const [confirmAdd, setConfirmAdd] = useState(false)
  const [removeTarget, setRemoveTarget] = useState<MarketplaceSource | null>(null)
  const [expanded, setExpanded] = useState<string | null>(null)

  const sourcesQuery = useQuery({
    queryKey: ['plugin-sources'],
    queryFn: ({ signal }) => listPluginMarketplaceSources(signal),
    enabled: open,
  })
  const sources = sourcesQuery.data?.items ?? []

  function invalidate(): void {
    void queryClient.invalidateQueries({ queryKey: ['plugin-sources'] })
    void queryClient.invalidateQueries({ queryKey: ['plugin-marketplace'] })
    onChanged()
  }

  const saveMutation = useMutation({
    mutationFn: (next: SourcePayload[]) =>
      replacePluginMarketplaceSources({ sources: next }),
    onSuccess: () => {
      setName('')
      setUrl('')
      setConfirmAdd(false)
      setRemoveTarget(null)
      invalidate()
    },
    onError: (error) => {
      setConfirmAdd(false)
      setRemoveTarget(null)
      setFormError(errorMessage(error))
    },
  })

  const checkMutation = useMutation({
    mutationFn: (id: string) => checkPluginMarketplaceSource(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['plugin-sources'] })
      void queryClient.invalidateQueries({ queryKey: ['plugin-marketplace'] })
    },
    onError: (error) => notifyError(error),
  })

  function validateNewSource(): boolean {
    const trimmedName = name.trim()
    const trimmedUrl = url.trim()
    if (trimmedName === '') {
      setFormError(t('admin.task_plugins.sources.error_name'))
      return false
    }
    if (trimmedUrl === '') {
      setFormError(t('admin.task_plugins.sources.error_url'))
      return false
    }
    if (!trimmedUrl.startsWith('https://')) {
      setFormError(t('admin.task_plugins.sources.error_https'))
      return false
    }
    try {
      void new URL(trimmedUrl)
    } catch {
      setFormError(t('admin.task_plugins.sources.error_url_invalid'))
      return false
    }
    if (sources.some((source) => source.url === trimmedUrl)) {
      setFormError(t('admin.task_plugins.sources.error_duplicate'))
      return false
    }
    return true
  }

  const canWrite = canManage && online && !saveMutation.isPending

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) onClose()
      }}
      title={t('admin.task_plugins.sources.title')}
      description={t('admin.task_plugins.sources.description')}
      variant="sheet"
      footer={
        <Button variant="secondary" onClick={onClose}>
          {t('admin.task_plugins.common.close')}
        </Button>
      }
    >
      <div className="space-y-4">
        {sourcesQuery.isPending ? <SkeletonRows rows={3} /> : null}
        {sourcesQuery.isError ? (
          <ErrorState
            message={errorMessage(sourcesQuery.error)}
            onRetry={() => void sourcesQuery.refetch()}
          />
        ) : null}

        {sourcesQuery.data !== undefined && sources.length === 0 ? (
          <p className="rounded-lg border border-dashed border-border-default px-4 py-8 text-center text-sm text-ink-3">
            {t('admin.task_plugins.marketplace.empty_sources')}
          </p>
        ) : null}

        {sources.length > 0 ? (
          <ul className="space-y-2" data-testid="plugin-source-list">
            {sources.map((source) => (
              <li
                key={source.id}
                data-testid="plugin-source-row"
                className="rounded-lg border border-border-default px-3 py-2"
              >
                <div className="flex flex-wrap items-center gap-2">
                  <span className="font-medium text-ink-1">{source.name}</span>
                  <StatusBadge variant={CHECK_VARIANTS[source.last_check_status]}>
                    {t(`admin.task_plugins.sources.check_${source.last_check_status}`)}
                  </StatusBadge>
                  <label className="ml-auto flex items-center gap-1 text-xs text-ink-2">
                    <input
                      type="checkbox"
                      checked={source.enabled}
                      disabled={!canWrite}
                      aria-label={t('admin.task_plugins.sources.enabled_label', {
                        name: source.name,
                      })}
                      onChange={() =>
                        saveMutation.mutate(
                          sources.map((entry) =>
                            entry.id === source.id
                              ? { ...sourcePayload(entry), enabled: !entry.enabled }
                              : sourcePayload(entry),
                          ),
                        )
                      }
                    />
                    {t('admin.task_plugins.sources.enabled')}
                  </label>
                </div>
                <p className="mt-0.5 break-all font-mono text-xs text-ink-3">
                  {source.url}
                </p>
                <div className="mt-1 flex flex-wrap items-center gap-2 text-xs">
                  {source.last_check_at !== null &&
                  source.last_check_at !== undefined ? (
                    <span className="text-ink-3">
                      {formatRelativeTime(source.last_check_at, locale)}
                    </span>
                  ) : null}
                  {source.last_check_status === 'failed' ? (
                    <button
                      type="button"
                      className="text-danger underline"
                      onClick={() =>
                        setExpanded((current) =>
                          current === source.id ? null : source.id,
                        )
                      }
                    >
                      {t('admin.task_plugins.sources.show_error')}
                    </button>
                  ) : null}
                  <Button
                    variant="ghost"
                    className="h-6 px-2 text-xs"
                    disabled={!canWrite || checkMutation.isPending}
                    onClick={() => checkMutation.mutate(source.id)}
                  >
                    {t('admin.task_plugins.sources.check_now')}
                  </Button>
                  <Button
                    variant="ghost"
                    className="h-6 px-2 text-xs text-danger"
                    disabled={!canWrite}
                    onClick={() => setRemoveTarget(source)}
                  >
                    {t('admin.task_plugins.common.delete')}
                  </Button>
                </div>
                {expanded === source.id ? (
                  <p className="mt-1 overflow-x-auto whitespace-pre-wrap rounded bg-surface-subtle px-2 py-1 font-mono text-xs text-danger">
                    {(source.last_error ?? source.error ?? t('admin.task_plugins.sources.no_error')).slice(0, 512)}
                  </p>
                ) : null}
              </li>
            ))}
          </ul>
        ) : null}

        <div className="rounded-lg border border-border-default p-3">
          <p className="text-[13px] font-medium text-ink-1">
            {t('admin.task_plugins.sources.add_title')}
          </p>
          <div className="mt-2 flex flex-col gap-2">
            <input
              value={name}
              disabled={!canWrite}
              aria-label={t('admin.task_plugins.sources.name')}
              placeholder={t('admin.task_plugins.sources.name')}
              onChange={(event) => setName(event.target.value)}
              className="h-9 rounded-lg border border-border-default bg-surface-card px-3 text-sm"
            />
            <input
              type="url"
              value={url}
              disabled={!canWrite}
              aria-label={t('admin.task_plugins.sources.url')}
              placeholder="https://"
              onChange={(event) => setUrl(event.target.value)}
              className="h-9 rounded-lg border border-border-default bg-surface-card px-3 font-mono text-sm"
            />
            <p className="text-xs text-ink-3">
              {t('admin.task_plugins.sources.https_hint')}
            </p>
            <div className="flex justify-end">
              <Button
                variant="secondary"
                disabled={!canWrite}
                onClick={() => {
                  setFormError(null)
                  if (validateNewSource()) setConfirmAdd(true)
                }}
              >
                {t('admin.task_plugins.sources.add')}
              </Button>
            </div>
          </div>
        </div>

        {formError !== null ? <FormAlert>{formError}</FormAlert> : null}
        {!online ? (
          <p className="text-xs text-ink-3">{t('admin.task_plugins.common.offline')}</p>
        ) : null}
      </div>

      <ConfirmDialog
        open={confirmAdd}
        title={t('admin.task_plugins.sources.add_confirm_title')}
        confirmLabel={t('admin.task_plugins.sources.add')}
        confirmDisabled={saveMutation.isPending}
        onCancel={() => setConfirmAdd(false)}
        onConfirm={() =>
          saveMutation.mutate([
            ...sources.map(sourcePayload),
            { name: name.trim(), url: url.trim(), enabled: true },
          ])
        }
      >
        <p>{t('admin.task_plugins.sources.add_confirm_body')}</p>
      </ConfirmDialog>

      <ConfirmDialog
        open={removeTarget !== null}
        title={t('admin.task_plugins.sources.remove_title', {
          name: removeTarget?.name ?? '',
        })}
        confirmLabel={t('admin.task_plugins.common.delete')}
        confirmDisabled={saveMutation.isPending}
        onCancel={() => setRemoveTarget(null)}
        onConfirm={() => {
          if (removeTarget === null) return
          saveMutation.mutate(
            sources
              .filter((source) => source.id !== removeTarget.id)
              .map(sourcePayload),
          )
        }}
      >
        <p>{t('admin.task_plugins.sources.remove_body')}</p>
      </ConfirmDialog>
    </Dialog>
  )
}
