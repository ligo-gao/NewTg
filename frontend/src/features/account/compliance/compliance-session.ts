/**
 * Payment compliance acknowledgement kept for the browser tab session.
 *
 * The product design has no account-level consent API and no backend record
 * for this user-side prompt (the admin Payment tab has its own audited
 * confirmation, which is a different surface). We therefore never fabricate a
 * consent record: this module only remembers, for as long as the tab lives,
 * that the viewer already read the gate, so a second payment in the same
 * session does not repeat it. `sessionStorage` may be unavailable (private
 * mode, blocked storage); every read/write fails closed and the gate simply
 * appears again.
 *
 * The key is versioned because the gate copy and the linked legal documents
 * can change; bumping the version intentionally asks every viewer to read the
 * new copy once.
 */
const COMPLIANCE_ACK_KEY = 'newtg.payment.compliance.v1'

/** True when the viewer already completed the gate in this tab session. */
export function isComplianceAcknowledged(): boolean {
  try {
    return globalThis.sessionStorage?.getItem(COMPLIANCE_ACK_KEY) === '1'
  } catch {
    return false
  }
}

/**
 * Marks the gate as read for this tab session.
 *
 * This is a UI-gating flag, not a consent record: it stores no timestamp,
 * version, user id or legal text. A missing/failing storage backend is a
 * no-op, so the current payment still proceeds and the next one asks again.
 */
export function acknowledgeCompliance(): void {
  try {
    globalThis.sessionStorage?.setItem(COMPLIANCE_ACK_KEY, '1')
  } catch {
    // Local memory unavailable; the payment action itself is not blocked.
  }
}

/** Clears the acknowledgement (used by tests and by session teardown). */
export function clearComplianceAcknowledgement(): void {
  try {
    globalThis.sessionStorage?.removeItem(COMPLIANCE_ACK_KEY)
  } catch {
    // Nothing to clear when storage is unavailable.
  }
}

/**
 * Read-only test seam: the storage key is otherwise private so callers cannot
 * write an arbitrary value that would bypass the gate contract.
 */
export function complianceStorageKey(): string {
  return COMPLIANCE_ACK_KEY
}
