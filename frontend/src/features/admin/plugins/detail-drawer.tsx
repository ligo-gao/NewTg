import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState, type KeyboardEvent } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert } from '@/shared/ui/form'
import { Tag } from '@/shared/ui/tag'

import {
  activateTaskPluginVersion,
  deleteTaskPluginVersion,
  getTaskPlugin,
  getTaskPluginSourceText,
  type TaskPluginVersion,
} from './api'
import { ConfirmDialog, ErrorState, RuntimeStatusBadge, SourceTag } from './components'
import { diffLines, errorMessage, shortHash } from './utils'

export type PluginDetailTab =
  | 'metadata'
  | 'endpoints'
  | 'models'
  | 'usage'
  | 'integrity'
  | 'history'

const DETAIL_TABS: readonly PluginDetailTab[] = [
  'metadata',
  'endpoints',
  'models',
  'usage',
  'integrity',
  'history',
]

const TAB_LABEL_KEYS: Record<PluginDetailTab, string> = {
  metadata: 'admin.task_plugins.detail.tab_metadata',
  endpoints: 'admin.task_plugins.detail.tab_endpoints',
  models: 'admin.task_plugins.detail.tab_models',
  usage: 'admin.task_plugins.detail.tab_usage',
  integrity: 'admin.task_plugins.detail.tab_integrity',
  history: 'admin.task_plugins.detail.tab_history',
}

function Field({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-meta text-ink-3">{label}</dt>
      <dd className="break-words font-mono text-body text-ink-1">{value}</dd>
    </div>
  )
}

/**
 * 只读源查看器（F-08-29 完整性页签）：无编辑能力、无保存按钮，
 * `aria-readonly` 明示语义；行号等宽。
 */
export function SourceViewer({
  text,
  testId,
}: {
  text: string
  testId?: string
}) {
  const { t } = useI18n()
  const lines = text.split('\n')
  return (
    <div
      data-testid={testId}
      aria-readonly="true"
      className="mt-2 overflow-auto rounded-lg border border-border-default bg-surface-subtle"
      style={{ maxHeight: 420 }}
    >
      <div className="flex items-center justify-between border-b border-border-hairline px-3 py-1.5">
        <span className="text-meta text-ink-3">
          {t('admin.task_plugins.detail.source_viewer')}
        </span>
        <Tag variant="neutral">{t('admin.task_plugins.detail.readonly')}</Tag>
      </div>
      <table
        aria-readonly="true"
        aria-label={t('admin.task_plugins.detail.source_viewer')}
        className="w-full border-collapse font-mono text-meta"
      >
        <tbody>
          {lines.map((line, index) => (
            <tr key={index}>
              <td className="w-10 select-none border-r border-border-hairline px-2 py-0 text-right align-top text-ink-3">
                {index + 1}
              </td>
              <td className="whitespace-pre px-2 text-ink-1">{line}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function VersionDiff({
  leftVersion,
  rightVersion,
  leftText,
  rightText,
}: {
  leftVersion: string
  rightVersion: string
  leftText: string
  rightText: string
}) {
  const { t } = useI18n()
  const rows = diffLines(leftText, rightText)
  const cellClass = (kind: string, side: 'left' | 'right'): string => {
    if (kind === 'same') return 'text-ink-2'
    if (kind === 'changed') return 'bg-warning-bg text-warning'
    if (kind === 'removed') return side === 'left' ? 'bg-danger-bg text-danger' : ''
    return side === 'right' ? 'bg-success-bg text-success' : ''
  }
  return (
    <div data-testid="plugin-source-diff" className="mt-3">
      <div className="grid grid-cols-2 gap-2 text-meta text-ink-3">
        <span>{leftVersion}</span>
        <span>{rightVersion}</span>
      </div>
      <div className="mt-1 overflow-auto rounded-lg border border-border-default" style={{ maxHeight: 360 }}>
        <table aria-label={t('admin.task_plugins.detail.compare_title', { version: leftVersion })} className="w-full border-collapse font-mono text-meta">
          <tbody>
            {rows.map((row, index) => (
              <tr key={index}>
                <td className={`w-1/2 whitespace-pre border-r border-border-hairline px-2 align-top ${cellClass(row.kind, 'left')}`}>
                  {row.left === null ? ' ' : `${index + 1}  ${row.left}`}
                </td>
                <td className={`w-1/2 whitespace-pre px-2 align-top ${cellClass(row.kind, 'right')}`}>
                  {row.right === null ? ' ' : `${index + 1}  ${row.right}`}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

export interface PluginDetailDrawerProps {
  open: boolean
  pluginKey: string | null
  initialTab: PluginDetailTab
  canManage: boolean
  online: boolean
  onClose: () => void
  /** 任何写操作成功后通知父级（列表/运行时缓存失效）。 */
  onChanged: () => void
}

/**
 * 插件详情抽屉（§6.10，F-08-29 / F-08-31）：宽 720、六页签、源只读。
 *
 * 元数据/完整性/变更历史来自 `GET /api/plugin/task/{key}`；端点/模型/用量
 * schema 三页签在契约里没有结构化字段（manifest 只在服务端解析），因此明确
 * 显示「当前接口未暴露该结构」而不是伪造空表——该项已登记。
 */
export function PluginDetailDrawer({
  open,
  pluginKey,
  initialTab,
  canManage,
  online,
  onClose,
  onChanged,
}: PluginDetailDrawerProps) {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const [tab, setTab] = useState<PluginDetailTab>(initialTab)
  const [compareVersion, setCompareVersion] = useState<string | null>(null)
  const [activateTarget, setActivateTarget] = useState<TaskPluginVersion | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<TaskPluginVersion | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  // 打开时按调用方指定的页签定位（编译失败「查看错误」直达完整性）。
  useEffect(() => {
    if (open) setTab(initialTab)
  }, [open, initialTab])

  const detailQuery = useQuery({
    queryKey: ['plugin-detail', pluginKey],
    queryFn: ({ signal }) => getTaskPlugin(pluginKey ?? '', true, signal),
    enabled: open && pluginKey !== null,
  })
  const detail = detailQuery.data
  const activeVersion = detail?.plugin.active_version ?? null
  const activeVersionRow =
    detail?.versions.find((entry) => entry.version === activeVersion) ?? null

  const compareQuery = useQuery({
    queryKey: ['plugin-source', pluginKey, compareVersion],
    queryFn: ({ signal }) =>
      getTaskPluginSourceText(pluginKey ?? '', compareVersion ?? undefined, signal),
    enabled: open && pluginKey !== null && compareVersion !== null,
  })

  function invalidate(): void {
    void queryClient.invalidateQueries({ queryKey: ['plugin-detail', pluginKey] })
    void queryClient.invalidateQueries({ queryKey: ['plugin-list'] })
    void queryClient.invalidateQueries({ queryKey: ['plugin-runtime'] })
    onChanged()
  }

  const activateMutation = useMutation({
    mutationFn: (version: string) =>
      activateTaskPluginVersion(
        pluginKey ?? '',
        version,
        detail?.plugin.updated_at ?? null,
      ),
    onSuccess: (result) => {
      setNotice(t('admin.task_plugins.detail.activate_success', { version: result.version.version }))
      setActivateTarget(null)
      invalidate()
    },
    onError: (error: unknown) => {
      setActionError(errorMessage(error))
      setActivateTarget(null)
    },
  })

  const deleteMutation = useMutation({
    mutationFn: (version: string) =>
      deleteTaskPluginVersion(
        pluginKey ?? '',
        version,
        detail?.plugin.updated_at ?? null,
      ),
    onSuccess: (result) => {
      setNotice(
        result.promoted_version === ''
          ? t('admin.task_plugins.detail.delete_success', {
              version: result.deleted_version,
            })
          : t('admin.task_plugins.detail.delete_promoted', {
              version: result.deleted_version,
              promoted: result.promoted_version,
            }),
      )
      setDeleteTarget(null)
      setCompareVersion(null)
      invalidate()
    },
    onError: (error: unknown) => {
      setActionError(errorMessage(error))
      setDeleteTarget(null)
    },
  })

  const handleTabKey = (event: KeyboardEvent<HTMLDivElement>): void => {
    if (event.key !== 'ArrowRight' && event.key !== 'ArrowLeft') return
    event.preventDefault()
    const index = DETAIL_TABS.indexOf(tab)
    const next =
      event.key === 'ArrowRight'
        ? DETAIL_TABS[(index + 1) % DETAIL_TABS.length]
        : DETAIL_TABS[(index - 1 + DETAIL_TABS.length) % DETAIL_TABS.length]
    if (next === undefined) return
    setTab(next)
    document.getElementById(`plugin-detail-tab-${next}`)?.focus()
  }

  const error = actionError ?? (detailQuery.error ? errorMessage(detailQuery.error) : null)

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) onClose()
      }}
      title={
        detail === undefined
          ? (pluginKey ?? '')
          : `${detail.plugin.name} ${detail.plugin.active_version ?? ''}`.trim()
      }
      description={detail === undefined ? undefined : detail.plugin.key}
      variant="sheet"
      panelClassName="!max-w-[calc(100vw-40px)] xl:!max-w-[720px]"
      footer={
        <>
          <Button
            variant="secondary"
            disabled={detail === undefined}
            onClick={() => {
              if (detail === undefined) return
              void navigator.clipboard?.writeText(detail.plugin.key)
              setNotice(t('admin.task_plugins.detail.copied'))
            }}
          >
            {t('admin.task_plugins.detail.copy_key')}
          </Button>
          <Button variant="secondary" onClick={onClose}>
            {t('admin.task_plugins.common.close')}
          </Button>
        </>
      }
    >
      {detailQuery.isPending ? (
        <div data-testid="plugin-detail-skeleton" className="space-y-2">
          {Array.from({ length: 6 }, (_, index) => (
            <div
              key={index}
              className="h-8 animate-pulse rounded-lg bg-surface-subtle"
            />
          ))}
        </div>
      ) : null}

      {detailQuery.isError ? (
        <ErrorState
          message={error ?? t('admin.task_plugins.detail.load_error')}
          onRetry={() => void detailQuery.refetch()}
        />
      ) : null}

      {detail !== undefined ? (
        <div>
          <div className="flex flex-wrap items-center gap-2">
            <SourceTag source={detail.plugin.source} />
            <RuntimeStatusBadge status={detail.plugin.runtime_status} />
          </div>

          <div
            role="tablist"
            aria-label={t('admin.task_plugins.detail.tabs_label')}
            className="mt-4 flex flex-wrap gap-1 border-b border-border-hairline"
            onKeyDown={handleTabKey}
          >
            {DETAIL_TABS.map((entry) => {
              const selected = entry === tab
              const count = entry === 'history' ? detail.versions.length : 0
              return (
                <button
                  key={entry}
                  id={`plugin-detail-tab-${entry}`}
                  type="button"
                  role="tab"
                  aria-selected={selected}
                  tabIndex={selected ? 0 : -1}
                  onClick={() => setTab(entry)}
                  className={[
                    'inline-flex h-9 items-center gap-1 rounded-t-lg px-2.5 text-meta',
                    selected
                      ? 'border-b-2 border-brand font-medium text-ink-1'
                      : 'text-ink-2 hover:bg-surface-hover hover:text-ink-1',
                  ].join(' ')}
                >
                  {t(TAB_LABEL_KEYS[entry])}
                  {count > 0 ? (
                    <span className="rounded-full bg-surface-subtle px-1.5 text-badge text-ink-3">
                      {count}
                    </span>
                  ) : null}
                </button>
              )
            })}
          </div>

          <div
            role="tabpanel"
            aria-labelledby={`plugin-detail-tab-${tab}`}
            className="mt-3"
          >
            {notice !== null ? (
              <p role="status" className="mb-2 text-meta text-success">
                {notice}
              </p>
            ) : null}
            {actionError !== null ? (
              <div className="mb-2">
                <FormAlert>{actionError}</FormAlert>
              </div>
            ) : null}

            {tab === 'metadata' ? (
              <dl className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <Field label={t('admin.task_plugins.detail.name')} value={detail.plugin.name} />
                <Field label={t('admin.task_plugins.detail.key')} value={detail.plugin.key} />
                <Field
                  label={t('admin.task_plugins.detail.active_version')}
                  value={detail.plugin.active_version ?? t('admin.task_plugins.detail.no_active')}
                />
                <Field
                  label={t('admin.task_plugins.detail.source')}
                  value={t(`admin.task_plugins.source.${detail.plugin.source}`)}
                />
                <Field
                  label={t('admin.task_plugins.detail.description')}
                  value={detail.plugin.description ?? '—'}
                />
                <Field
                  label={t('admin.task_plugins.detail.updated')}
                  value={formatRelativeTime(detail.plugin.updated_at, locale)}
                />
                <Field
                  label={t('admin.task_plugins.detail.bound_channels')}
                  value={String(detail.plugin.bound_channels)}
                />
                <Field
                  label={t('admin.task_plugins.detail.runtime_layer')}
                  value={detail.plugin.layer ?? '—'}
                />
              </dl>
            ) : null}

            {tab === 'endpoints' || tab === 'models' || tab === 'usage' ? (
              <p className="rounded-lg border border-dashed border-border-default px-4 py-8 text-center text-body text-ink-3">
                {t('admin.task_plugins.detail.not_exposed')}
              </p>
            ) : null}

            {tab === 'integrity' ? (
              <div>
                {detail.plugin.compile_error !== undefined &&
                detail.plugin.compile_error !== '' ? (
                  <div className="mb-3">
                    <FormAlert>
                      <span className="block font-medium">
                        {t('admin.task_plugins.upload.compile_failed_title', {
                          error: detail.plugin.compile_error,
                        })}
                      </span>
                      {detail.plugin.runtime_errors?.map((issue) => (
                        <span key={`${issue.key}-${issue.fieldPath}-${issue.message}`} className="mt-1 block font-mono text-meta">
                          {issue.fieldPath === '' ? issue.key : `${issue.fieldPath}: `}
                          {issue.message}
                        </span>
                      ))}
                    </FormAlert>
                  </div>
                ) : null}
                <dl className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  <Field
                    label={t('admin.task_plugins.detail.hash')}
                    value={activeVersionRow?.source_hash ?? '—'}
                  />
                  <Field
                    label={t('admin.task_plugins.detail.uploaded_at')}
                    value={
                      activeVersionRow === null
                        ? '—'
                        : formatRelativeTime(activeVersionRow.created_at, locale)
                    }
                  />
                  <Field
                    label={t('admin.task_plugins.detail.uploaded_by')}
                    value={
                      activeVersionRow === null
                        ? '—'
                        : `#${activeVersionRow.uploaded_by}`
                    }
                  />
                  <Field
                    label={t('admin.task_plugins.detail.source_version')}
                    value={detail.source_version ?? '—'}
                  />
                </dl>
                {detail.source_text !== undefined ? (
                  <SourceViewer
                    testId="plugin-source-viewer"
                    text={detail.source_text}
                  />
                ) : (
                  <p className="mt-3 text-body text-ink-3">
                    {t('admin.task_plugins.detail.no_source')}
                  </p>
                )}
              </div>
            ) : null}

            {tab === 'history' ? (
              <div>
                {detail.versions.length === 0 ? (
                  <p className="rounded-lg border border-dashed border-border-default px-4 py-8 text-center text-body text-ink-3">
                    {t('admin.task_plugins.detail.no_versions')}
                  </p>
                ) : (
                  <div className="overflow-x-auto rounded-xl border border-border-default">
                    <table className="w-full text-left text-body">
                      <thead className="bg-surface-subtle text-meta text-ink-3">
                        <tr>
                          <th className="px-3 py-2 font-medium">
                            {t('admin.task_plugins.detail.version')}
                          </th>
                          <th className="px-3 py-2 font-medium">
                            {t('admin.task_plugins.detail.uploaded_at')}
                          </th>
                          <th className="px-3 py-2 font-medium">
                            {t('admin.task_plugins.detail.hash')}
                          </th>
                          <th className="px-3 py-2 font-medium">
                            {t('admin.task_plugins.detail.actions')}
                          </th>
                        </tr>
                      </thead>
                      <tbody>
                        {detail.versions.map((version) => (
                          <tr
                            key={version.id}
                            data-testid="plugin-version-row"
                            className={[
                              'border-t border-border-hairline',
                              version.active
                                ? 'border-l-2 border-l-brand'
                                : '',
                            ].join(' ')}
                          >
                            <td className="px-3 py-2">
                              <span className="font-mono">{version.version}</span>{' '}
                              {version.active ? (
                                <Tag variant="brand">
                                  {t('admin.task_plugins.detail.current')}
                                </Tag>
                              ) : null}
                            </td>
                            <td className="px-3 py-2 text-meta text-ink-2">
                              {formatRelativeTime(version.created_at, locale)}
                            </td>
                            <td
                              className="px-3 py-2 font-mono text-meta"
                              title={version.source_hash}
                            >
                              {shortHash(version.source_hash)}
                            </td>
                            <td className="px-3 py-2">
                              <span className="flex flex-wrap gap-2">
                                {canManage && !version.active ? (
                                  <Button
                                    variant="ghost"
                                    className="h-7 px-2 text-badge"
                                    disabled={!online || activateMutation.isPending}
                                    onClick={() => {
                                      setActionError(null)
                                      setActivateTarget(version)
                                    }}
                                  >
                                    {t('admin.task_plugins.detail.activate')}
                                  </Button>
                                ) : null}
                                <Button
                                  variant="ghost"
                                  className="h-7 px-2 text-badge"
                                  onClick={() =>
                                    setCompareVersion((current) =>
                                      current === version.version ? null : version.version,
                                    )
                                  }
                                >
                                  {t('admin.task_plugins.detail.compare')}
                                </Button>
                                {canManage ? (
                                  <Button
                                    variant="ghost"
                                    className="h-7 px-2 text-badge"
                                    disabled={
                                      !online ||
                                      detail.versions.length <= 1 ||
                                      deleteMutation.isPending
                                    }
                                    title={
                                      detail.versions.length <= 1
                                        ? t('admin.task_plugins.detail.delete_unique')
                                        : undefined
                                    }
                                    onClick={() => {
                                      setActionError(null)
                                      setDeleteTarget(version)
                                    }}
                                  >
                                    {t('admin.task_plugins.detail.delete')}
                                  </Button>
                                ) : null}
                              </span>
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}

                {detail.versions.length <= 1 ? (
                  <p className="mt-2 text-meta text-ink-3">
                    {t('admin.task_plugins.detail.delete_unique')}
                  </p>
                ) : null}

                {compareVersion !== null ? (
                  <div>
                    {compareQuery.isPending ? (
                      <div className="mt-3 h-24 animate-pulse rounded-lg bg-surface-subtle" />
                    ) : null}
                    {compareQuery.isError ? (
                      <div className="mt-3">
                        <ErrorState
                          message={errorMessage(compareQuery.error)}
                          onRetry={() => void compareQuery.refetch()}
                        />
                      </div>
                    ) : null}
                    {compareQuery.data !== undefined ? (
                      <VersionDiff
                        leftVersion={compareQuery.data.version}
                        rightVersion={detail.source_version ?? activeVersion ?? ''}
                        leftText={compareQuery.data.source_text}
                        rightText={detail.source_text ?? ''}
                      />
                    ) : null}
                  </div>
                ) : null}
              </div>
            ) : null}
          </div>
        </div>
      ) : null}

      <ConfirmDialog
        open={activateTarget !== null}
        title={t('admin.task_plugins.detail.activate_title', {
          version: activateTarget?.version ?? '',
        })}
        confirmLabel={t('admin.task_plugins.detail.activate')}
        dangerous={false}
        confirmDisabled={activateMutation.isPending}
        onCancel={() => setActivateTarget(null)}
        onConfirm={() => {
          if (activateTarget !== null) activateMutation.mutate(activateTarget.version)
        }}
      >
        <p>{t('admin.task_plugins.detail.activate_body')}</p>
      </ConfirmDialog>

      <ConfirmDialog
        open={deleteTarget !== null}
        title={t('admin.task_plugins.detail.delete_title', {
          version: deleteTarget?.version ?? '',
        })}
        confirmLabel={t('admin.task_plugins.detail.delete')}
        confirmDisabled={deleteMutation.isPending}
        onCancel={() => setDeleteTarget(null)}
        onConfirm={() => {
          if (deleteTarget !== null) deleteMutation.mutate(deleteTarget.version)
        }}
      >
        <p>
          {deleteTarget?.active === true
            ? t('admin.task_plugins.detail.delete_active_body')
            : t('admin.task_plugins.detail.delete_body')}
        </p>
      </ConfirmDialog>
    </Dialog>
  )
}
