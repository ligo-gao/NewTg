import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { AnnouncementPage } from '@/features/public/announcements/announcement-page'
import { PublicLayout } from '@/features/public/public-layout'

/**
 * 公开公告详情（`/announcements/$id`）。
 *
 * 公开只读页：不挂登录守卫，匿名可直接访问；`PublicLayout` 提供共享的
 * header/footer 与 `<main id="main">`。后端只对「已发布且生效中」的公告
 * 返回 200，其余 404 —— 空态由页面渲染，路由本身不做存在性判断。
 */
export const Route = createFileRoute('/announcements/$id')({
  loader: () => loadRouteDictionary('public'),
  component: AnnouncementRoute,
})

function AnnouncementRoute() {
  const { id } = Route.useParams()
  return (
    <PublicLayout activePath="/announcements">
      <AnnouncementPage id={id} />
    </PublicLayout>
  )
}
