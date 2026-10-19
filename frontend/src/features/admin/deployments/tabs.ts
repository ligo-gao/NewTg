/**
 * 轻量纯函数模块：路由的 `validateSearch` / `beforeLoad` 会随入口加载，
 * 因此这些辅助函数不能放在页面特性模块里，否则会把整个部署页面（含抽屉、
 * 表格与 API 客户端）拖进首屏 chunk，破坏 150 KiB 预算。
 */
export type DeploymentsTabId = 'deployments' | 'resources' | 'settings'

export function normalizeDeploymentsTab(value: unknown): DeploymentsTabId {
  return value === 'resources' || value === 'settings' ? value : 'deployments'
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
