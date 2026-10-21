import { describe, expect, it } from 'vitest'

import {
  UNKNOWN_DISABLED_REASON_KEY,
  channelDisabledReasonKey,
} from './channel-disabled-reason'

/**
 * R-M4-05: the backend stores stable reason codes; the UI maps every known
 * code to a locale key and falls back to a generic label for unknown ones so a
 * raw code is never rendered to the operator.
 */
describe('channelDisabledReasonKey', () => {
  it.each([
    ['channel_upstream_error', 'common.channels.reason.channel_upstream_error'],
    ['channel_auth_error', 'common.channels.reason.channel_auth_error'],
    ['channel_keyword', 'common.channels.reason.channel_keyword'],
    ['channel_timeout', 'common.channels.reason.channel_timeout'],
  ])('maps %s to its locale key', (code, key) => {
    expect(channelDisabledReasonKey(code)).toBe(key)
  })

  it('falls back to the generic label for an unknown code', () => {
    expect(channelDisabledReasonKey('channel_future_reason')).toBe(
      UNKNOWN_DISABLED_REASON_KEY,
    )
  })
})
