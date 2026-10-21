import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'

import { useI18n } from '@/shared/lib/i18n-context'

import { listTaskPlugins } from './api'
import { EmptyState, ErrorState, PluginIcon, SectionCard } from './components'
import { errorMessage } from './utils'

/**
 * 定价 Tab（F-08-37，**只读**）。
 *
 * 定价的单一真源是模块 06 的模型定价（「模型 × 插件变体」计费表达式），
 * 本页签不提供任何输入控件，只做聚合入口 + 深链。契约目前没有插件维度的
 * 计费聚合端点，因此计费形态/变体数/关联模型数显示为「—」，并在页内说明；
 * 深链 `?plugin=<key>` 由模型定价页在前端按 `billing_setting.plugin_billing_expr`
 * 的 `<pluginKey>::<model>` 键过滤（C3/R-50，不新增后端端点）。
 */
export function PricingTab() {
  const { t } = useI18n()

  const pluginsQuery = useQuery({
    queryKey: ['plugin-list', 'pricing-all'],
    queryFn: ({ signal }) => listTaskPlugins({ page: 1, pageSize: 100 }, signal),
  })

  const items = pluginsQuery.data?.items ?? []

  return (
    <SectionCard description={t('admin.task_plugins.pricing.readonly_note')}>
      {pluginsQuery.isPending ? (
        <div className="space-y-2" aria-hidden="true">
          {Array.from({ length: 4 }, (_, index) => (
            <div
              key={index}
              className="h-10 animate-pulse rounded-lg bg-surface-subtle"
            />
          ))}
        </div>
      ) : null}

      {pluginsQuery.isError ? (
        <ErrorState
          message={errorMessage(pluginsQuery.error)}
          onRetry={() => void pluginsQuery.refetch()}
        />
      ) : null}

      {pluginsQuery.data !== undefined && items.length === 0 ? (
        <EmptyState
          testId="plugin-pricing-empty"
          title={t('admin.task_plugins.pricing.empty')}
        />
      ) : null}

      {items.length > 0 ? (
        <>
          <div className="overflow-x-auto rounded-xl border border-border-default">
            <table className="w-full text-left text-sm">
              <thead className="bg-surface-subtle text-xs text-ink-3">
                <tr>
                  <th className="px-3 py-2 font-medium">
                    {t('admin.task_plugins.pricing.col_plugin')}
                  </th>
                  <th className="px-3 py-2 font-medium">
                    {t('admin.task_plugins.pricing.col_shape')}
                  </th>
                  <th className="px-3 py-2 font-medium">
                    {t('admin.task_plugins.pricing.col_variants')}
                  </th>
                  <th className="px-3 py-2 font-medium">
                    {t('admin.task_plugins.pricing.col_models')}
                  </th>
                  <th className="px-3 py-2 font-medium">
                    {t('admin.task_plugins.installed.col_actions')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {items.map((plugin) => (
                  <tr
                    key={plugin.key}
                    data-testid="pricing-row"
                    className="border-t border-border-hairline"
                  >
                    <td className="px-3 py-2">
                      <span className="flex items-center gap-2">
                        <PluginIcon size="sm" icon={plugin.icon} pluginKey={plugin.key} />
                        <span className="flex flex-col">
                          <span className="font-medium text-ink-1">{plugin.name}</span>
                          <span className="font-mono text-xs text-ink-3">
                            {plugin.key}
                          </span>
                        </span>
                      </span>
                    </td>
                    {/* 契约未暴露插件计费聚合：不臆造形态与计数。 */}
                    <td className="px-3 py-2 text-ink-3">
                      {t('admin.task_plugins.pricing.unknown')}
                    </td>
                    <td className="px-3 py-2 text-ink-3">
                      {t('admin.task_plugins.pricing.unknown')}
                    </td>
                    <td className="px-3 py-2 text-ink-3">
                      {t('admin.task_plugins.pricing.unknown')}
                    </td>
                    <td className="px-3 py-2">
                      <Link
                        to="/admin/pricing/model-pricing"
                        search={{ plugin: plugin.key }}
                        className="inline-flex items-center gap-1 text-xs text-brand underline"
                      >
                        {t('admin.task_plugins.pricing.goto')}
                        <span aria-hidden="true">↗</span>
                      </Link>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="mt-2 text-xs text-ink-3">
            {t('admin.task_plugins.pricing.aggregation_pending')}
          </p>
        </>
      ) : null}
    </SectionCard>
  )
}
