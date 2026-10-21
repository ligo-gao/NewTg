/**
 * Auto-disable reason code → locale key (R-M4-05).
 *
 * The backend stores a stable machine code, never the upstream error text:
 * upstream bodies may contain credentials or URLs and must not be echoed
 * (GC-6). The UI therefore localises the code here instead of rendering it.
 *
 * Unknown codes fall back to a generic "automatically disabled" label. This is
 * deliberate: a future backend reason must still be readable to the operator,
 * and never leak an untranslated code into the UI.
 */
const REASON_KEYS: Record<string, string> = {
  channel_upstream_error: 'common.channels.reason.channel_upstream_error',
  channel_auth_error: 'common.channels.reason.channel_auth_error',
  channel_keyword: 'common.channels.reason.channel_keyword',
  channel_timeout: 'common.channels.reason.channel_timeout',
}

export const UNKNOWN_DISABLED_REASON_KEY = 'common.channels.reason.unknown'

export function channelDisabledReasonKey(reason: string): string {
  return REASON_KEYS[reason] ?? UNKNOWN_DISABLED_REASON_KEY
}
