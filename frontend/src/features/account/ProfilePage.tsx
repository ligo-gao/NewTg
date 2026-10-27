import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState, type FormEvent, type ReactNode } from 'react'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { AccountEmailDialog } from '@/features/account/AccountEmailDialog'
import { topupConfigQueryOptions, walletSummaryQueryOptions } from '@/features/account/topup/queries'
import { formatYuan } from '@/features/account/topup/money'
import { listTokens } from '@/features/tokens/api'
import { publicStatusQueryOptions } from '@/lib/public-status'
import {
  SELF_SETTINGS_QUERY_KEY,
  selfSettingsQueryOptions,
} from '@/lib/self-settings'
import { useLanguageSelection } from '@/lib/use-language-selection'
import { useI18n, type Locale } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'
import { StatusBadge } from '@/shared/ui/status-badge'
import { Tag } from '@/shared/ui/tag'
import { useToastStore } from '@/shared/ui/toast'

const ME_KEY = ['me'] as const
const SESSIONS_KEY = ['sessions'] as const
const NOTIFY_KEY = ['notification-preferences'] as const
const TOKEN_COUNT_KEY = ['tokens', 'count'] as const

type NotificationChannel = 'email' | 'webhook' | 'bark' | 'gotify'

interface NotificationDraft {
  balanceEnabled: boolean
  threshold: string
  channels: NotificationChannel[]
  digestEnabled: boolean
  webhook: string
}

const CHANNELS: NotificationChannel[] = ['email', 'webhook', 'bark', 'gotify']

const cardClass = 'rounded-xl border border-border-hairline bg-surface-card'
const selectClass =
  'h-8 rounded-md border border-border-default bg-surface-card px-2 text-badge text-ink-1'
const inputClass =
  'h-8 rounded-md border border-border-default bg-surface-card px-2 text-badge text-ink-1'

function formatJoined(value: string, locale: string): string {
  if (value === '') return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '—'
  return new Intl.DateTimeFormat(locale, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
  }).format(date)
}

function Toggle({
  checked,
  onChange,
  label,
  disabled = false,
}: {
  checked: boolean
  onChange: (next: boolean) => void
  label: string
  disabled?: boolean
}) {
  return (
    <label className="relative inline-flex h-5 w-9 flex-none items-center">
      <input
        type="checkbox"
        className="peer sr-only"
        checked={checked}
        disabled={disabled}
        onChange={(event) => onChange(event.target.checked)}
        aria-label={label}
      />
      <span className="h-5 w-9 rounded-full bg-border-default transition-colors peer-checked:bg-brand peer-disabled:opacity-50 peer-focus-visible:shadow-[var(--focus-ring)]" />
      <span className="pointer-events-none absolute left-0.5 top-0.5 size-4 rounded-full bg-white shadow transition-transform peer-checked:translate-x-4" />
    </label>
  )
}

function Row({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) {
  return (
    <div className="grid flex-1 gap-2 border-t border-border-hairline px-5 py-3.5 first:border-t-0 min-[760px]:grid-cols-[minmax(0,1fr)_auto] min-[760px]:items-center">
      <div>
        <div className="text-badge text-ink-2">{label}</div>
        {hint !== undefined ? <div className="mt-0.5 text-badge text-ink-3">{hint}</div> : null}
      </div>
      <div className="flex flex-wrap items-center justify-end gap-2">{children}</div>
    </div>
  )
}

export function ProfilePage() {
  const { t, locale, setLocale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  // 与公开页的 LanguageSwitch 共用同一条持久化路径：服务端写入失败时保留
  // 本地语言并给出「仅保存在本机」提示，绝不回滚（回滚会伪造用户没做过的
  // 手势，写入 explicit/pending 标记）。
  const { onChange: persistLanguage } = useLanguageSelection(true)
  const [languagePending, setLanguagePending] = useState(false)

  const meQuery = useQuery({
    queryKey: ME_KEY,
    queryFn: async () => requireData(await apiClient.GET('/api/me')),
  })
  const selfQuery = useQuery(selfSettingsQueryOptions())
  const walletQuery = useQuery(walletSummaryQueryOptions())
  const topupConfigQuery = useQuery(topupConfigQueryOptions())
  const statusQuery = useQuery(publicStatusQueryOptions())
  const tokensQuery = useQuery({
    queryKey: TOKEN_COUNT_KEY,
    queryFn: async () => (await listTokens({ page: 1, pageSize: 1 })).total,
    staleTime: 30_000,
  })
  const enabledTokensQuery = useQuery({
    queryKey: [...TOKEN_COUNT_KEY, 'enabled'] as const,
    queryFn: async () =>
      (await listTokens({ page: 1, pageSize: 1, status: 'enabled' })).total,
    staleTime: 30_000,
  })
  const sessionsQuery = useQuery({
    queryKey: SESSIONS_KEY,
    queryFn: async () => requireData(await apiClient.GET('/api/sessions')),
    staleTime: 30_000,
  })
  const prefsQuery = useQuery({
    queryKey: NOTIFY_KEY,
    queryFn: async () => requireData(await apiClient.GET('/api/notification-preferences')),
    staleTime: 30_000,
  })

  const me = meQuery.data
  const settings = selfQuery.data
  const wallet = walletQuery.data
  const quotaPerUnit = topupConfigQuery.data?.quota_per_unit ?? 100

  const [editingName, setEditingName] = useState(false)
  const [nameDraft, setNameDraft] = useState('')
  const [nameError, setNameError] = useState<string | undefined>(undefined)
  const [emailOpen, setEmailOpen] = useState(false)
  const [notify, setNotify] = useState<NotificationDraft>({
    balanceEnabled: false,
    threshold: '1000',
    channels: ['email'],
    digestEnabled: true,
    webhook: '',
  })

  useEffect(() => {
    const prefs = prefsQuery.data
    setNotify({
      balanceEnabled: prefs?.balance_warning?.enabled ?? false,
      threshold: String(prefs?.balance_warning?.threshold ?? 1000),
      channels: (prefs?.balance_warning?.channels ?? ['email']) as NotificationChannel[],
      digestEnabled: prefs?.usage_digest?.enabled ?? true,
      webhook: prefs?.webhook_url ?? '',
    })
  }, [prefsQuery.data])

  useEffect(() => {
    if (!editingName) setNameDraft(me?.display_name ?? '')
  }, [me?.display_name, editingName])

  const updateName = useMutation({
    mutationFn: async (name: string) => {
      await apiClient.PATCH('/api/me', {
        body: { display_name: name.trim() === '' ? null : name.trim() },
      })
    },
    onSuccess: async () => {
      setNameError(undefined)
      setEditingName(false)
      push({ tone: 'success', message: t('account.profile.display_name_saved') })
      await queryClient.invalidateQueries({ queryKey: ME_KEY })
    },
    onError: () => setNameError(t('account.profile.save_error')),
  })

  const updateSetting = useMutation({
    mutationFn: async (patch: {
      language?: Locale
      default_model?: string
      record_ip_log?: boolean
    }) => {
      if (settings === undefined) throw new Error('settings not loaded')
      return requireData(
        await apiClient.PUT('/api/self/setting', {
          body: { ...patch, updated_at: settings.updated_at },
        }),
      )
    },
    onSuccess: (updated) => {
      queryClient.setQueryData(SELF_SETTINGS_QUERY_KEY, updated)
    },
    onError: async () => {
      push({ tone: 'error', message: t('account.profile.save_error') })
      await queryClient.invalidateQueries({ queryKey: SELF_SETTINGS_QUERY_KEY })
    },
  })

  const saveNotifications = useMutation({
    mutationFn: async () => {
      const threshold = Number(notify.threshold)
      return requireData(
        await apiClient.PUT('/api/notification-preferences', {
          body: {
            balance_warning: {
              enabled: notify.balanceEnabled,
              threshold: Number.isFinite(threshold) && threshold >= 0 ? threshold : 0,
              channels: notify.channels,
            },
            usage_digest: { enabled: notify.digestEnabled, channels: notify.channels },
            security_alert: { enabled: true },
            webhook_url: notify.webhook.trim() === '' ? null : notify.webhook.trim(),
          },
        }),
      )
    },
    onSuccess: (updated) => {
      queryClient.setQueryData(NOTIFY_KEY, updated)
      push({ tone: 'success', message: t('account.profile.notifications_saved') })
    },
    onError: () => push({ tone: 'error', message: t('account.profile.notifications_error') }),
  })

  const signOutOthers = useMutation({
    mutationFn: async () => {
      await apiClient.POST('/api/sessions/revoke-others', {})
    },
    onSuccess: async () => {
      push({ tone: 'success', message: t('account.profile.signed_out') })
      await queryClient.invalidateQueries({ queryKey: SESSIONS_KEY })
    },
  })

  function startEditingName(): void {
    setNameDraft(me?.display_name ?? '')
    setNameError(undefined)
    setEditingName(true)
  }

  function submitName(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault()
    updateName.mutate(nameDraft)
  }

  async function changeLanguage(next: Locale): Promise<void> {
    // 先切换运行时（用户手势），再交给共享 hook 持久化；失败不回滚。
    setLocale?.(next)
    setLanguagePending(true)
    try {
      await persistLanguage?.(next)
    } finally {
      setLanguagePending(false)
    }
  }

  function toggleChannel(channel: NotificationChannel, enabled: boolean): void {
    setNotify((current) => ({
      ...current,
      channels: enabled
        ? Array.from(new Set([...current.channels, channel]))
        : current.channels.filter((item) => item !== channel),
    }))
  }

  if (meQuery.isPending) {
    return <p className="text-body text-ink-2">{t('account.profile.loading')}</p>
  }
  if (meQuery.isError || me === undefined) {
    return (
      <div className="flex items-center gap-3 text-body text-ink-2">
        <span>{t('account.profile.error')}</span>
        <Button variant="secondary" onClick={() => void meQuery.refetch()}>
          {t('common.action.retry')}
        </Button>
      </div>
    )
  }

  const displayName = me.display_name ?? me.username
  const initial = displayName.slice(0, 1).toUpperCase()
  const emailVerified = me.email_verified
  const joined = formatJoined(me.created_at, locale)
  const sessionsCount = sessionsQuery.data?.sessions.length
  const modelOptions = Array.from(
    new Set((statusQuery.data?.usable_groups ?? []).flatMap((group) => group.models)),
  ).sort()
  const currentModel = settings?.default_model ?? ''
  if (currentModel !== '' && !modelOptions.includes(currentModel)) {
    modelOptions.unshift(currentModel)
  }

  return (
    <div className="mx-auto flex w-full max-w-[1520px] flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-page-title text-ink-1">{t('account.profile.center_title')}</h1>
          <p className="mt-1 max-w-[48em] text-meta text-ink-2">
            {t('account.profile.center_subtitle')}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button
            variant="secondary"
            className="h-8 px-3 text-body"
            onClick={() => window.location.assign('/wallet')}
          >
            {t('account.profile.quick_wallet')}
          </Button>
          <Button
            className="h-8 px-3 text-body"
            onClick={() => window.location.assign('/security')}
          >
            {t('account.profile.full_security')}
          </Button>
        </div>
      </header>

      <div className="flex flex-col gap-4">
        <section
          className={`${cardClass} flex flex-wrap items-center gap-x-8 gap-y-4 p-5`}
          aria-label={t('account.profile.title')}
        >
          <div className="flex min-w-0 items-center gap-3">
            {me.avatar_url !== null ? (
              <img
                src={me.avatar_url}
                alt=""
                className="size-12 flex-none rounded-full border border-border-hairline object-cover"
              />
            ) : (
              <span
                aria-hidden="true"
                className="flex size-12 flex-none items-center justify-center rounded-full bg-brand-subtle text-section text-brand"
              >
                {initial}
              </span>
            )}
            <div className="min-w-0">
              <h2 className="truncate text-section text-ink-1">{displayName}</h2>
              <p className="truncate font-mono text-badge text-ink-3">@{me.username}</p>
              <div className="mt-1.5 flex flex-wrap gap-1">
                <StatusBadge variant={me.status === 'enabled' ? 'success' : 'neutral'}>
                  {me.status === 'enabled'
                    ? t('account.profile.status_enabled')
                    : t('account.profile.status_disabled')}
                </StatusBadge>
                <Tag variant={emailVerified ? 'success' : 'neutral'}>
                  {emailVerified
                    ? t('account.profile.email_verified')
                    : t('account.profile.email_unverified')}
                </Tag>
              </div>
            </div>
          </div>
          <dl className="flex flex-wrap gap-x-8 gap-y-3 text-badge min-[760px]:ml-auto">
            <div className="flex min-w-0 flex-col gap-0.5">
              <dt className="text-badge text-ink-3">{t('account.profile.email')}</dt>
              <dd className="min-w-0 truncate text-ink-1">{me.email ?? '—'}</dd>
            </div>
            <div className="flex flex-col gap-0.5">
              <dt className="text-badge text-ink-3">{t('account.profile.joined_at')}</dt>
              <dd className="font-mono text-badge text-ink-1">{joined}</dd>
            </div>
            <div className="flex flex-col gap-0.5">
              <dt className="text-badge text-ink-3">{t('account.language.label')}</dt>
              <dd className="text-ink-1">
                {t(`account.language.name_${locale.toLowerCase().replace(/-/g, '_')}`)}
              </dd>
            </div>
          </dl>
        </section>

        <section
          className={`${cardClass} grid grid-cols-2 overflow-hidden min-[900px]:grid-cols-4`}
          aria-label={t('account.profile.overview')}
        >
          {[
            {
              label: t('account.profile.balance'),
              value: wallet === undefined ? '—' : formatYuan(Math.round((wallet.balance * 100) / quotaPerUnit), { locale }),
              note: `${t('account.profile.held')} ${wallet === undefined ? '—' : formatYuan(Math.round((wallet.held_credits * 100) / quotaPerUnit), { locale })}`,
            },
            {
              label: t('account.profile.consumed'),
              value: wallet === undefined ? '—' : formatYuan(Math.round((wallet.total_consumed * 100) / quotaPerUnit), { locale }),
              note: '',
            },
            {
              label: t('account.profile.requests'),
              value: wallet === undefined ? '—' : new Intl.NumberFormat(locale).format(wallet.total_requests),
              note: '',
            },
            {
              label: t('account.profile.tokens'),
              value: tokensQuery.data === undefined ? '—' : new Intl.NumberFormat(locale).format(tokensQuery.data),
              note:
                enabledTokensQuery.data === undefined
                  ? ''
                  : t('account.profile.tokens_enabled', {
                      count: new Intl.NumberFormat(locale).format(enabledTokensQuery.data),
                    }),
            },
          ].map((stat, index) => (
            <div
              key={stat.label}
              className={`px-5 py-4 ${
                index === 1 ? 'border-l border-border-hairline' : ''
              } ${index === 2 ? 'max-[899px]:border-t min-[900px]:border-l' : ''} ${
                index === 3 ? 'border-l border-border-hairline max-[899px]:border-t' : ''
              }`}
            >
              <div className="text-badge text-ink-3">{stat.label}</div>
              <div className="mt-1 font-mono text-stat tracking-[-.02em] text-ink-1">
                {stat.value}
              </div>
              {stat.note !== '' ? <div className="mt-0.5 text-badge text-ink-4">{stat.note}</div> : null}
            </div>
          ))}
        </section>

        <div className="flex flex-col gap-4">
          <section className={`${cardClass} flex flex-col`} aria-labelledby="profile-basic-title">
            <div className="border-b border-border-hairline px-5 py-4">
              <h2 id="profile-basic-title" className="text-section text-ink-1">
                {t('account.profile.basic_title')}
              </h2>
              <p className="mt-0.5 text-badge text-ink-3">{t('account.profile.basic_desc')}</p>
            </div>
            <Row label={t('account.profile.display_name')} hint={t('account.profile.display_name_hint')}>
              {editingName ? (
                <form className="flex flex-wrap items-center gap-2" onSubmit={submitName}>
                  {nameError !== undefined ? <FormAlert>{nameError}</FormAlert> : null}
                  <input
                    className={inputClass}
                    maxLength={32}
                    value={nameDraft}
                    onChange={(event) => setNameDraft(event.target.value)}
                    aria-label={t('account.profile.display_name')}
                  />
                  <Button type="submit" className="h-8 px-3 text-body" disabled={updateName.isPending}>
                    {updateName.isPending ? t('account.profile.saving') : t('account.profile.save')}
                  </Button>
                  <Button
                    variant="secondary"
                    type="button"
                    className="h-8 px-3 text-body"
                    onClick={() => setEditingName(false)}
                  >
                    {t('account.profile.cancel')}
                  </Button>
                </form>
              ) : (
                <>
                  <span className="text-badge text-ink-1">{displayName}</span>
                  <Button variant="secondary" className="h-8 px-3 text-body" onClick={startEditingName}>
                    {t('account.profile.edit')}
                  </Button>
                </>
              )}
            </Row>
            <Row label={t('account.profile.username')}>
              <span className="font-mono text-badge text-ink-1">@{me.username}</span>
            </Row>
            <Row label={t('account.profile.email')}>
              <span className="text-badge text-ink-1">{me.email ?? '—'}</span>
              <Tag variant={emailVerified ? 'success' : 'neutral'}>
                {emailVerified
                  ? t('account.profile.email_verified')
                  : t('account.profile.email_unverified')}
              </Tag>
              <Button variant="secondary" className="h-8 px-3 text-body" onClick={() => setEmailOpen(true)}>
                {t('account.profile.change_email')}
              </Button>
            </Row>
            <Row label={t('account.profile.role_status')}>
              <Tag variant={me.role === 'admin' ? 'admin' : 'neutral'}>
                {me.role === 'admin'
                  ? t('account.profile.role_admin')
                  : t('account.profile.role_user')}
              </Tag>
              <StatusBadge variant={me.status === 'enabled' ? 'success' : 'neutral'}>
                {me.status === 'enabled'
                  ? t('account.profile.status_enabled')
                  : t('account.profile.status_disabled')}
              </StatusBadge>
            </Row>
          </section>

          <section className={`${cardClass} flex flex-col`} aria-labelledby="profile-prefs-title">
            <div className="border-b border-border-hairline px-5 py-4">
              <h2 id="profile-prefs-title" className="text-section text-ink-1">
                {t('account.profile.prefs_title')}
              </h2>
              <p className="mt-0.5 text-badge text-ink-3">{t('account.profile.prefs_desc')}</p>
            </div>
            <Row label={t('account.language.label')} hint={t('account.profile.language_help')}>
              <select
                className={selectClass}
                value={locale}
                onChange={(event) => void changeLanguage(event.target.value as Locale)}
                disabled={settings === undefined || languagePending}
                aria-label={t('account.language.label')}
              >
                {(['zh-CN', 'en', 'zh-TW', 'fr', 'ru', 'ja', 'vi'] as const).map((code) => (
                  <option key={code} value={code}>
                    {t(`account.language.name_${code.toLowerCase().replace(/-/g, '_')}`)}
                  </option>
                ))}
              </select>
            </Row>
            <Row label={t('account.profile.default_model')} hint={t('account.profile.default_model_help')}>
              <select
                className={`${selectClass} max-w-[260px]`}
                value={currentModel}
                onChange={(event) => updateSetting.mutate({ default_model: event.target.value })}
                disabled={settings === undefined || updateSetting.isPending}
                aria-label={t('account.profile.default_model')}
              >
                <option value="">{t('account.profile.default_model_none')}</option>
                {modelOptions.map((model) => (
                  <option key={model} value={model}>
                    {model}
                  </option>
                ))}
              </select>
            </Row>
            <Row label={t('account.profile.record_ip')} hint={t('account.profile.record_ip_help')}>
              <Toggle
                checked={settings?.record_ip_log ?? false}
                label={t('account.profile.record_ip')}
                disabled={settings === undefined || updateSetting.isPending}
                onChange={(next) => updateSetting.mutate({ record_ip_log: next })}
              />
            </Row>
            <div className="flex items-center justify-between gap-3 border-t border-border-hairline px-5 py-3 text-badge text-ink-4">
              <span>{t('account.profile.prefs_note')}</span>
            </div>
          </section>

          <section className={`${cardClass} flex flex-col`} aria-labelledby="profile-notify-title">
            <div className="border-b border-border-hairline px-5 py-4">
              <h2 id="profile-notify-title" className="text-section text-ink-1">
                {t('account.profile.notify_title')}
              </h2>
              <p className="mt-0.5 text-badge text-ink-3">{t('account.profile.notify_desc')}</p>
            </div>
            <Row label={t('account.profile.balance_warning')} hint={t('account.profile.balance_warning_help')}>
              <Toggle
                checked={notify.balanceEnabled}
                label={t('account.profile.balance_warning')}
                onChange={(next) => setNotify((current) => ({ ...current, balanceEnabled: next }))}
              />
              <input
                className={`${inputClass} w-24 font-mono`}
                value={notify.threshold}
                onChange={(event) => setNotify((current) => ({ ...current, threshold: event.target.value }))}
                aria-label={t('account.profile.threshold')}
                inputMode="numeric"
              />
              <span className="font-mono text-badge text-ink-3">{t('account.profile.threshold')}</span>
            </Row>
            <Row label={t('account.profile.channels')}>
              {CHANNELS.map((channel) => (
                <label key={channel} className="inline-flex items-center gap-1.5 text-badge text-ink-2">
                  <input
                    type="checkbox"
                    className="size-3.5 accent-[var(--brand)]"
                    checked={notify.channels.includes(channel)}
                    onChange={(event) => toggleChannel(channel, event.target.checked)}
                  />
                  {t(`account.profile.channel_${channel}`)}
                </label>
              ))}
            </Row>
            <Row label={t('account.profile.usage_digest')} hint={t('account.profile.usage_digest_help')}>
              <Toggle
                checked={notify.digestEnabled}
                label={t('account.profile.usage_digest')}
                onChange={(next) => setNotify((current) => ({ ...current, digestEnabled: next }))}
              />
            </Row>
            <Row label={t('account.profile.security_alert')} hint={t('account.profile.security_alert_help')}>
              <Toggle checked label={t('account.profile.security_alert')} onChange={() => undefined} disabled />
              <span className="text-badge text-ink-4">{t('account.profile.forced')}</span>
            </Row>
            <Row label={t('account.profile.webhook_url')} hint={t('account.profile.webhook_url_help')}>
              <input
                className={`${inputClass} w-full max-w-[320px] font-mono text-badge`}
                value={notify.webhook}
                onChange={(event) => setNotify((current) => ({ ...current, webhook: event.target.value }))}
                placeholder="https://example.com/hooks/newtg"
                aria-label={t('account.profile.webhook_url')}
              />
            </Row>
            <div className="flex items-center justify-between gap-3 border-t border-border-hairline bg-surface-hover px-5 py-3">
              <span className="text-badge text-ink-4">{t('account.profile.notify_desc')}</span>
              <Button
                className="h-8 px-3 text-body"
                disabled={saveNotifications.isPending}
                onClick={() => saveNotifications.mutate()}
              >
                {t('account.profile.save_notifications')}
              </Button>
            </div>
          </section>

          <section className={`${cardClass} flex flex-col`} aria-labelledby="profile-security-title">
            <div className="flex items-start justify-between gap-3 border-b border-border-hairline px-5 py-4">
              <div>
                <h2 id="profile-security-title" className="text-section text-ink-1">
                  {t('account.profile.security_title')}
                </h2>
                <p className="mt-0.5 text-badge text-ink-3">{t('account.profile.security_desc')}</p>
              </div>
              <Button
                variant="secondary"
                className="h-8 px-3 text-body"
                onClick={() => window.location.assign('/security')}
              >
                {t('account.profile.full_security')}
              </Button>
            </div>
            <Row label={t('account.profile.password')}>
              <Button variant="secondary" className="h-8 px-3 text-body" onClick={() => window.location.assign('/security')}>
                {t('account.profile.change_password')}
              </Button>
            </Row>
            <Row label={t('account.profile.two_fa')}>
              <Button variant="secondary" className="h-8 px-3 text-body" onClick={() => window.location.assign('/security')}>
                {t('account.profile.manage')}
              </Button>
            </Row>
            <Row
              label={t('account.profile.sessions')}
              hint={
                sessionsCount === undefined
                  ? undefined
                  : t('account.profile.sessions_help', { count: String(sessionsCount) })
              }
            >
              <Button variant="secondary" className="h-8 px-3 text-body" onClick={() => window.location.assign('/security')}>
                {t('account.profile.manage_sessions')}
              </Button>
              <Button
                variant="ghost"
                className="h-8 px-3 text-body"
                disabled={signOutOthers.isPending}
                onClick={() => signOutOthers.mutate()}
              >
                {t('account.profile.sign_out_others')}
              </Button>
            </Row>
          </section>
        </div>
      </div>

      <AccountEmailDialog
        open={emailOpen}
        onOpenChange={setEmailOpen}
        currentEmail={me.email ?? null}
        onChanged={() => {
          void queryClient.invalidateQueries({ queryKey: ME_KEY })
        }}
      />
    </div>
  )
}
