import { useEffect, useRef, useState } from 'react'

import { loadTurnstile, type TurnstileApi } from '@/lib/turnstile'
import { useI18n } from '@/shared/lib/i18n-context'

export interface TurnstileWidgetProps {
  /** Public site key from `GET /api/status` (`auth.turnstile.site_key`). */
  siteKey: string
  /** Receives the fresh token, or `undefined` on error/expiry/consumption. */
  onToken: (token: string | undefined) => void
  /**
   * Increment to drop the current (single-use) token and ask for a new one.
   * The parent must also clear its own token state; `reset` alone only
   * restarts the challenge.
   */
  resetSignal?: number
}

/**
 * Explicitly rendered Turnstile widget. Every token is single-use: the caller
 * resets after each request attempt so a failed login (or a 429) does not
 * leave a consumed token in state that would make the next submit fail.
 */
export function TurnstileWidget({
  siteKey,
  onToken,
  resetSignal = 0,
}: TurnstileWidgetProps) {
  const { t } = useI18n()
  const containerRef = useRef<HTMLDivElement>(null)
  const widgetIdRef = useRef<string | undefined>(undefined)
  const onTokenRef = useRef(onToken)
  // Distinguish "script never loaded" from "the challenge itself failed": the
  // user-facing advice differs (refresh vs retry the challenge).
  const [failure, setFailure] = useState<'load' | 'challenge' | undefined>(undefined)

  // Parent callbacks are inline arrows; route them through a ref so the
  // render/reset lifecycle is keyed on the key and signal only.
  useEffect(() => {
    onTokenRef.current = onToken
  }, [onToken])

  useEffect(() => {
    let cancelled = false
    let widgetId: string | undefined

    void loadTurnstile()
      .then((api: TurnstileApi) => {
        if (cancelled || containerRef.current === null) return
        widgetId = api.render(containerRef.current, {
          sitekey: siteKey,
          callback: (token) => {
            setFailure(undefined)
            onTokenRef.current(token)
          },
          'error-callback': () => {
            // Tell the user the challenge failed instead of silently leaving a
            // submit button that looks ready but cannot work.
            setFailure('challenge')
            onTokenRef.current(undefined)
          },
          'expired-callback': () => {
            onTokenRef.current(undefined)
          },
          theme: 'auto',
        })
        widgetIdRef.current = widgetId
      })
      .catch(() => {
        if (!cancelled) {
          setFailure('load')
          onTokenRef.current(undefined)
        }
      })

    return () => {
      cancelled = true
      if (widgetId !== undefined) window.turnstile?.remove(widgetId)
      if (widgetIdRef.current === widgetId) widgetIdRef.current = undefined
    }
  }, [siteKey])

  useEffect(() => {
    if (resetSignal === 0) return
    const widgetId = widgetIdRef.current
    if (widgetId !== undefined) window.turnstile?.reset(widgetId)
  }, [resetSignal])

  return (
    <div className="flex flex-col gap-1.5">
      <div ref={containerRef} />
      {failure !== undefined ? (
        <p role="alert" className="text-meta text-danger">
          {failure === 'load'
            ? t('auth.turnstile.unavailable')
            : t('auth.turnstile.failed')}
        </p>
      ) : null}
    </div>
  )
}
