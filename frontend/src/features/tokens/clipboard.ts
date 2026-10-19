/**
 * Clipboard write with a deterministic failure signal.
 *
 * `navigator.clipboard?.writeText` alone is not enough: when the API is absent
 * the optional chain resolves to `undefined` and callers would report success
 * without copying anything. This helper returns a boolean and falls back to the
 * legacy selection + `execCommand('copy')` path before giving up.
 */
export async function writeClipboard(text: string): Promise<boolean> {
  const clipboard =
    typeof navigator !== 'undefined' ? navigator.clipboard : undefined
  if (clipboard !== undefined && typeof clipboard.writeText === 'function') {
    try {
      await clipboard.writeText(text)
      return true
    } catch {
      // Permission denied / insecure context: try the legacy path.
    }
  }
  if (typeof document === 'undefined') return false
  try {
    const area = document.createElement('textarea')
    area.value = text
    area.setAttribute('readonly', '')
    area.style.position = 'fixed'
    area.style.top = '-1000px'
    area.style.opacity = '0'
    document.body.appendChild(area)
    area.select()
    const copied =
      typeof document.execCommand === 'function' && document.execCommand('copy')
    document.body.removeChild(area)
    return copied
  } catch {
    return false
  }
}
