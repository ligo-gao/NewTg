import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { useToastStore } from '@/shared/ui/toast'
import { Button } from '@/shared/ui/button'
import { StatusBadge } from '@/shared/ui/status-badge'

import { SignOutOthersDialog } from './SignOutOthersDialog'

const SESSIONS_KEY = ['sessions'] as const

export interface SessionsCardProps {
  className?: string
}

/**
 * F-01-13/F-01-14 session list. Revoking one session is immediate (no data
 * loss, per the design); revoking all others goes through the confirm dialog.
 */
export function SessionsCard({ className }: SessionsCardProps) {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const sessionsQuery = useQuery({
    queryKey: SESSIONS_KEY,
    queryFn: async () => requireData(await apiClient.GET('/api/sessions')),
  })

  const revoke = useMutation({
    mutationFn: async (sid: string) => {
      await apiClient.DELETE('/api/sessions/{sid}', { params: { path: { sid } } })
    },
    onSuccess: async () => {
      push({ tone: 'success', message: t('account.sessions.revoked') })
      await queryClient.invalidateQueries({ queryKey: SESSIONS_KEY })
    },
    onError: () => {
      push({ tone: 'error', message: t('account.sessions.revoke_error') })
    },
  })

  const sessions = sessionsQuery.data?.sessions ?? []
  const others = sessions.filter((session) => !session.current)

  return (
    <section
      className={[
        'rounded-xl border border-border-hairline bg-surface-card p-5',
        className,
      ]
        .filter(Boolean)
        .join(' ')}
      aria-labelledby="sessions-title"
    >
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 id="sessions-title" className="text-base font-semibold text-ink-1">
            {t('account.sessions.title')}
          </h2>
          <p className="mt-1 text-[13px] text-ink-2">
            {t('account.sessions.description')}
          </p>
        </div>
        <SignOutOthersDialog
          count={others.length}
          trigger={
            <Button
              variant="secondary"
              disabled={others.length === 0}
              className="shrink-0"
            >
              {t('account.sessions.sign_out_other')}
            </Button>
          }
        />
      </div>

      {sessionsQuery.isPending ? (
        <div className="mt-4 space-y-2" aria-busy="true">
          {[0, 1, 2].map((index) => (
            <div key={index} className="h-16 animate-pulse rounded-lg bg-surface-subtle" />
          ))}
        </div>
      ) : sessionsQuery.isError ? (
        <div className="mt-4 flex items-center gap-3 text-sm text-ink-2">
          <span>{t('account.sessions.error')}</span>
          <Button variant="secondary" onClick={() => void sessionsQuery.refetch()}>
            {t('common.action.retry')}
          </Button>
        </div>
      ) : sessions.length === 0 ? (
        <p className="mt-4 text-sm text-ink-2">{t('account.sessions.empty')}</p>
      ) : (
        <ul role="list" className="mt-4 divide-y divide-border-hairline">
          {sessions.map((session) => (
            <li
              key={session.id}
              className="flex flex-col gap-2 py-3 sm:flex-row sm:items-center sm:justify-between"
            >
              <div className="min-w-0">
                <div className="flex items-center gap-2">
                  <span className="truncate text-sm font-medium text-ink-1">
                    {session.user_agent.slice(0, 48)}
                  </span>
                  {session.current ? (
                    <StatusBadge variant="brand">
                      {t('account.sessions.current')}
                    </StatusBadge>
                  ) : null}
                </div>
                <p className="mt-0.5 font-mono text-xs tabular-nums text-ink-3">
                  {session.ip} ·{' '}
                  {t('account.sessions.last_active', {
                    time: formatRelativeTime(session.last_seen_at, locale),
                  })}{' '}
                  ·{' '}
                  {t('account.sessions.signed_in', {
                    time: formatRelativeTime(session.created_at, locale),
                  })}
                </p>
              </div>
              {!session.current ? (
                <Button
                  variant="ghost"
                  className="self-end text-danger hover:bg-danger-bg sm:self-auto"
                  disabled={revoke.isPending && revoke.variables === session.id}
                  onClick={() => {
                    revoke.mutate(session.id)
                  }}
                >
                  {t('account.sessions.sign_out')}
                </Button>
              ) : null}
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
