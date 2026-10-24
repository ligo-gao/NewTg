/**
 * 轻量纯函数模块：路由的 `validateSearch` / `beforeLoad` 会随入口加载，
 * 因此这些辅助函数不能放在页面特性模块里，否则会把整个对账页面（含表格、
 * 图表与 API 客户端）拖进首屏 chunk。
 */
export type ReconciliationTabId = 'entry' | 'results' | 'overview' | 'accounts'

export const RECONCILIATION_TABS: { id: ReconciliationTabId; key: string }[] = [
  { id: 'entry', key: 'admin.upstream_reconciliation.page.tab_entry' },
  { id: 'results', key: 'admin.upstream_reconciliation.page.tab_results' },
  { id: 'overview', key: 'admin.upstream_reconciliation.page.tab_overview' },
  { id: 'accounts', key: 'admin.upstream_reconciliation.page.tab_accounts' },
]

export function normalizeReconciliationTab(
  value: unknown,
): ReconciliationTabId {
  return value === 'entry' || value === 'results' || value === 'accounts'
    ? value
    : 'overview'
}

export function hasSettingsManage(
  permissions: readonly string[] | undefined,
): boolean {
  return (
    permissions?.some(
      (permission) => permission === '*' || permission === 'settings:manage',
    ) ?? false
  )
}
