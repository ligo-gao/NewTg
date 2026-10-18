import { useQuery } from '@tanstack/react-query'
import {
  createFileRoute,
  Outlet,
  redirect,
  useRouterState,
} from '@tanstack/react-router'

import { usePricingCapabilities } from '@/features/admin/pricing/use-pricing-capabilities'
import { fetchMe, meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'

// Keys carry the `common.` module prefix because the dictionary flattener
// already restores the module namespace (`{module}.{feature}.{element}`).
// `admin` items are hidden from non-admin sessions (§9.3 role guard); the
// server still enforces RBAC on every request — this is UX only (§9.1).
const NAV_ITEMS = [
  { href: '/dashboard', key: 'common.nav.dashboard' },
  { href: '/profile', key: 'common.nav.profile' },
  { href: '/security', key: 'common.nav.security' },
  { href: '/admin/users', key: 'common.nav.users', admin: true },
  { href: '/admin/users/groups', key: 'common.nav.groups', admin: true },
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
}

/** Extracted so the layout header can be rendered in isolation by tests. */
export function ConsoleHeader({ activePath }: ConsoleHeaderProps) {
  const { t } = useI18n()
  // The console guard has already seeded `['me']`; this read is served from
  // the cache (a direct `/dashboard` navigation still triggers one probe).
  const meQuery = useQuery(meQueryOptions())
  const capabilities = usePricingCapabilities()

  // 能力未知时按 M1 的「先隐藏、确认后再显示」处理：不把入口闪给一个
  // 可能没有 pricing:read 的管理员。服务端仍会拒绝越权请求。
  const canSeePricing = capabilities.known && capabilities.canRead

  return (
    <header className="border-b border-border-hairline bg-surface-card px-6 py-3">
      <div className="flex items-center gap-6">
        <p className="text-sm font-semibold">{t('common.app.title')}</p>
        <nav
          aria-label={t('common.app.title')}
          className="flex flex-wrap items-center gap-4 text-[13px]"
        >
          {NAV_ITEMS.filter(
            (item) => !('admin' in item) || meQuery.data?.role === 'admin',
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
      </div>
    </header>
  )
}

export function ConsoleLayout() {
  // 路由变化会重渲染 layout；在这里读一次 pathname 交给 header 做活动态。
  const activePath = useRouterState({
    select: (state) => state.location.pathname,
  })

  return (
    <div className="min-h-screen bg-surface-page text-ink-1">
      <ConsoleHeader activePath={activePath} />
      <main id="main" className="p-6">
        <Outlet />
      </main>
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
