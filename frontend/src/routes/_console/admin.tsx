import { createFileRoute, Outlet, redirect } from '@tanstack/react-router'

import { fetchMe } from '@/lib/session'

/**
 * Role layer of the guard chain (web-00 §9.3 row 2). It is a UX gate only —
 * every admin endpoint still enforces RBAC server-side (§9.1). A signed-in
 * non-admin lands on the single `/403` page (§8.3), never on `/401`.
 *
 * The pathless `children` under this file (`users/…`) inherit the guard, so
 * new admin screens cannot forget to mount it.
 */
export const Route = createFileRoute('/_console/admin')({
  beforeLoad: async ({ context, location }) => {
    const me = await fetchMe(context.queryClient)
    if (me === null) {
      // The parent guard normally catches this first; keep the redirect here
      // so this layer is correct in isolation (e.g. a future route re-parent).
      throw redirect({ to: '/sign-in', search: { redirect: location.href } })
    }
    if (me.role !== 'admin') {
      throw redirect({ to: '/403' })
    }
  },
  component: Outlet,
})
