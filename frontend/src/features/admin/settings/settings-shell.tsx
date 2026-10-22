import { useCallback, useEffect, useMemo, useState, type KeyboardEvent } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { NotifyEntryCard } from '@/features/admin/notifications/notify-entry-card'
import { meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert } from '@/shared/ui/form'
import { StatusBadge } from '@/shared/ui/status-badge'

import {
  fetchPaymentCompliance,
  PAYMENT_COMPLIANCE_QUERY_KEY,
  type PaymentComplianceStatus,
} from './api'
import { PaymentComplianceGate } from './payment-compliance-gate'
import {
  SETTINGS_TABS,
  SETTINGS_TABS_CONFIG,
  tabConfig,
  type SettingsTab,
} from './sections'
import { SettingsSectionForm } from './settings-section-form'

export interface SettingsShellProps {
  tab: SettingsTab
  onTabChange: (tab: SettingsTab) => void
}

/**
 * Admin settings shell (modules-08 UI design §1.1/§1.3).
 *
 * The shell owns the seven-region tab bar and the navigation guard; each card
 * owns its own load/save lifecycle so a failure in one section never blocks
 * the others (the design mandates per-section saves).
 */
export function SettingsShell({ tab, onTabChange }: SettingsShellProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const meQuery = useQuery(meQueryOptions())

  const canManage =
    meQuery.data?.permissions.some(
      (permission) => permission === '*' || permission === 'settings:manage',
    ) ?? false

  // modules-08 §3.4：支付设置整体受合规确认保护。读面只在能管理设置且有
  // 权访问支付页时发起；未确认时绝不挂载任何支付 section（关闭遮罩也不能
  // 看到配置）。identity 未定时同样不挂载，避免权限判定前的空窗。
  const isPaymentTab = tab === 'payment'
  const complianceQuery = useQuery({
    queryKey: PAYMENT_COMPLIANCE_QUERY_KEY,
    queryFn: ({ signal }) => fetchPaymentCompliance(signal),
    enabled: isPaymentTab && canManage,
    retry: false,
    staleTime: 30_000,
  })
  const paymentNeedsGate =
    isPaymentTab &&
    canManage &&
    complianceQuery.isSuccess &&
    complianceQuery.data.confirmed !== true
  const paymentBlocked =
    isPaymentTab &&
    (meQuery.isPending ||
      (canManage && (complianceQuery.isPending || complianceQuery.isError || paymentNeedsGate)))

  const [dirtySections, setDirtySections] = useState<ReadonlySet<string>>(new Set())
  const [pendingTab, setPendingTab] = useState<SettingsTab | null>(null)

  const handleDirtyChange = useCallback((sectionId: string, dirty: boolean) => {
    setDirtySections((current) => {
      const has = current.has(sectionId)
      if (dirty === has) return current
      const next = new Set(current)
      if (dirty) next.add(sectionId)
      else next.delete(sectionId)
      return next
    })
  }, [])

  const hasUnsavedChanges = dirtySections.size > 0

  // Browser-level guard: the copy cannot be customized (UI design §2.4,
  // recorded as a known limitation), but the prompt must still appear.
  useEffect(() => {
    if (!hasUnsavedChanges) return
    const handler = (event: BeforeUnloadEvent): void => {
      event.preventDefault()
      // Older browsers only honour a non-empty returnValue.
      event.returnValue = ''
    }
    window.addEventListener('beforeunload', handler)
    return () => {
      window.removeEventListener('beforeunload', handler)
    }
  }, [hasUnsavedChanges])

  const activeTab = tabConfig(tab)
  const sections = useMemo(() => activeTab.sections, [activeTab])

  function requestTabChange(next: SettingsTab): void {
    if (next === tab) return
    if (hasUnsavedChanges) {
      setPendingTab(next)
      return
    }
    onTabChange(next)
  }

  function leaveAndDiscard(): void {
    const next = pendingTab
    setPendingTab(null)
    // The dirty cards unmount on the tab change and release their own flags,
    // but clearing here keeps the guard correct even if a section fails to
    // report its cleanup (e.g. a render error during unmount).
    setDirtySections(new Set())
    if (next !== null) onTabChange(next)
  }

  function handleTabKeyDown(event: KeyboardEvent<HTMLDivElement>): void {
    if (event.key !== 'ArrowRight' && event.key !== 'ArrowLeft' && event.key !== 'Home' && event.key !== 'End') {
      return
    }
    event.preventDefault()
    const index = SETTINGS_TABS.indexOf(tab)
    let nextIndex = index
    if (event.key === 'ArrowRight') nextIndex = (index + 1) % SETTINGS_TABS.length
    if (event.key === 'ArrowLeft') nextIndex = (index - 1 + SETTINGS_TABS.length) % SETTINGS_TABS.length
    if (event.key === 'Home') nextIndex = 0
    if (event.key === 'End') nextIndex = SETTINGS_TABS.length - 1
    const next = SETTINGS_TABS[nextIndex]
    if (next === undefined) return
    document.getElementById(`settings-tab-${next}`)?.focus()
    requestTabChange(next)
  }

  return (
    <div className="mx-auto flex max-w-[1440px] flex-col gap-4" data-testid="settings-shell">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold text-ink-1">{t('settings.shell.title')}</h1>
          <p className="mt-1 text-sm text-ink-2">{t('settings.shell.subtitle')}</p>
        </div>
        {hasUnsavedChanges ? (
          <p className="text-xs text-warning" role="status">
            {t('settings.shell.dirty')}
          </p>
        ) : null}
        {/*
         * 公告管理入口（F-08-04）：公告是独立列表页，不进本 Shell 的 tab。
         * 缺少 settings:manage 时不渲染——与页面 403 守卫同一权限口径。
         * 用普通 <a>（而非 Router Link）是为了让 Shell 仍能在无路由上下文
         * 的测试/嵌入场景渲染；它只做整页跳转，不参与 Shell 的脏标记。
         */}
        {canManage ? (
          <a
            href="/admin/settings/announcements"
            className="inline-flex h-9 items-center justify-center rounded-lg border border-border-default bg-surface-card px-3.5 text-sm font-medium text-ink-1 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {t('settings.shell.manage_announcements')}
          </a>
        ) : null}
      </header>

      {!canManage && !meQuery.isPending ? (
        <FormAlert tone="info">{t('settings.shell.read_only')}</FormAlert>
      ) : null}

      <div
        role="tablist"
        aria-label={t('settings.shell.tabs_label')}
        className="flex flex-wrap gap-1 border-b border-border-hairline"
        onKeyDown={handleTabKeyDown}
      >
        {SETTINGS_TABS_CONFIG.map((entry) => {
          const selected = entry.id === tab
          return (
            <button
              key={entry.id}
              id={`settings-tab-${entry.id}`}
              type="button"
              role="tab"
              aria-selected={selected}
              aria-controls={selected ? `settings-panel-${entry.id}` : undefined}
              tabIndex={selected ? 0 : -1}
              onClick={() => requestTabChange(entry.id)}
              className={[
                'relative h-10 rounded-t-lg px-3 text-[13px]',
                selected
                  ? 'border-b-2 border-brand font-medium text-ink-1'
                  : 'text-ink-2 hover:bg-surface-hover hover:text-ink-1',
              ].join(' ')}
            >
              {t(entry.labelKey)}
              {dirtySections.size > 0 && entry.sections.some((section) => dirtySections.has(section.id)) ? (
                <span aria-hidden="true" className="ml-1.5 inline-block size-1.5 rounded-full bg-warning" />
              ) : null}
            </button>
          )
        })}
      </div>

      <div
        role="tabpanel"
        id={`settings-panel-${tab}`}
        aria-labelledby={`settings-tab-${tab}`}
        className="flex flex-col gap-4"
      >
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-sm text-ink-2">{t(activeTab.descriptionKey)}</p>
          {isPaymentTab && canManage && complianceQuery.data?.confirmed ? (
            <StatusBadge variant="success">
              {t('settings.payment_compliance.confirmed_badge')}
            </StatusBadge>
          ) : null}
        </div>
        {paymentBlocked ? (
          paymentNeedsGate ? (
            <p role="status" className="text-sm text-ink-2">
              {t('settings.payment_compliance.blocked_hint')}
            </p>
          ) : complianceQuery.isError ? (
            <div className="flex items-center gap-3">
              <FormAlert tone="danger">
                {t('settings.payment_compliance.status_error')}
              </FormAlert>
              <Button
                variant="secondary"
                onClick={() => {
                  void complianceQuery.refetch()
                }}
              >
                {t('settings.shell.retry')}
              </Button>
            </div>
          ) : (
            <p role="status" className="text-sm text-ink-2">
              {t('settings.payment_compliance.status_loading')}
            </p>
          )
        ) : (
          sections.map((section) => (
            <SettingsSectionForm
              key={section.id}
              config={section}
              readOnly={!canManage}
              onDirtyChange={handleDirtyChange}
            />
          ))
        )}
        {/*
         * 支付 Tab 首次进入的合规确认（modules-08 §3.4）。取消/关闭由
         * onCancel 切到非支付 Tab，因此未确认时 section 永远不会挂载。
         */}
        {paymentNeedsGate ? (
          <PaymentComplianceGate
            open
            expectedText={t('settings.payment_compliance.expected_text')}
            onCancel={() => onTabChange('site')}
            onConfirmed={(status: PaymentComplianceStatus) => {
              queryClient.setQueryData(PAYMENT_COMPLIANCE_QUERY_KEY, status)
            }}
          />
        ) : null}
        {/*
         * 通知渠道内的「通知模板」入口卡（UI §6.9）：只做导航与覆盖概况，
         * 不是 settings section，因此不参与 section 级保存/脏标记。
         * 缺少 settings:manage 时卡片自身返回 null。
         */}
        {tab === 'notify' ? <NotifyEntryCard canManage={canManage} /> : null}
      </div>

      <Dialog
        open={pendingTab !== null}
        onOpenChange={(open) => {
          if (!open) setPendingTab(null)
        }}
        title={t('settings.shell.unsaved_title')}
        description={t('settings.shell.unsaved_body')}
        role="alertdialog"
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onClick={() => setPendingTab(null)}>
              {t('settings.shell.unsaved_keep')}
            </Button>
            <Button variant="danger" onClick={leaveAndDiscard}>
              {t('settings.shell.unsaved_discard')}
            </Button>
          </div>
        }
      />
    </div>
  )
}
