import { useQuery } from '@tanstack/react-query'
import { useMemo, type KeyboardEvent } from 'react'

import { fetchSettingsSection } from '@/features/admin/settings/api'
import { meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'

import { InstalledTab } from './installed-tab'
import { MarketplaceTab } from './marketplace-tab'
import { PricingTab } from './pricing-tab'
import { RuntimeTab } from './runtime-tab'
import { SandboxTab } from './sandbox-tab'
import { hasSettingsManage, type TaskPluginsTabId } from './tabs'
import { UploadTab } from './upload-tab'
import { useOnlineStatus } from './use-online'

export type { TaskPluginsTabId } from './tabs'

const TABS: { id: TaskPluginsTabId; key: string }[] = [
  { id: 'installed', key: 'admin.task_plugins.page.tab_installed' },
  { id: 'upload', key: 'admin.task_plugins.page.tab_upload' },
  { id: 'marketplace', key: 'admin.task_plugins.page.tab_marketplace' },
  { id: 'sandbox', key: 'admin.task_plugins.page.tab_sandbox' },
  { id: 'pricing', key: 'admin.task_plugins.page.tab_pricing' },
  { id: 'runtime', key: 'admin.task_plugins.page.tab_runtime' },
]

function settingsValue(
  section: { items: { key: string; value: unknown }[] } | undefined,
  key: string,
): unknown {
  return section?.items.find((item) => item.key === key)?.value
}

/**
 * 任务插件管理页（UI §6.10，F-08-28～F-08-37）。
 *
 * 独立页、页内六 Tab，不进 Settings Shell：本页数据是动态注册表 + 逐操作
 * 即时生效的写操作，没有「整段未保存」语义，套 Shell 会让脏标记无从定义。
 *
 * 权限（R-M6D-02）：读 admin、写 `settings:manage`。缺少 manage 时上传 Tab、
 * 行内开关、市场安装、沙箱试运行与版本操作全部不渲染；服务端仍逐请求强制。
 */
export function TaskPluginsPage({
  tab,
  onTabChange,
}: {
  tab: TaskPluginsTabId
  onTabChange: (tab: TaskPluginsTabId) => void
}) {
  const { t } = useI18n()
  const online = useOnlineStatus()
  const me = useQuery(meQueryOptions())
  const canManage = hasSettingsManage(me.data?.permissions)

  // plugin section 的读取是 admin 权限：市场总开关与内置禁用集都在这里。
  // 读取失败按契约默认值降级（市场开、禁用集空），不用空态冒充配置。
  const settingsQuery = useQuery({
    queryKey: ['plugin-settings'],
    queryFn: ({ signal }) => fetchSettingsSection('plugin', signal),
    retry: false,
    staleTime: 30_000,
  })
  const marketplaceEnabled =
    settingsValue(settingsQuery.data, 'plugin.marketplace.enabled') !== false

  const factoryDisabledKeys = useMemo(() => {
    const value = settingsValue(settingsQuery.data, 'plugin.disabled_factory_keys')
    return Array.isArray(value)
      ? value.filter((entry): entry is string => typeof entry === 'string')
      : []
  }, [settingsQuery.data])

  const visibleTabs = TABS.filter((entry) => {
    if (entry.id === 'upload') return canManage
    if (entry.id === 'marketplace') return marketplaceEnabled
    return true
  })
  const effectiveTab = visibleTabs.some((entry) => entry.id === tab) ? tab : 'installed'

  const handleTabKey = (
    event: KeyboardEvent<HTMLDivElement>,
    index: number,
  ): void => {
    if (event.key !== 'ArrowRight' && event.key !== 'ArrowLeft') return
    event.preventDefault()
    const next =
      event.key === 'ArrowRight'
        ? (index + 1) % visibleTabs.length
        : (index - 1 + visibleTabs.length) % visibleTabs.length
    const target = visibleTabs[next]
    if (target === undefined) return
    onTabChange(target.id)
    document.getElementById(`task-plugins-tab-${target.id}`)?.focus()
  }

  return (
    <div
      className="mx-auto flex max-w-[1520px] flex-col gap-4"
      data-testid="task-plugins-page"
    >
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold text-ink-1">
            {t('admin.task_plugins.page.title')}
          </h1>
          <p className="mt-1 text-sm text-ink-2">
            {t('admin.task_plugins.page.subtitle')}
          </p>
        </div>
        {canManage && effectiveTab !== 'upload' ? (
          <Button onClick={() => onTabChange('upload')}>
            {t('admin.task_plugins.page.upload_shortcut')}
          </Button>
        ) : null}
      </header>

      {!canManage && !me.isPending ? (
        <FormAlert tone="info">{t('admin.task_plugins.common.read_only')}</FormAlert>
      ) : null}

      <div
        role="tablist"
        aria-label={t('admin.task_plugins.page.tabs_label')}
        className="flex flex-wrap gap-1 border-b border-border-hairline"
        onKeyDown={(event) => {
          const index = visibleTabs.findIndex((entry) => entry.id === effectiveTab)
          if (index >= 0) handleTabKey(event, index)
        }}
      >
        {visibleTabs.map((entry) => {
          const selected = entry.id === effectiveTab
          return (
            <button
              key={entry.id}
              id={`task-plugins-tab-${entry.id}`}
              type="button"
              role="tab"
              aria-selected={selected}
              tabIndex={selected ? 0 : -1}
              onClick={() => onTabChange(entry.id)}
              className={[
                'h-10 rounded-t-lg px-3 text-[13px]',
                selected
                  ? 'border-b-2 border-brand font-medium text-ink-1'
                  : 'text-ink-2 hover:bg-surface-hover hover:text-ink-1',
              ].join(' ')}
            >
              {t(entry.key)}
            </button>
          )
        })}
      </div>

      <div
        role="tabpanel"
        id={`task-plugins-panel-${effectiveTab}`}
        aria-labelledby={`task-plugins-tab-${effectiveTab}`}
      >
        {effectiveTab === 'installed' ? (
          <InstalledTab
            canManage={canManage}
            online={online}
            factoryDisabledKeys={factoryDisabledKeys}
            factorySettingsUpdatedAt={settingsQuery.data?.updated_at ?? null}
            onFactorySaved={() => {
              void settingsQuery.refetch()
            }}
            onGoRuntime={() => onTabChange('runtime')}
            onGoUpload={() => onTabChange('upload')}
          />
        ) : null}
        {effectiveTab === 'upload' ? (
          <UploadTab canManage={canManage} online={online} />
        ) : null}
        {effectiveTab === 'marketplace' ? (
          <MarketplaceTab
            canManage={canManage}
            online={online}
            enabled={marketplaceEnabled}
          />
        ) : null}
        {effectiveTab === 'sandbox' ? (
          <SandboxTab canManage={canManage} online={online} />
        ) : null}
        {effectiveTab === 'pricing' ? <PricingTab /> : null}
        {effectiveTab === 'runtime' ? <RuntimeTab /> : null}
      </div>
    </div>
  )
}
