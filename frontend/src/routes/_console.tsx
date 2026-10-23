import { createFileRoute, redirect } from '@tanstack/react-router'

import { ConsoleShell } from '@/features/shell/console-shell'
import { fetchMe } from '@/lib/session'

/**
 * Session layer of the guard chain (web-00 §9.3 row 1). A missing session
 * redirects to `/sign-in` with the origin path in `?redirect=`; no intermediate
 * 401 page is rendered (§9.4).
 */
export const Route = createFileRoute('/_console')({
  beforeLoad: async ({ context, location }) => {
    const me = await fetchMe(context.queryClient)
    if (me === null) {
      throw redirect({ to: '/sign-in', search: { redirect: location.href } })
    }
    return { me }
  },
  component: ConsoleShell,
})
