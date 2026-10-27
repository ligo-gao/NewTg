/**
 * Browser download helpers shared by every one-shot file export.
 *
 * Feature-local copies existed in `admin/redemption-codes/redemption-export.ts`
 * and `logs/export-api.ts`; both now funnel through here so the timing rules
 * (object-URL lifetime) are defined once.
 */

/**
 * Triggers one download of `blob` under `filename`.
 *
 * The object URL is released on a `setTimeout(..., 0)` rather than
 * synchronously after `click()`: some browsers only read the Blob once the
 * click handler returns, and an immediate `revokeObjectURL` yields an empty
 * file. Existing export tests assert this deferred revoke, so it is
 * load-bearing, not stylistic.
 */
export function downloadBlob(blob: Blob, filename: string): void {
  const objectUrl = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = objectUrl
  anchor.download = filename
  anchor.rel = 'noopener'
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  setTimeout(() => {
    URL.revokeObjectURL(objectUrl)
  }, 0)
}
