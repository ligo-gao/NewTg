/**
 * 轻量纯函数模块：路由的 `validateSearch` / `beforeLoad` 会随入口加载，
 * 因此这些辅助函数不能放在页面特性模块里，否则会把整个插件页（六 Tab、
 * 抽屉与 API 客户端）拖进首屏 chunk（沿用 deployments/tabs.ts 的同一理由）。
 */

export type TaskPluginsTabId =
  | 'installed'
  | 'upload'
  | 'marketplace'
  | 'sandbox'
  | 'pricing'
  | 'runtime'

const TAB_IDS: readonly TaskPluginsTabId[] = [
  'installed',
  'upload',
  'marketplace',
  'sandbox',
  'pricing',
  'runtime',
]

export function normalizeTaskPluginsTab(value: unknown): TaskPluginsTabId {
  return TAB_IDS.find((id) => id === value) ?? 'installed'
}

/**
 * 写操作权限（§6.10.1）：查看是 admin，上传/激活/启停/市场源/试运行全部走
 * `settings:manage`。缺权限时入口隐藏；服务端仍逐请求强制（前端隐藏不是授权）。
 */
export function hasSettingsManage(
  permissions: readonly string[] | undefined,
): boolean {
  return (
    permissions?.some(
      (permission) => permission === '*' || permission === 'settings:manage',
    ) ?? false
  )
}
