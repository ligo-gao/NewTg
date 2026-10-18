import { createFileRoute } from '@tanstack/react-router'

import { ChannelListPage } from '@/features/admin/channels/channel-list-page'
import { useI18n } from '@/shared/lib/i18n-context'

function ChannelsRoute() {
  const { t } = useI18n()

  return (
    <section className="mx-auto mt-6 max-w-[1440px]">
      <h1 className="text-xl font-semibold text-ink-1">{t('common.channels.title')}</h1>
      <div className="mt-4">
        <ChannelListPage />
      </div>
    </section>
  )
}

export const Route = createFileRoute('/_console/admin/channels/')({
  component: ChannelsRoute,
})
