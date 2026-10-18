import { useCallback, useEffect, useRef, useState } from 'react'

export interface Countdown {
  /** Remaining whole seconds; 0 when idle. */
  seconds: number
  running: boolean
  start: (seconds: number) => void
  stop: () => void
}

/**
 * One-second countdown used by the email-code and reset-email resend buttons.
 *
 * The initial value is always supplied by the caller: the server's
 * `Retry-After` wins on a 429, otherwise the documented default applies. The
 * hook never invents a duration of its own.
 */
export function useCountdown(): Countdown {
  const [seconds, setSeconds] = useState(0)
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null)

  const stop = useCallback(() => {
    if (intervalRef.current !== null) {
      clearInterval(intervalRef.current)
      intervalRef.current = null
    }
  }, [])

  const start = useCallback(
    (value: number) => {
      stop()
      const normalized = Math.max(0, Math.ceil(value))
      setSeconds(normalized)
      if (normalized === 0) return
      intervalRef.current = setInterval(() => {
        setSeconds((current) => {
          if (current <= 1) {
            stop()
            return 0
          }
          return current - 1
        })
      }, 1000)
    },
    [stop],
  )

  // Unmount cleanup: a pending interval must not keep ticking into a dead tree.
  useEffect(() => stop, [stop])

  return { seconds, running: seconds > 0, start, stop }
}
