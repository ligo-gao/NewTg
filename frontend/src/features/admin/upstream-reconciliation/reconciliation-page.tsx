import { useQuery } from '@tanstack/react-query'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { useState } from 'react'

import { PageHeader } from '@/features/shell/page-header'
import { meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'
import { handleTabListKeyDown } from '@/shared/lib/roving-focus'

import { AccountsTab } from './accounts-tab'
import { EntryTab } from './entry-tab'
import { OverviewTab } from './overview-tab'
import { ResultsTab } from './results-tab'
import {
  hasSettingsManage,
  normalizeReconciliationTab,
  RECONCILIATION_TABS,
  type ReconciliationTabId,
} from './tabs'

/**
 * 上游用量对账页（管理端）。
 *
 * 页面是四个 Tab 的宿主：页头、WAI-ARIA tablist 与单一 tabpanel。当前任务
 * 只搭外壳，四个分区先渲染占位提示，后续任务逐个替换为真正的 Tab 组件。
 * `?tab=` 由本页持有：切换走 `replace`，因为它不是新页面，反复切 Tab 不应
 * 堆满历史栈。
 */
export function UpstreamReconciliationPage() {
  const { t } = useI18n()
  const search = useSearch({
    // `useSearch` keys off the route id, which keeps the pathless layout
    // segments; `useNavigate` below keys off the full URL path.
    from: '/_console/admin/upstream-reconciliation',
  })
  const navigate = useNavigate({ from: '/admin/upstream-reconciliation' })
  const tab = normalizeReconciliationTab(search.tab)
  // 路由 beforeLoad 已挡住无权限直链；页面内再收窄一次，录入 Tab 的写控件
  // 永远不会以只读身份出现（服务端仍逐请求强制）。
  const me = useQuery(meQueryOptions())
  const canManage = hasSettingsManage(me.data?.permissions)
  // 运行选择跨 Tab 共享：结果 Tab 与后续概览 Tab 读同一次运行。不放 URL
  // search param，路由的 validateSearch 不在本任务范围内。
  const [runId, setRunId] = useState<number | null>(null)

  const selectTab = (next: ReconciliationTabId): void => {
    if (next === tab) return
    void navigate({ search: { tab: next }, replace: true })
  }

  return (
    <div
      className="mx-auto flex max-w-[1520px] flex-col gap-4"
      data-testid="upstream-reconciliation-page"
    >
      <PageHeader
        crumbs={[
          { label: t('common.console.label') },
          { label: t('common.nav.group.admin') },
          { label: t('common.nav.upstream_reconciliation') },
        ]}
        title={t('admin.upstream_reconciliation.page.title')}
        description={t('admin.upstream_reconciliation.page.subtitle')}
      />

      <div
        role="tablist"
        aria-label={t('admin.upstream_reconciliation.page.tabs_label')}
        className="flex flex-wrap gap-1 border-b border-border-hairline"
        onKeyDown={handleTabListKeyDown}
      >
        {RECONCILIATION_TABS.map((entry) => {
          const selected = entry.id === tab
          return (
            <button
              key={entry.id}
              id={`reconciliation-tab-${entry.id}`}
              type="button"
              role="tab"
              aria-selected={selected}
              tabIndex={selected ? 0 : -1}
              onClick={() => {
                selectTab(entry.id)
              }}
              className={[
                'h-10 rounded-t-lg px-3 text-meta',
                selected
                  ? 'border-b-2 border-brand font-medium text-ink-1'
                  : 'text-ink-2 hover:bg-surface-hover hover:text-ink-1',
              ].join(' ')}
            >
              {t(entry.key)}
            </button>
          )
        })}
      </div>

      <section
        role="tabpanel"
        id={`reconciliation-panel-${tab}`}
        aria-labelledby={`reconciliation-tab-${tab}`}
        className="flex flex-col gap-4"
      >
        {tab === 'entry' ? <EntryTab canManage={canManage} /> : null}
        {tab === 'results' ? (
          <ResultsTab
            runId={runId}
            onSelectRun={setRunId}
            canManage={canManage}
          />
        ) : null}
        {tab === 'overview' ? <OverviewTab runId={runId} /> : null}
        {tab === 'accounts' ? <AccountsTab canManage={canManage} /> : null}
      </section>
    </div>
  )
}
