import { useQuery } from '@tanstack/react-query'

import { useI18n } from '@/shared/lib/i18n-context'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { Button } from '@/shared/ui/button'
import { StatusBadge } from '@/shared/ui/status-badge'

import { getTaskPluginRuntimeStatus } from './api'
import { ErrorState, PluginIcon, RuntimeStatusBadge } from './components'
import { errorMessage } from './utils'

/**
 * 运行时状态 Tab（F-08-36）。
 *
 * **不自动轮询**：仅「刷新」按钮手动拉取（避免多管理员同时高频拉取）。
 * 逐副本聚合需要实例心跳表；当前端点只返回本副本单节点对象，因此这里按
 * 「一个副本 = 一张卡」渲染，并常驻显示「generation 为节点本地号，跨节点
 * 比较请用 DB revision」。失联三色阈值（30s / 5min，与 F-08-21 同口径）
 * 依赖心跳，当前无真源可算——已登记，不用 `reloaded_at` 冒充「最后在线」。
 */
export function RuntimeTab() {
  const { t, locale } = useI18n()

  const statusQuery = useQuery({
    queryKey: ['plugin-runtime'],
    queryFn: ({ signal }) => getTaskPluginRuntimeStatus(signal),
    // 手动刷新模型：缓存永不过期，挂载/聚焦都不会隐式重拉。
    staleTime: Infinity,
    refetchOnWindowFocus: false,
  })
  const status = statusQuery.data
  const errorPlugins = (status?.plugins ?? []).filter(
    (plugin) => (plugin.errors?.length ?? 0) > 0,
  )

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <Button
          variant="secondary"
          disabled={statusQuery.isFetching}
          onClick={() => void statusQuery.refetch()}
        >
          {statusQuery.isFetching
            ? t('admin.task_plugins.common.loading')
            : t('admin.task_plugins.runtime.refresh')}
        </Button>
        {statusQuery.dataUpdatedAt > 0 ? (
          <span className="text-xs text-ink-3" data-testid="runtime-refreshed-at">
            {t('admin.task_plugins.runtime.last_refreshed', {
              time: formatRelativeTime(
                new Date(statusQuery.dataUpdatedAt).toISOString(),
                locale,
              ),
            })}
          </span>
        ) : null}
      </div>

      {statusQuery.isPending ? (
        <div className="grid gap-3 lg:grid-cols-2">
          {Array.from({ length: 2 }, (_, index) => (
            <div
              key={index}
              className="h-40 animate-pulse rounded-xl border border-border-hairline bg-surface-subtle"
            />
          ))}
        </div>
      ) : null}

      {statusQuery.isError ? (
        <ErrorState
          message={errorMessage(statusQuery.error)}
          onRetry={() => void statusQuery.refetch()}
        />
      ) : null}

      {status !== undefined ? (
        <section
          data-testid="runtime-node-card"
          className="rounded-xl border border-border-default bg-surface-card p-5"
        >
          <header className="flex flex-wrap items-center gap-3">
            <h2 className="font-mono text-sm font-semibold text-ink-1">
              {status.node}
            </h2>
            <StatusBadge
              variant={status.reload_error ? 'danger' : 'success'}
            >
              {status.reload_error
                ? t('admin.task_plugins.runtime.rebuild_failed')
                : t('admin.task_plugins.runtime.rebuild_ok')}
            </StatusBadge>
            <span className="text-xs text-ink-2">
              {t('admin.task_plugins.runtime.generation', {
                n: status.generation,
              })}
            </span>
            <span className="text-xs text-ink-2">
              {t('admin.task_plugins.runtime.reloaded', {
                time: formatRelativeTime(status.reloaded_at, locale),
              })}
            </span>
          </header>

          <p className="mt-1 font-mono text-xs text-ink-3">
            revision {status.revision}
          </p>

          {status.reload_error !== undefined && status.reload_error !== '' ? (
            <p className="mt-2 whitespace-pre-wrap rounded bg-danger-bg px-2 py-1 font-mono text-xs text-danger">
              {status.reload_error}
            </p>
          ) : null}

          <div className="mt-3 border-t border-border-hairline pt-3">
            {errorPlugins.length === 0 ? (
              <p className="text-xs text-ink-3">
                {t('admin.task_plugins.runtime.no_errors')}
              </p>
            ) : (
              <ul className="space-y-2">
                {errorPlugins.map((plugin) => (
                  <li
                    key={plugin.key}
                    data-testid="runtime-plugin-error"
                    className="rounded-lg border border-danger-line bg-danger-bg px-2 py-1.5"
                  >
                    <div className="flex flex-wrap items-center gap-2">
                      <PluginIcon size="sm" pluginKey={plugin.key} />
                      <span className="font-mono text-xs font-medium text-ink-1">
                        {plugin.key}
                      </span>
                      <RuntimeStatusBadge status={plugin.status} />
                      {plugin.version !== undefined ? (
                        <span className="font-mono text-xs text-ink-3">
                          v{plugin.version}
                        </span>
                      ) : null}
                    </div>
                    <ul className="mt-1 space-y-0.5">
                      {(plugin.errors ?? []).map((issue) => (
                        <li
                          key={`${issue.fieldPath}-${issue.rule}-${issue.message}`}
                          className="font-mono text-xs text-danger"
                        >
                          {issue.fieldPath === '' ? issue.key : `${issue.fieldPath}: `}
                          {issue.message}
                        </li>
                      ))}
                    </ul>
                  </li>
                ))}
              </ul>
            )}
          </div>

          {/* 常驻口径提示：跨节点比较 generation 没有意义。 */}
          <p className="mt-3 text-xs text-ink-3">
            {t('admin.task_plugins.runtime.generation_hint')}
          </p>
        </section>
      ) : null}
    </div>
  )
}
