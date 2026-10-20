import { useQuery } from '@tanstack/react-query'
import {
  createFileRoute,
  Outlet,
  redirect,
  useNavigate,
  useRouterState,
} from '@tanstack/react-router'
import { useState } from 'react'

import { usePricingCapabilities } from '@/features/admin/pricing/use-pricing-capabilities'
import { usePlaygroundEnabled } from '@/features/playground/visibility'
import { AnnouncementPopup } from '@/features/shell/announcement-popup'
import {
  CommandPalette,
  useCommandPaletteShortcut,
} from '@/features/shell/command-palette'
import { fetchMe, meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'

// Keys carry the `common.` module prefix because the dictionary flattener
// already restores the module namespace (`{module}.{feature}.{element}`).
// `admin` items are hidden from non-admin sessions (§9.3 role guard); the
// server still enforces RBAC on every request — this is UX only (§9.1).
const NAV_ITEMS = [
  { href: '/dashboard', key: 'common.nav.dashboard' },
  // M6 API tokens (modules-02): the user-facing key management page.
  { href: '/tokens', key: 'common.nav.tokens' },
  { href: '/wallet', key: 'common.nav.wallet' },
  { href: '/orders', key: 'common.nav.orders' },
  // M5 兑换码管理（modules-04 F-04-15/16）：web-00 §4 冻结为不带 /admin
  // 前缀的管理专属 URL，页面自己施加 admin + redemption:manage 守卫；
  // 这里同样按权限隐藏入口，避免把注定 403 的链接展示给无权限管理员。
  {
    href: '/redemption-codes',
    key: 'common.nav.redemption_codes',
    admin: true,
    permission: 'redemption:manage',
  },
  // M6 logs (modules-05): both pages narrow to the caller's own rows server-side.
  // IA §4.3 puts them under `/dashboard`; the old top-level paths redirect.
  { href: '/dashboard/usage-logs', key: 'common.nav.usage_logs' },
  { href: '/dashboard/audit-logs', key: 'common.nav.audit_logs' },
  { href: '/profile', key: 'common.nav.profile' },
  { href: '/security', key: 'common.nav.security' },
  // M2 channel management; `/admin/channels` sits under the admin layout guard.
  { href: '/admin/channels', key: 'common.nav.channels', admin: true },
  // D1d 模型与供应商目录（modules-06 F-06-22～26）：页面与入口同权限——
  // 读 `channels:read`；写面与批量/同步按钮在页内按更细的权限再收一层。
  // 缺读权限时入口不渲染，直链由路由 beforeLoad 送 403。
  {
    href: '/admin/models',
    key: 'common.nav.models',
    admin: true,
    permission: 'channels:read',
  },
  { href: '/admin/users', key: 'common.nav.users', admin: true },
  { href: '/admin/users/groups', key: 'common.nav.groups', admin: true },
  // M6c FE-08: settings shell plus the two system pages that are reachable
  // from the same admin area. All three inherit the `_console/admin` guard.
  { href: '/admin/settings', key: 'common.nav.settings', admin: true },
  { href: '/admin/system/status', key: 'common.nav.system_status', admin: true },
  {
    href: '/admin/system/audit-logs',
    key: 'common.nav.operation_logs',
    admin: true,
  },
  // M6d 模型部署（modules-08 §6.13）：独立页，入口与页面能力一致地复用
  // settings:manage；缺少权限时连入口都不渲染，直链由路由守卫落 403。
  {
    href: '/admin/system/deployments',
    key: 'common.nav.model_deployments',
    admin: true,
    permission: 'settings:manage',
  },
  // M6d 任务插件（modules-08 §6.10）：独立页、页内六 Tab。后端 R-M6D-16
  // 已把读端点统一为 settings:manage，入口与路由守卫因此同权限；缺权限时
  // 入口不渲染、直链由路由 beforeLoad 送 403。
  {
    href: '/admin/system/task-plugins',
    key: 'common.nav.task_plugins',
    admin: true,
    permission: 'settings:manage',
  },
  // D3b 性能管理（modules-08 §6.12）：独立页，三个运维对象都是进程本地，
  // 读写统一复用 settings:manage；缺权限时入口不渲染、直链由 beforeLoad
  // 送 403（与部署/插件页同一口径）。
  {
    href: '/admin/system/performance',
    key: 'common.nav.performance',
    admin: true,
    permission: 'settings:manage',
  },
] as const

/**
 * 定价分组的导航项。可见性叠加 pricing:read（F-06-13/16 的页面权限），
 * 与「admin 角色」这一层是 AND 关系。
 */
const PRICING_NAV_ITEMS = [
  {
    href: '/admin/pricing/model-pricing',
    key: 'common.nav.model_pricing',
  },
  {
    href: '/admin/pricing/group-pricing',
    key: 'common.nav.group_pricing',
  },
] as const

function linkClass(active: boolean): string {
  return active ? 'font-medium text-ink-1' : 'text-ink-2 hover:text-ink-1'
}

/** 活动路由精确匹配；/admin/pricing 的 index 会重定向到子路由。 */
function isActive(pathname: string | undefined, href: string): boolean {
  if (pathname === undefined) return false
  return pathname === href || pathname === `${href}/`
}

export interface ConsoleHeaderProps {
  /** 当前路由；由 ConsoleLayout 注入，测试可省略。 */
  activePath?: string
  /** 打开命令面板的可见入口（web-00 §15.1.1）。 */
  onOpenCommandPalette?: () => void
}

/**
 * macOS 用 ⌘K，其余平台用 Ctrl+K。`userAgentData.platform` 优先，浏览器
 * 不支持时回退到已弃用但兼容面最广的 `navigator.platform`。
 */
function isApplePlatform(): boolean {
  if (typeof navigator === 'undefined') return false
  const uaData = navigator as Navigator & {
    userAgentData?: { platform?: string }
  }
  const platform = uaData.userAgentData?.platform ?? navigator.platform
  return /mac|iphone|ipad|ipod/i.test(platform)
}

/** Extracted so the layout header can be rendered in isolation by tests. */
export function ConsoleHeader({
  activePath,
  onOpenCommandPalette,
}: ConsoleHeaderProps) {
  const { t } = useI18n()
  const shortcutHint = isApplePlatform()
    ? t('common.command.shortcut_mac')
    : t('common.command.shortcut_other')
  // The console guard has already seeded `['me']`; this read is served from
  // the cache (a direct `/dashboard` navigation still triggers one probe).
  const meQuery = useQuery(meQueryOptions())
  const capabilities = usePricingCapabilities()
  // 站点级侧栏开关（`nav.modules.chat.playground`，默认 true）。这里只读根
  // 守卫已写入的 status 缓存，不额外发请求；关闭时 Chat 组整体不渲染。
  const playgroundEnabled = usePlaygroundEnabled()

  // 能力未知时按 M1 的「先隐藏、确认后再显示」处理：不把入口闪给一个
  // 可能没有 pricing:read 的管理员。服务端仍会拒绝越权请求。
  const canSeePricing = capabilities.known && capabilities.canRead

  // 细粒度权限的入口可见性（§9.3 UX 层）：服务端仍逐请求强制。权限集合
  // 来自 /api/me 的生效权限（`["*"]` 代表全量管理员）。
  const permissions = meQuery.data?.permissions
  const can = (key: string): boolean =>
    permissions?.some(
      (permission) => permission === '*' || permission === key,
    ) ?? false

  return (
    <header className="border-b border-border-hairline bg-surface-card px-6 py-3">
      <div className="flex items-center gap-6">
        <p className="text-sm font-semibold">{t('common.app.title')}</p>
        <nav
          aria-label={t('common.app.title')}
          className="flex flex-wrap items-center gap-4 text-[13px]"
        >
          {NAV_ITEMS.filter(
            (item) =>
              (!('admin' in item) || meQuery.data?.role === 'admin') &&
              (!('permission' in item) || can(item.permission)),
          ).map((item) => (
            <a
              key={item.href}
              href={item.href}
              aria-current={isActive(activePath, item.href) ? 'page' : undefined}
              className={linkClass(isActive(activePath, item.href))}
            >
              {t(item.key)}
            </a>
          ))}
          {playgroundEnabled ? (
            <div
              role="group"
              aria-label={t('common.nav.chat')}
              className="flex items-center gap-3 border-l border-border-hairline pl-4"
            >
              <span className="text-ink-3">{t('common.nav.chat')}</span>
              <a
                href="/playground"
                aria-current={
                  isActive(activePath, '/playground') ? 'page' : undefined
                }
                className={linkClass(isActive(activePath, '/playground'))}
              >
                {t('common.nav.playground')}
              </a>
            </div>
          ) : null}
          {canSeePricing ? (
            <div
              role="group"
              className="flex items-center gap-3 border-l border-border-hairline pl-4"
              aria-label={t('common.nav.pricing')}
            >
              <span className="text-ink-3">{t('common.nav.pricing')}</span>
              {PRICING_NAV_ITEMS.map((item) => (
                <a
                  key={item.href}
                  href={item.href}
                  aria-current={isActive(activePath, item.href) ? 'page' : undefined}
                  className={linkClass(isActive(activePath, item.href))}
                >
                  {t(item.key)}
                </a>
              ))}
            </div>
          ) : null}
        </nav>
        <button
          type="button"
          aria-keyshortcuts="Meta+K Control+K"
          onClick={onOpenCommandPalette}
          className="ml-auto inline-flex h-8 items-center gap-2 rounded-lg border border-border-default bg-surface-page px-2.5 text-[13px] text-ink-2 hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          <svg
            aria-hidden="true"
            viewBox="0 0 16 16"
            className="size-4"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.5"
          >
            <circle cx="7" cy="7" r="4.5" />
            <path d="m10.5 10.5 3 3" strokeLinecap="round" />
          </svg>
          <span>{t('common.command.open')}</span>
          <kbd className="rounded border border-border-hairline bg-surface-card px-1 font-mono text-[11px] text-ink-3">
            {shortcutHint}
          </kbd>
        </button>
      </div>
    </header>
  )
}

export function ConsoleLayout() {
  // 路由变化会重渲染 layout；在这里读一次 pathname 交给 header 做活动态。
  const activePath = useRouterState({
    select: (state) => state.location.pathname,
  })
  const navigate = useNavigate()
  const [commandPaletteOpen, setCommandPaletteOpen] = useState(false)

  // 全局快捷键只在控制台壳挂载一次；ConsoleLayout 卸载时移除（web-00 §15.1.1）。
  useCommandPaletteShortcut(() => setCommandPaletteOpen(true))

  return (
    <div className="min-h-screen bg-surface-page text-ink-1">
      <ConsoleHeader
        activePath={activePath}
        onOpenCommandPalette={() => setCommandPaletteOpen(true)}
      />
      <main id="main" className="p-6">
        <Outlet />
      </main>
      {/*
       * 用户侧公告弹层（modules-01 归属，挂在控制台外壳）：只在布局渲染后
       * 拉取生效中公告，不参与任何路由 loader，因此不阻塞导航。
       */}
      <AnnouncementPopup />
      <CommandPalette
        open={commandPaletteOpen}
        onOpenChange={setCommandPaletteOpen}
        onNavigate={(to) => {
          // 客户端跳转，不产生任何请求（命令面板不是全局搜索）。
          void navigate({ to })
        }}
      />
    </div>
  )
}

/**
 * Session layer of the guard chain (web-00 §9.3 row 1). A missing session
 * redirects to `/sign-in` with the origin path in `?redirect=`; no intermediate
 * 401 page is rendered (§9.4). The path is sanitized again by
 * `sanitizeAuthRedirect` when the sign-in flow consumes it.
 */
export const Route = createFileRoute('/_console')({
  beforeLoad: async ({ context, location }) => {
    const me = await fetchMe(context.queryClient)
    if (me === null) {
      throw redirect({ to: '/sign-in', search: { redirect: location.href } })
    }
    return { me }
  },
  component: ConsoleLayout,
})
