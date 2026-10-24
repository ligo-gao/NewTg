import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useDeferredValue, useEffect, useState } from 'react'

import { saveSettingsSection } from '@/features/admin/settings/api'
import { useI18n } from '@/shared/lib/i18n-context'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert } from '@/shared/ui/form'

import {
  createIdempotencyKey,
  listTaskPlugins,
  setTaskPluginStatus,
  type TaskPlugin,
  type TaskPluginPage,
  type TaskPluginRuntimeStatus,
} from './api'
import {
  ConfirmDialog,
  EmptyState,
  ErrorState,
  OfflineNote,
  Pagination,
  PluginIcon,
  RuntimeStatusBadge,
  SectionCard,
  SkeletonRows,
  SourceTag,
  Switch,
} from './components'
import { PluginDetailDrawer, type PluginDetailTab } from './detail-drawer'
import { notifyError, notifySuccess } from './notify'
import { RUNTIME_STATUSES, errorMessage } from './utils'

const PAGE_SIZE = 20

export interface InstalledTabProps {
  canManage: boolean
  online: boolean
  /** `plugin.disabled_factory_keys` 的当前值。 */
  factoryDisabledKeys: readonly string[]
  /** 设置 section 的乐观锁版本；保存禁用集时回传。 */
  factorySettingsUpdatedAt: string | null
  /** 设置保存后让父级失效 plugin 设置查询。 */
  onFactorySaved: () => void
  onGoRuntime: () => void
  onGoUpload: () => void
}

/**
 * 已安装 Tab（F-08-28 / F-08-32）：筛选 + 表格 + 异常优先 + offset 分页。
 *
 * 排序由服务端保证（`runtimeSeverity`：compile_failed → not_registered →
 * disabled_fallback → disabled → registered，其次名称），前端按响应顺序渲染，
 * 避免对「当前页」二次排序破坏跨页一致性。
 */
export function InstalledTab({
  canManage,
  online,
  factoryDisabledKeys,
  factorySettingsUpdatedAt,
  onFactorySaved,
  onGoRuntime,
  onGoUpload,
}: InstalledTabProps) {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()

  const [search, setSearch] = useState('')
  const deferredSearch = useDeferredValue(search)
  const [source, setSource] = useState<'' | 'builtin' | 'upload'>('')
  const [enabled, setEnabled] = useState<'' | 'true' | 'false'>('')
  const [runtimeStatus, setRuntimeStatus] = useState<'' | TaskPluginRuntimeStatus>('')
  const [page, setPage] = useState(1)

  const [drawer, setDrawer] = useState<{
    key: string
    tab: PluginDetailTab
  } | null>(null)
  const [factoryOpen, setFactoryOpen] = useState(false)
  const [disableTarget, setDisableTarget] = useState<TaskPlugin | null>(null)

  const filters = {
    search: deferredSearch.trim() === '' ? undefined : deferredSearch.trim(),
    source: source === '' ? undefined : source,
    enabled: enabled === '' ? undefined : enabled === 'true',
    runtimeStatus: runtimeStatus === '' ? undefined : runtimeStatus,
  }
  const filtered =
    filters.search !== undefined ||
    filters.source !== undefined ||
    filters.enabled !== undefined ||
    filters.runtimeStatus !== undefined

  const listQueryKey = ['plugin-list', filters, page] as const
  const listQuery = useQuery({
    queryKey: listQueryKey,
    queryFn: ({ signal }) =>
      listTaskPlugins(
        { ...filters, page, pageSize: PAGE_SIZE },
        signal,
      ),
    placeholderData: (previous) => previous,
  })
  const data: TaskPluginPage | undefined = listQuery.data

  const statusMutation = useMutation({
    mutationFn: (target: TaskPlugin) =>
      setTaskPluginStatus(target.key, !target.enabled, target.updated_at),
    onMutate: async (target) => {
      const key = listQueryKey
      await queryClient.cancelQueries({ queryKey: key })
      const previous = queryClient.getQueryData<TaskPluginPage>(key)
      if (previous !== undefined) {
        queryClient.setQueryData<TaskPluginPage>(key, {
          ...previous,
          items: previous.items.map((item) =>
            item.key === target.key ? { ...item, enabled: !item.enabled } : item,
          ),
        })
      }
      return { previous, key }
    },
    onError: (error, _target, context) => {
      // 失败回滚到快照，避免界面显示一个服务端并未接受的开关状态。
      if (context?.previous !== undefined) {
        queryClient.setQueryData(context.key, context.previous)
      }
      notifyError(error)
    },
    onSuccess: (result) => {
      notifySuccess(
        t(
          result.enabled
            ? 'admin.task_plugins.installed.toggle_enabled'
            : 'admin.task_plugins.installed.toggle_disabled',
        ),
      )
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: ['plugin-list'] })
      void queryClient.invalidateQueries({ queryKey: ['plugin-runtime'] })
    },
  })

  function requestToggle(target: TaskPlugin): void {
    // 禁用「被渠道绑定」的插件是不可逆面更大的操作；当前 bound_channels 恒 0，
    // 因此确认分支实际不可达（绑定列由 T7 落地，已登记）。
    if (target.enabled && target.bound_channels > 0) {
      setDisableTarget(target)
      return
    }
    statusMutation.mutate(target)
  }

  const total = data?.total ?? 0

  // 「注册表未加载」只能在内置层全员缺席时使用；只有探测到内置插件存在
  // （但列表仍为空）才显示普通的「尚无已安装插件」。探测仅在必要时发起。
  const registryProbe = useQuery({
    queryKey: ['plugin-list', 'builtin-probe'],
    queryFn: ({ signal }) =>
      listTaskPlugins({ source: 'builtin', page: 1, pageSize: 1 }, signal),
    enabled: data !== undefined && !filtered && total === 0,
  })
  const builtinMissing =
    registryProbe.data !== undefined && registryProbe.data.total === 0
  const showRegistryEmpty =
    data !== undefined && !filtered && total === 0 && builtinMissing
  const showInstalledEmpty =
    data !== undefined &&
    !filtered &&
    total === 0 &&
    registryProbe.data !== undefined &&
    registryProbe.data.total > 0
  const showFilteredEmpty = data !== undefined && filtered && total === 0

  return (
    <div className="space-y-3">
      {factoryDisabledKeys.length > 0 ? (
        <div
          data-testid="factory-disabled-banner"
          className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-warning-line bg-warning-bg px-3 py-2 text-body text-warning"
        >
          <span>
            {t('admin.task_plugins.installed.factory_banner', {
              n: factoryDisabledKeys.length,
            })}
          </span>
          {canManage ? (
            <Button
              variant="ghost"
              className="h-7 px-2 text-badge"
              onClick={() => setFactoryOpen(true)}
            >
              {t('admin.task_plugins.installed.factory_manage')}
            </Button>
          ) : null}
        </div>
      ) : null}

      <SectionCard>
        <div className="flex flex-wrap items-end gap-2">
          <label className="flex flex-col gap-1 text-meta text-ink-3">
            {t('admin.task_plugins.installed.search')}
            <input
              type="search"
              value={search}
              onChange={(event) => {
                setSearch(event.target.value)
                setPage(1)
              }}
              placeholder={t('admin.task_plugins.installed.search_placeholder')}
              className="h-9 w-56 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            />
          </label>
          <label className="flex flex-col gap-1 text-meta text-ink-3">
            {t('admin.task_plugins.installed.filter_source')}
            <select
              value={source}
              onChange={(event) => {
                setSource(event.target.value as typeof source)
                setPage(1)
              }}
              className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1"
            >
              <option value="">{t('admin.task_plugins.installed.all_sources')}</option>
              <option value="builtin">{t('admin.task_plugins.source.builtin')}</option>
              <option value="upload">{t('admin.task_plugins.source.upload')}</option>
            </select>
          </label>
          <label className="flex flex-col gap-1 text-meta text-ink-3">
            {t('admin.task_plugins.installed.filter_enabled')}
            <select
              value={enabled}
              onChange={(event) => {
                setEnabled(event.target.value as typeof enabled)
                setPage(1)
              }}
              className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1"
            >
              <option value="">{t('admin.task_plugins.installed.all_enabled')}</option>
              <option value="true">{t('admin.task_plugins.installed.enabled_only')}</option>
              <option value="false">{t('admin.task_plugins.installed.disabled_only')}</option>
            </select>
          </label>
          <label className="flex flex-col gap-1 text-meta text-ink-3">
            {t('admin.task_plugins.installed.filter_runtime')}
            <select
              value={runtimeStatus}
              onChange={(event) => {
                setRuntimeStatus(event.target.value as typeof runtimeStatus)
                setPage(1)
              }}
              className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1"
            >
              <option value="">{t('admin.task_plugins.installed.all_runtime')}</option>
              {RUNTIME_STATUSES.map((status) => (
                <option key={status} value={status}>
                  {t(`admin.task_plugins.status.${status}`)}
                </option>
              ))}
            </select>
          </label>
          {canManage ? (
            <Button
              variant="secondary"
              className="ml-auto"
              onClick={() => setFactoryOpen(true)}
            >
              {t('admin.task_plugins.installed.factory_open')}
            </Button>
          ) : null}
        </div>

        <div className="mt-3 space-y-2">
          {listQuery.isPending ? <SkeletonRows rows={5} /> : null}

          {listQuery.isError ? (
            <ErrorState
              message={errorMessage(listQuery.error)}
              onRetry={() => void listQuery.refetch()}
            />
          ) : null}

          {data !== undefined && data.items.length > 0 ? (
            <div className="overflow-x-auto rounded-xl border border-border-default">
              <table className="w-full text-left text-body">
                <thead className="bg-surface-subtle text-meta text-ink-3">
                  <tr>
                    <th className="px-3 py-2 font-medium">
                      {t('admin.task_plugins.installed.col_plugin')}
                    </th>
                    <th className="px-3 py-2 font-medium">
                      {t('admin.task_plugins.installed.col_source')}
                    </th>
                    <th className="px-3 py-2 font-medium">
                      {t('admin.task_plugins.installed.col_version')}
                    </th>
                    <th className="px-3 py-2 font-medium">
                      {t('admin.task_plugins.installed.col_enabled')}
                    </th>
                    <th className="px-3 py-2 font-medium">
                      {t('admin.task_plugins.installed.col_status')}
                    </th>
                    <th className="hidden px-3 py-2 font-medium xl:table-cell">
                      {t('admin.task_plugins.installed.col_bound')}
                    </th>
                    <th className="hidden px-3 py-2 font-medium xl:table-cell">
                      {t('admin.task_plugins.installed.col_updated')}
                    </th>
                    <th className="px-3 py-2 font-medium">
                      {t('admin.task_plugins.installed.col_actions')}
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {data.items.map((plugin) => (
                    <tr
                      key={plugin.key}
                      data-testid="plugin-row"
                      className="h-14 border-t border-border-hairline"
                    >
                      <td className="px-3 py-2">
                        <span className="flex items-center gap-2">
                          <PluginIcon size="sm" icon={plugin.icon} pluginKey={plugin.key} />
                          <span className="flex flex-col">
                            <span className="font-medium text-ink-1">{plugin.name}</span>
                            <span className="font-mono text-meta text-ink-3">
                              {plugin.key}
                            </span>
                          </span>
                        </span>
                      </td>
                      <td className="px-3 py-2">
                        <SourceTag source={plugin.source} />
                      </td>
                      <td className="px-3 py-2 font-mono text-meta">
                        {plugin.active_version ?? '—'}
                      </td>
                      <td className="px-3 py-2">
                        {canManage ? (
                          <Switch
                            checked={plugin.enabled}
                            disabled={!online || statusMutation.isPending}
                            label={t('admin.task_plugins.installed.toggle_label', {
                              name: plugin.name,
                            })}
                            onChange={() => requestToggle(plugin)}
                          />
                        ) : (
                          <span className="text-meta text-ink-2">
                            {t(
                              plugin.enabled
                                ? 'admin.task_plugins.installed.enabled_yes'
                                : 'admin.task_plugins.installed.enabled_no',
                            )}
                          </span>
                        )}
                      </td>
                      <td className="px-3 py-2">
                        <RuntimeStatusBadge status={plugin.runtime_status} />
                      </td>
                      <td className="hidden px-3 py-2 text-meta text-ink-2 xl:table-cell">
                        <span title={t('admin.task_plugins.installed.bound_hint')}>
                          {plugin.bound_channels}
                        </span>
                      </td>
                      <td className="hidden px-3 py-2 text-meta text-ink-2 xl:table-cell">
                        {formatRelativeTime(plugin.updated_at, locale)}
                      </td>
                      <td className="px-3 py-2">
                        <span className="flex flex-wrap gap-1">
                          <Button
                            variant="ghost"
                            className="h-7 px-2 text-badge"
                            onClick={() =>
                              setDrawer({ key: plugin.key, tab: 'metadata' })
                            }
                          >
                            {t('admin.task_plugins.installed.detail')}
                          </Button>
                          {plugin.runtime_status === 'compile_failed' ? (
                            <Button
                              variant="ghost"
                              className="h-7 px-2 text-badge text-danger"
                              onClick={() =>
                                setDrawer({ key: plugin.key, tab: 'integrity' })
                              }
                            >
                              {t('admin.task_plugins.installed.view_error')}
                            </Button>
                          ) : null}
                        </span>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : null}

          {data !== undefined &&
          total === 0 &&
          !filtered &&
          registryProbe.isPending ? (
            <SkeletonRows rows={5} />
          ) : null}

          {showInstalledEmpty ? (
            <EmptyState
              testId="plugin-empty-installed"
              title={t('admin.task_plugins.installed.empty_installed')}
              action={
                canManage ? (
                  <Button onClick={onGoUpload}>
                    {t('admin.task_plugins.page.upload_shortcut')}
                  </Button>
                ) : undefined
              }
            />
          ) : null}

          {showRegistryEmpty ? (
            <EmptyState
              testId="plugin-empty-registry"
              title={t('admin.task_plugins.installed.empty_registry')}
              action={
                <Button variant="secondary" onClick={onGoRuntime}>
                  {t('admin.task_plugins.installed.go_runtime')}
                </Button>
              }
            />
          ) : null}

          {showFilteredEmpty ? (
            <EmptyState
              testId="plugin-empty-filtered"
              title={t('admin.task_plugins.installed.empty_filtered')}
              action={
                canManage ? (
                  <Button variant="primary" onClick={onGoUpload}>
                    {t('admin.task_plugins.page.upload_shortcut')}
                  </Button>
                ) : undefined
              }
            />
          ) : null}

          {data !== undefined && data.items.length > 0 ? (
            <Pagination
              page={page}
              pageSize={PAGE_SIZE}
              total={total}
              onPageChange={setPage}
            />
          ) : null}

          {/* 绑定列尚未落地：可见说明代替把「恒 0」藏进 tooltip。 */}
          <p className="text-meta text-ink-3">
            {t('admin.task_plugins.installed.bound_hint')}
          </p>
        </div>

        <OfflineNote visible={!online} />
      </SectionCard>

      <PluginDetailDrawer
        open={drawer !== null}
        pluginKey={drawer?.key ?? null}
        initialTab={drawer?.tab ?? 'metadata'}
        canManage={canManage}
        online={online}
        onClose={() => setDrawer(null)}
        onChanged={() => undefined}
      />

      <ConfirmDialog
        open={disableTarget !== null}
        title={t('admin.task_plugins.installed.disable_bound_title', {
          name: disableTarget?.name ?? '',
        })}
        confirmLabel={t('admin.task_plugins.installed.disable_bound_confirm')}
        onCancel={() => setDisableTarget(null)}
        onConfirm={() => {
          if (disableTarget !== null) statusMutation.mutate(disableTarget)
          setDisableTarget(null)
        }}
      >
        <p>
          {t('admin.task_plugins.installed.disable_bound_body', {
            n: disableTarget?.bound_channels ?? 0,
          })}
        </p>
      </ConfirmDialog>

      <FactoryDisableDialog
        open={factoryOpen}
        canManage={canManage}
        online={online}
        disabledKeys={factoryDisabledKeys}
        expectUpdatedAt={factorySettingsUpdatedAt}
        onClose={() => setFactoryOpen(false)}
        onSaved={() => {
          setFactoryOpen(false)
          onFactorySaved()
          void queryClient.invalidateQueries({ queryKey: ['plugin-list'] })
          void queryClient.invalidateQueries({ queryKey: ['plugin-runtime'] })
        }}
      />
    </div>
  )
}

/**
 * 内置插件全局禁用管理（F-08-32）。
 * 写的是 `plugin.disabled_factory_keys`（settings section `plugin`，整组校验
 * + 单事务 + 广播）。保存前弹危险确认：命中 key 的 factory 层在所有实例立即
 * 不再注册，有上传覆盖层时回退（`disabled_fallback`），在途任务不受影响。
 */
function FactoryDisableDialog({
  open,
  canManage,
  online,
  disabledKeys,
  expectUpdatedAt,
  onClose,
  onSaved,
}: {
  open: boolean
  canManage: boolean
  online: boolean
  disabledKeys: readonly string[]
  expectUpdatedAt: string | null
  onClose: () => void
  onSaved: () => void
}) {
  const { t } = useI18n()
  const [selected, setSelected] = useState<string[]>([])
  const [confirming, setConfirming] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // 每次打开都以服务端当前值为准，避免上一次未保存的勾选残留。
  useEffect(() => {
    if (open) {
      setSelected([...disabledKeys].sort())
      setError(null)
      setConfirming(false)
    }
  }, [open, disabledKeys])

  const builtinQuery = useQuery({
    queryKey: ['plugin-list', 'builtin-all'],
    queryFn: ({ signal }) =>
      listTaskPlugins({ source: 'builtin', page: 1, pageSize: 100 }, signal),
    enabled: open,
  })

  const saveMutation = useMutation({
    mutationFn: (keys: string[]) =>
      saveSettingsSection(
        {
          section: 'plugin',
          values: { 'plugin.disabled_factory_keys': keys },
          expect_updated_at: expectUpdatedAt,
          dangerous_confirmed: true,
        },
        createIdempotencyKey(),
      ),
    onSuccess: () => {
      setConfirming(false)
      onSaved()
    },
    onError: (mutationError) => {
      setConfirming(false)
      setError(errorMessage(mutationError))
    },
  })

  // 回退层是否存在的唯一可观察信号是运行时的 disabled_fallback 状态；
  // 未激活过的新选择没有其它真源可查。
  const hasFallback =
    builtinQuery.data?.items.some(
      (plugin) =>
        selected.includes(plugin.key) &&
        plugin.runtime_status === 'disabled_fallback',
    ) ?? false

  function toggleKey(key: string): void {
    setSelected((current) =>
      current.includes(key)
        ? current.filter((entry) => entry !== key)
        : [...current, key].sort(),
    )
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) onClose()
      }}
      title={t('admin.task_plugins.factory.title')}
      description={t('admin.task_plugins.factory.description')}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            {t('admin.task_plugins.common.cancel')}
          </Button>
          <Button
            variant="danger"
            disabled={!canManage || !online || saveMutation.isPending}
            onClick={() => {
              setError(null)
              setConfirming(true)
            }}
          >
            {t('admin.task_plugins.factory.save')}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        {builtinQuery.isPending ? <SkeletonRows rows={3} /> : null}
        {builtinQuery.isError ? (
          <ErrorState
            message={errorMessage(builtinQuery.error)}
            onRetry={() => void builtinQuery.refetch()}
          />
        ) : null}
        {builtinQuery.data !== undefined ? (
          <ul className="space-y-1">
            {builtinQuery.data.items.map((plugin) => (
              <li key={plugin.key} className="flex items-center gap-2">
                <input
                  id={`factory-disable-${plugin.key}`}
                  type="checkbox"
                  checked={selected.includes(plugin.key)}
                  onChange={() => toggleKey(plugin.key)}
                />
                <label htmlFor={`factory-disable-${plugin.key}`} className="text-body text-ink-1">
                  {plugin.name}{' '}
                  <span className="font-mono text-meta text-ink-3">{plugin.key}</span>
                </label>
                {plugin.runtime_status === 'disabled_fallback' ? (
                  <span className="text-meta text-warning">
                    {t('admin.task_plugins.factory.has_override')}
                  </span>
                ) : null}
              </li>
            ))}
          </ul>
        ) : null}
        {error !== null ? <FormAlert>{error}</FormAlert> : null}

        <ConfirmDialog
          open={confirming}
          title={t('admin.task_plugins.factory.confirm_title')}
          confirmLabel={t('admin.task_plugins.factory.confirm_label')}
          confirmDisabled={saveMutation.isPending}
          onCancel={() => setConfirming(false)}
          onConfirm={() => saveMutation.mutate(selected)}
        >
          <p>
            {t(
              hasFallback
                ? 'admin.task_plugins.factory.confirm_body'
                : 'admin.task_plugins.factory.confirm_body_no_override',
            )}
          </p>
        </ConfirmDialog>
      </div>
    </Dialog>
  )
}
