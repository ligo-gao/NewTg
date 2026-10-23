import { useEffect, useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert, TextField } from '@/shared/ui/form'
import { StatusBadge } from '@/shared/ui/status-badge'
import { Tag } from '@/shared/ui/tag'
import { useToastStore } from '@/shared/ui/toast'

import { LanguageCard } from './LanguageCard'

const ME_KEY = ['me'] as const

/** F-01-18 profile page. Only `display_name` is writable (BOPLA freeze). */
export function ProfilePage() {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const meQuery = useQuery({
    queryKey: ME_KEY,
    queryFn: async () => requireData(await apiClient.GET('/api/me')),
  })
  const [displayName, setDisplayName] = useState('')
  const [error, setError] = useState<string | undefined>(undefined)

  const me = meQuery.data
  useEffect(() => {
    // Re-seed only when the server projection changes (initial load or a
    // successful save), never on every keystroke.
    setDisplayName(me?.display_name ?? '')
  }, [me])

  const update = useMutation({
    mutationFn: async (name: string) => {
      await apiClient.PATCH('/api/me', {
        body: { display_name: name.trim() === '' ? null : name.trim() },
      })
    },
    onSuccess: async () => {
      push({ tone: 'success', message: t('account.profile.saved') })
      await queryClient.invalidateQueries({ queryKey: ME_KEY })
    },
    onError: () => {
      setError(t('account.profile.save_error'))
    },
  })

  function handleSubmit(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault()
    setError(undefined)
    update.mutate(displayName)
  }

  if (meQuery.isPending) {
    return <p className="text-sm text-ink-2">{t('account.profile.loading')}</p>
  }
  if (meQuery.isError || me === undefined) {
    return (
      <div className="flex items-center gap-3 text-sm text-ink-2">
        <span>{t('account.profile.error')}</span>
        <Button variant="secondary" onClick={() => void meQuery.refetch()}>
          {t('common.action.retry')}
        </Button>
      </div>
    )
  }

  const dirty = displayName !== (me.display_name ?? '')

  return (
    <div className="mx-auto flex max-w-[1520px] flex-col gap-4">
      <section className="rounded-xl border border-border-hairline bg-surface-card p-5">
        <div className="flex items-center gap-3">
          <span
            aria-hidden="true"
            className="flex size-16 items-center justify-center rounded-full bg-brand-subtle text-xl font-semibold text-brand"
          >
            {(me.display_name ?? me.username).slice(0, 1).toUpperCase()}
          </span>
          <div>
            <h1 className="text-xl font-semibold text-ink-1">
              {me.display_name ?? me.username}
            </h1>
            <p className="font-mono text-[13px] text-ink-2">@{me.username}</p>
          </div>
        </div>
      </section>

      <section className="rounded-xl border border-border-hairline bg-surface-card p-5">
        <h2 className="text-base font-semibold text-ink-1">
          {t('account.profile.title')}
        </h2>

        <dl className="mt-4 grid grid-cols-1 gap-4 text-sm sm:grid-cols-2">
          <div>
            <dt className="text-[13px] text-ink-2">{t('account.profile.username')}</dt>
            <dd className="mt-0.5 font-mono text-ink-1">@{me.username}</dd>
          </div>
          <div>
            <dt className="text-[13px] text-ink-2">{t('account.profile.email')}</dt>
            <dd className="mt-0.5 text-ink-1">{me.email ?? '—'}</dd>
          </div>
          <div>
            <dt className="text-[13px] text-ink-2">{t('account.profile.role')}</dt>
            <dd className="mt-1">
              <Tag variant={me.role === 'admin' ? 'admin' : 'neutral'}>
                {me.role === 'admin'
                  ? t('account.profile.role_admin')
                  : t('account.profile.role_user')}
              </Tag>
            </dd>
          </div>
          <div>
            <dt className="text-[13px] text-ink-2">{t('account.profile.status')}</dt>
            <dd className="mt-1">
              <StatusBadge variant={me.status === 'enabled' ? 'success' : 'neutral'}>
                {me.status === 'enabled'
                  ? t('account.profile.status_enabled')
                  : t('account.profile.status_disabled')}
              </StatusBadge>
            </dd>
          </div>
        </dl>

        <form
          className="mt-6 flex max-w-[420px] flex-col gap-3"
          onSubmit={handleSubmit}
        >
          {error !== undefined ? <FormAlert>{error}</FormAlert> : null}
          <TextField
            label={t('account.profile.display_name')}
            hint={t('account.profile.display_name_hint')}
            maxLength={32}
            value={displayName}
            onChange={(event) => {
              setDisplayName(event.target.value)
            }}
          />
          <Button
            type="submit"
            className="self-start"
            disabled={!dirty || update.isPending}
          >
            {update.isPending ? t('account.profile.saving') : t('account.profile.save')}
          </Button>
        </form>
      </section>

      {/* F-01-19：语言偏好即时保存（modules-01 §界面语言偏好）。 */}
      <LanguageCard />
    </div>
  )
}
