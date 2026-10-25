import { createFileRoute, redirect } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { AboutPage } from '@/features/public/about-page'
import { aboutAccessRedirect } from '@/features/public/about-guard'
import { PublicLayout } from '@/features/public/public-layout'

export const Route = createFileRoute('/about')({
  // 公开页字典按域懒加载（web-00 §7.2）：about 路由不需要引入控制台字典。
  loader: () => loadRouteDictionary('public'),
  beforeLoad: async ({ context, location }) => {
    const target = await aboutAccessRedirect(
      context.queryClient,
      location.pathname,
      location.searchStr,
    )
    if (target !== null) throw redirect({ href: target })
  },
  component: AboutRoute,
})

function AboutRoute() {
  return (
    <PublicLayout activePath="/about">
      <AboutPage />
    </PublicLayout>
  )
}
