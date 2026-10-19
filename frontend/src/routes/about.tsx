import { createFileRoute } from '@tanstack/react-router'

import { loadDictionary } from '@/app/providers/i18n'
import { AboutPage } from '@/features/public/about-page'
import { PublicLayout } from '@/features/public/public-layout'

export const Route = createFileRoute('/about')({
  // 公开页字典按域懒加载（web-00 §7.2）：about 路由不需要引入控制台字典。
  loader: () => loadDictionary('en', 'public'),
  component: AboutRoute,
})

function AboutRoute() {
  return (
    <PublicLayout activePath="/about">
      <AboutPage />
    </PublicLayout>
  )
}
