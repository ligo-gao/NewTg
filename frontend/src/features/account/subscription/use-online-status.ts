import { useEffect, useState } from 'react'

/**
 * Browser online signal for the subscription section.
 *
 * The error pages expose the same idea from `app/error-page-shell`, but a
 * feature must not import `app/` (web-00 §2.2 layering). Keeping a local hook
 * also avoids pulling the public footer/header graph into the wallet chunk.
 *
 * `navigator.onLine === false` is the only reliable offline signal available
 * before a request is attempted (web-00 §8.5); requests that still fail while
 * the flag is true surface as `ApiError.kind === 'offline'` from the client.
 */
export function useOnlineStatus(): boolean {
  const [online, setOnline] = useState<boolean>(() =>
    typeof navigator === 'undefined' ? true : navigator.onLine !== false,
  )

  useEffect(() => {
    const update = (): void => setOnline(navigator.onLine !== false)
    // Re-sync once: the signal may have changed between the lazy initializer
    // and the effect subscribing to the events.
    update()
    window.addEventListener('online', update)
    window.addEventListener('offline', update)
    return () => {
      window.removeEventListener('online', update)
      window.removeEventListener('offline', update)
    }
  }, [])

  return online
}
