import { useQuery } from '@tanstack/react-query'

import { useI18n } from '@/shared/lib/i18n-context'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { Button } from '@/shared/ui/button'
import { StatusBadge } from '@/shared/ui/status-badge'

import { getTaskPluginRuntimeStatus } from './api'
import type { PluginRuntimeInstance } from './api'
import { ErrorState, PluginIcon, RuntimeStatusBadge } from './components'
import { errorMessage } from './utils'

/**
 * 运行时状态 Tab（F-08-36 / G-M6D-05/21）。
 *
 * **不自动轮询**：仅「刷新」按钮手动拉取（避免多管理员同时高频拉取）。
 * 响应是逐副本聚合：每个未过期实例一张卡（后端按 instance_id 升序），本地
 * 副本永远包含。Redis 不可用时后端降级为「仅本副本」并置
 * aggregate_unavailable=true，这里显示常驻提示而不是错误态——本副本的
 * 诊断信息仍然有效。
 *
 * generation 是节点本地号，跨节点比较须用 DB revision（relay-06 §2.5）；
 * 该口径在页面常驻显示，避免管理员把不同副本的 generation 当同一序列。
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
  const aggregate = statusQuery.data
  const instances = aggregate?.instances ?? []

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
          <span className="text-meta text-ink-3" data-testid="runtime-refreshed-at">
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

      {aggregate?.aggregate_unavailable === true ? (
        <p
          data-testid="runtime-aggregate-unavailable"
          className="rounded-lg border border-warning-line bg-warning-bg px-3 py-2 text-meta text-warning"
        >
          {t('admin.task_plugins.runtime.aggregate_unavailable')}
        </p>
      ) : null}

      {aggregate !== undefined && instances.length === 0 ? (
        <p className="rounded-xl border border-border-hairline bg-surface-subtle px-4 py-6 text-center text-body text-ink-3">
          {t('admin.task_plugins.runtime.empty_replicas')}
        </p>
      ) : null}

      {instances.map((instance) => (
        <RuntimeInstanceCard key={instance.instance_id} instance={instance} />
      ))}

      {instances.length > 0 ? (
        <p className="text-meta text-ink-3">
          {t('admin.task_plugins.runtime.generation_hint')}
        </p>
      ) : null}
    </div>
  )
}

function RuntimeInstanceCard({ instance }: { instance: PluginRuntimeInstance }) {
  const { t, locale } = useI18n()
  const errorPlugins = instance.plugins.filter(
    (plugin) => (plugin.errors?.length ?? 0) > 0,
  )

  return (
    <section
      data-testid="runtime-node-card"
      data-instance-id={instance.instance_id}
      className="rounded-xl border border-border-default bg-surface-card p-5"
    >
      <header className="flex flex-wrap items-center gap-3">
        <h2 className="font-mono text-body font-semibold text-ink-1">
          {instance.instance_id}
        </h2>
        {instance.node !== '' && instance.node !== instance.instance_id ? (
          <span className="font-mono text-meta text-ink-3">{instance.node}</span>
        ) : null}
        {instance.local ? (
          <span
            data-testid="runtime-local-badge"
            className="rounded-full border border-border-default px-2 py-0.5 text-badge text-ink-2"
          >
            {t('admin.task_plugins.runtime.local_replica')}
          </span>
        ) : null}
        <span className="font-mono text-meta text-ink-3">
          {instance.role} · v{instance.version}
        </span>
        <StatusBadge variant={instance.reload_error ? 'danger' : 'success'}>
          {instance.reload_error
            ? t('admin.task_plugins.runtime.rebuild_failed')
            : t('admin.task_plugins.runtime.rebuild_ok')}
        </StatusBadge>
        <span className="text-meta text-ink-2">
          {t('admin.task_plugins.runtime.generation', { n: instance.generation })}
        </span>
        <span className="text-meta text-ink-2">
          {t('admin.task_plugins.runtime.reloaded', {
            time: formatRelativeTime(instance.reloaded_at, locale),
          })}
        </span>
        <span className="text-meta text-ink-3">
          {t('admin.task_plugins.runtime.last_seen', {
            time: formatRelativeTime(instance.last_seen, locale),
          })}
        </span>
      </header>

      <p className="mt-1 font-mono text-meta text-ink-3">
        revision {instance.revision}
      </p>

      {instance.reload_error !== undefined && instance.reload_error !== '' ? (
        <p className="mt-2 whitespace-pre-wrap rounded bg-danger-bg px-2 py-1 font-mono text-meta text-danger">
          {instance.reload_error}
        </p>
      ) : null}

      <div className="mt-3 border-t border-border-hairline pt-3">
        {errorPlugins.length === 0 ? (
          <p className="text-meta text-ink-3">
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
                  <span className="font-mono text-meta text-ink-1">
                    {plugin.key}
                  </span>
                  <RuntimeStatusBadge status={plugin.status} />
                  {plugin.version !== undefined ? (
                    <span className="font-mono text-meta text-ink-3">
                      v{plugin.version}
                    </span>
                  ) : null}
                </div>
                <ul className="mt-1 space-y-0.5">
                  {(plugin.errors ?? []).map((issue) => (
                    <li
                      key={`${issue.fieldPath}-${issue.rule}-${issue.message}`}
                      className="font-mono text-meta text-danger"
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
    </section>
  )
}
