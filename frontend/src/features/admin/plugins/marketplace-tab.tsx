import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Tag } from '@/shared/ui/tag'

import {
  getPluginMarketplace,
  type MarketplacePlugin,
  type MarketplaceSource,
} from './api'
import { EmptyState, ErrorState, PluginIcon } from './components'
import { InstallDialog } from './install-dialog'
import { MarketplaceSourcesDrawer } from './marketplace-sources-drawer'
import { errorMessage, installState, problemSlug } from './utils'

export interface MarketplaceTabProps {
  canManage: boolean
  online: boolean
  /** `plugin.marketplace.enabled`；关闭时市场 Tab 不渲染（§6.10）。 */
  enabled: boolean
}

/**
 * 市场 Tab（F-08-33 / F-08-34）。
 *
 * 索引来自已启用源，服务端缓存 5 分钟；「刷新索引」显式绕过缓存。多源同
 * key+version 不同 hash 时服务端标 `conflict`，卡片只提示不猜权威源。
 * 未配置任何启用源 → 空态 + 打开市场源抽屉的入口。
 */
export function MarketplaceTab({ canManage, online, enabled }: MarketplaceTabProps) {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const [sourceFilter, setSourceFilter] = useState('')
  const [installTarget, setInstallTarget] = useState<MarketplacePlugin | null>(null)
  const [sourcesOpen, setSourcesOpen] = useState(false)

  const marketQuery = useQuery({
    queryKey: ['plugin-marketplace'],
    queryFn: ({ signal }) => getPluginMarketplace(false, signal),
    retry: false,
    enabled,
  })

  const refreshMutation = useMutation({
    mutationFn: () => getPluginMarketplace(true),
    onSuccess: (data) => {
      queryClient.setQueryData(['plugin-marketplace'], data)
      void queryClient.invalidateQueries({ queryKey: ['plugin-sources'] })
    },
  })

  if (!enabled) {
    return (
      <EmptyState
        testId="plugin-marketplace-disabled"
        title={t('admin.task_plugins.marketplace.disabled')}
      />
    )
  }

  const home = marketQuery.data
  const disabledByServer =
    marketQuery.isError && problemSlug(marketQuery.error) === 'plugin_marketplace_disabled'

  if (disabledByServer) {
    return (
      <EmptyState
        testId="plugin-marketplace-disabled"
        title={t('admin.task_plugins.marketplace.disabled')}
      />
    )
  }

  const sources: MarketplaceSource[] = home?.sources ?? []
  const enabledSources = sources.filter((source) => source.enabled)
  const plugins = (home?.plugins ?? []).filter((plugin) => {
    if (sourceFilter === '') return true
    return plugin.versions.some((version) => version.source_id === sourceFilter)
  })
  const sorted = [...plugins].sort((a, b) => a.name.localeCompare(b.name, locale))

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <label className="flex items-center gap-2 text-xs text-ink-3">
          {t('admin.task_plugins.marketplace.source_filter')}
          <select
            value={sourceFilter}
            onChange={(event) => setSourceFilter(event.target.value)}
            className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
          >
            <option value="">{t('admin.task_plugins.marketplace.all_sources')}</option>
            {sources.map((source) => (
              <option key={source.id} value={source.id}>
                {source.name}
              </option>
            ))}
          </select>
        </label>
        <Button
          variant="secondary"
          disabled={refreshMutation.isPending}
          onClick={() => refreshMutation.mutate()}
        >
          {refreshMutation.isPending
            ? t('admin.task_plugins.common.loading')
            : t('admin.task_plugins.marketplace.refresh')}
        </Button>
        <Button
          variant="secondary"
          disabled={!canManage}
          onClick={() => setSourcesOpen(true)}
        >
          {t('admin.task_plugins.marketplace.manage_sources')}
        </Button>
        {home !== undefined ? (
          <span className="ml-auto text-xs text-ink-3">
            {t('admin.task_plugins.marketplace.cached_at', {
              time: new Date(home.cached_at).toLocaleTimeString(locale),
            })}
          </span>
        ) : null}
      </div>

      {refreshMutation.isError ? (
        <ErrorState
          message={
            problemSlug(refreshMutation.error) === 'plugin_marketplace_disabled'
              ? t('admin.task_plugins.marketplace.disabled')
              : `${t('admin.task_plugins.marketplace.unreachable')} ${errorMessage(refreshMutation.error)}`
          }
          onRetry={() => refreshMutation.mutate()}
        />
      ) : null}

      {marketQuery.isPending ? (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
          {Array.from({ length: 8 }, (_, index) => (
            <div
              key={index}
              className="h-[168px] animate-pulse rounded-xl border border-border-hairline bg-surface-subtle"
            />
          ))}
        </div>
      ) : null}

      {marketQuery.isError && !disabledByServer ? (
        <div className="space-y-2">
          <ErrorState
            message={`${t('admin.task_plugins.marketplace.unreachable')} ${errorMessage(marketQuery.error)}`}
            onRetry={() => void marketQuery.refetch()}
          />
          <Button variant="secondary" onClick={() => setSourcesOpen(true)}>
            {t('admin.task_plugins.marketplace.manage_sources')}
          </Button>
        </div>
      ) : null}

      {home !== undefined && enabledSources.length === 0 ? (
        <EmptyState
          testId="plugin-marketplace-empty-sources"
          title={t('admin.task_plugins.marketplace.empty_sources')}
          action={
            <Button variant="secondary" onClick={() => setSourcesOpen(true)}>
              {t('admin.task_plugins.marketplace.manage_sources')}
            </Button>
          }
        />
      ) : null}

      {home !== undefined && enabledSources.length > 0 && sorted.length === 0 ? (
        <EmptyState title={t('admin.task_plugins.marketplace.empty_plugins')} />
      ) : null}

      {sorted.length > 0 ? (
        <ul className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
          {sorted.map((plugin) => {
            const state = installState(plugin)
            const conflict = plugin.conflicts !== undefined && plugin.conflicts.length > 0
            return (
              <li
                key={plugin.key}
                data-testid="marketplace-card"
                className="flex h-[168px] flex-col rounded-xl border border-border-default bg-surface-card p-3"
              >
                <div className="flex items-start gap-2">
                  <PluginIcon icon={null} pluginKey={plugin.key} />
                  <div className="min-w-0">
                    <p className="truncate font-medium text-ink-1">{plugin.name}</p>
                    <p className="truncate font-mono text-xs text-ink-3">{plugin.key}</p>
                  </div>
                </div>
                <p className="mt-1 line-clamp-2 text-xs text-ink-2">
                  {plugin.description}
                </p>
                <div className="mt-auto flex items-center justify-between gap-2">
                  <span className="font-mono text-xs text-ink-2">
                    v{plugin.latest_version}
                  </span>
                  <span className="flex items-center gap-1">
                    {conflict ? (
                      <Tag variant="warning">
                        {t('admin.task_plugins.marketplace.conflict')}
                      </Tag>
                    ) : null}
                    {state === 'installed' ? (
                      <Tag variant="neutral">
                        {t('admin.task_plugins.marketplace.installed')}
                      </Tag>
                    ) : null}
                    {state === 'upgrade' ? (
                      <Tag variant="warning">
                        {t('admin.task_plugins.marketplace.upgrade')}
                      </Tag>
                    ) : null}
                    {state !== 'installed' ? (
                      <Button
                        className="h-7 px-2 text-xs"
                        disabled={!canManage || !online}
                        onClick={() => {
                          setInstallTarget(plugin)
                        }}
                      >
                        {state === 'upgrade'
                          ? t('admin.task_plugins.marketplace.upgrade_action')
                          : t('admin.task_plugins.marketplace.install')}
                      </Button>
                    ) : null}
                  </span>
                </div>
              </li>
            )
          })}
        </ul>
      ) : null}

      <InstallDialog
        plugin={installTarget}
        canManage={canManage}
        online={online}
        onClose={() => setInstallTarget(null)}
      />

      <MarketplaceSourcesDrawer
        open={sourcesOpen}
        canManage={canManage}
        online={online}
        onClose={() => setSourcesOpen(false)}
        onChanged={() => {
          void marketQuery.refetch()
        }}
      />
    </div>
  )
}
