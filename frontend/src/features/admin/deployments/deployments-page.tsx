import { useQuery } from '@tanstack/react-query'
import { useState, type KeyboardEvent } from 'react'

import { fetchDeploymentSettings } from './api'
import { CreateDeploymentDrawer } from './create-deployment-drawer'
import { DeploymentsTab } from './deployments-tab'
import { ResourcesTab } from './resources-tab'
import { SettingsTab } from './settings-tab'
import { hasSettingsManage, type DeploymentsTabId } from './tabs'
import { useOnlineStatus } from './use-online'
import { meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'

export type { DeploymentsTabId } from './tabs'

const TABS: { id: DeploymentsTabId; key: string }[] = [
  { id: 'deployments', key: 'admin.deployments.page.tab_deployments' },
  { id: 'resources', key: 'admin.deployments.page.tab_resources' },
  { id: 'settings', key: 'admin.deployments.page.tab_settings' },
]

/**
 * 模型部署管理页（UI §6.13，F-08-51～F-08-58）。
 *
 * 独立页、页内三 Tab；不进 Settings Shell。全部能力复用 `settings:manage`：
 * 缺少权限时入口不渲染（`_console.tsx`）、直链由路由 beforeLoad 送 `/403`；
 * 页面内再做一层 canManage 收窄，保证写操作永远不会以只读身份出现。
 */
export function ModelDeploymentsPage({
  tab,
  onTabChange,
}: {
  tab: DeploymentsTabId
  onTabChange: (tab: DeploymentsTabId) => void
}) {
  const { t } = useI18n()
  const online = useOnlineStatus()
  const me = useQuery(meQueryOptions())
  const canManage = hasSettingsManage(me.data?.permissions)
  const settingsQuery = useQuery({
    queryKey: ['deployment-settings'],
    queryFn: ({ signal }) => fetchDeploymentSettings(signal),
    enabled: canManage,
    retry: false,
  })
  const settings = settingsQuery.data
  const [createOpen, setCreateOpen] = useState(false)

  const createDisabled =
    !canManage ||
    !online ||
    settings?.enabled === false ||
    settings?.has_secret === false
  const createDisabledReason = !online
    ? t('admin.deployments.common.offline')
    : settings?.enabled === false
      ? t('admin.deployments.list.banner_disabled')
      : settings?.has_secret === false
        ? t('admin.deployments.list.banner_no_secret')
        : undefined

  const handleTabKey = (
    event: KeyboardEvent<HTMLDivElement>,
    index: number,
  ): void => {
    if (event.key !== 'ArrowRight' && event.key !== 'ArrowLeft') return
    event.preventDefault()
    const next =
      event.key === 'ArrowRight'
        ? (index + 1) % TABS.length
        : (index - 1 + TABS.length) % TABS.length
    const target = TABS[next]
    if (target === undefined) return
    onTabChange(target.id)
    document.getElementById(`deployments-tab-${target.id}`)?.focus()
  }

  return (
    <div
      className="mx-auto flex max-w-[1520px] flex-col gap-4"
      data-testid="model-deployments-page"
    >
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold text-ink-1">
            {t('admin.deployments.page.title')}
          </h1>
          <p className="mt-1 text-sm text-ink-2">
            {t('admin.deployments.page.subtitle')}
          </p>
        </div>
        {tab === 'deployments' && canManage ? (
          <div className="flex flex-col items-end gap-1">
            <Button
              disabled={createDisabled}
              title={createDisabledReason}
              onClick={() => setCreateOpen(true)}
            >
              {t('admin.deployments.list.create')}
            </Button>
            {createDisabled && createDisabledReason !== undefined ? (
              <p className="text-xs text-ink-3">{createDisabledReason}</p>
            ) : null}
          </div>
        ) : null}
      </header>

      {!canManage && !me.isPending ? (
        <FormAlert tone="info">{t('admin.deployments.common.read_only')}</FormAlert>
      ) : null}

      <div
        role="tablist"
        aria-label={t('admin.deployments.page.tabs_label')}
        className="flex flex-wrap gap-1 border-b border-border-hairline"
        onKeyDown={(event) => {
          const index = TABS.findIndex((entry) => entry.id === tab)
          if (index >= 0) handleTabKey(event, index)
        }}
      >
        {TABS.map((entry) => {
          const selected = entry.id === tab
          return (
            <button
              key={entry.id}
              id={`deployments-tab-${entry.id}`}
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
        id={`deployments-panel-${tab}`}
        aria-labelledby={`deployments-tab-${tab}`}
      >
        {tab === 'deployments' ? (
          <DeploymentsTab
            canManage={canManage}
            enabled={settings?.enabled}
            hasSecret={settings?.has_secret}
            onGoSettings={() => onTabChange('settings')}
            onCreate={() => setCreateOpen(true)}
          />
        ) : null}
        {tab === 'resources' ? <ResourcesTab /> : null}
        {tab === 'settings' ? <SettingsTab canManage={canManage} /> : null}
      </div>

      <CreateDeploymentDrawer open={createOpen} onClose={() => setCreateOpen(false)} />
    </div>
  )
}
