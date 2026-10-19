import { createFileRoute, redirect } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { PlaygroundPage } from '@/features/playground/playground-page'
import { isPlaygroundEnabled } from '@/features/playground/visibility'
import { fetchPublicStatus } from '@/lib/public-status'

/**
 * `/playground`（F-02-19）。
 *
 * 侧栏键 `nav.modules.chat.playground=false` 时直链必须重定向 `/dashboard`
 * （§2.15 权限 + TC-F-02-19-05）。该键只是前端可见性：`/pg/*` 仍由会话
 * 鉴权与分组/余额独立判定，因此这里不做任何“接口授权”假设。
 *
 * 状态探测失败（null）时放行：一次 `/api/status` 故障不应把页面锁死，
 * 后端对 `/pg/*` 的鉴权才是权威。
 */
export const Route = createFileRoute('/_console/playground')({
  beforeLoad: async ({ context }) => {
    const status = await fetchPublicStatus(context.queryClient)
    if (status !== null && !isPlaygroundEnabled(status)) {
      throw redirect({ to: '/dashboard' })
    }
  },
  loader: () => loadRouteDictionary('account'),
  component: PlaygroundPage,
})
