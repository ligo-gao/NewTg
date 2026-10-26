import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { ChannelListPage } from '@/features/admin/channels/channel-list-page'
import { useI18n } from '@/shared/lib/i18n-context'

function ChannelsRoute() {
  const { t } = useI18n()

  return (
    <section className="mx-auto mt-6 max-w-[1520px]">
      <h1 className="text-page-title text-ink-1">{t('common.channels.title')}</h1>
      <div className="mt-4">
        <ChannelListPage />
      </div>
    </section>
  )
}

export const Route = createFileRoute('/_console/admin/channels/')({
  // 渠道表单的「补全模型档案」向导复用 `admin.models.capability_*` /
  // `admin.models.modality_*` 标签，因此本路由必须先把 admin 域词典
  // 加载进来，否则向导会渲染原始键名。
  loader: () => loadRouteDictionary('admin'),
  component: ChannelsRoute,
})
