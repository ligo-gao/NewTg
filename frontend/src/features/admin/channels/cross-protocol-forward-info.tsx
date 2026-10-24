import { useId } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'

import {
  CROSS_PROTOCOL_HEADER,
  CROSS_PROTOCOL_SWITCH_KEY,
  CROSS_PROTOCOL_VALUES,
} from './types'

export interface CrossProtocolForwardInfoProps {
  /**
   * Live value of `relay.cross_protocol_forward_enabled`.
   *
   * M4 deliberately has no settings API: the key is maintained by module 08
   * (site settings). Production therefore passes nothing and the panel reports
   * the status as unavailable; module 08 can later supply the real value and
   * tests pin both copy variants. Passing a value here is display-only — this
   * component never renders an editable control, because a switch wired to a
   * non-existent endpoint would be "editable but ineffective" (Task 6 brief).
   */
  enabled?: boolean
}

/**
 * Read-only explanation of cross-protocol forwarding (M4 Task 6).
 *
 * The behaviour documented here is frozen by M4 Task 5 (`relay/crossproto`):
 * `x-newtg-protocol` selects the *upstream* protocol on chat completions, the
 * switch defaults to false, and a header-bearing request is rejected with 403
 * while the switch is off. There is no admin API in M4, so this panel only
 * explains the contract and points at module 08 for the switch itself.
 *
 * Deferred (registered): an editable switch / protocol mapping UI depends on
 * module 08's settings API. It is intentionally absent rather than mocked.
 */
export function CrossProtocolForwardInfo({
  enabled,
}: CrossProtocolForwardInfoProps) {
  const { t } = useI18n()
  const headingId = useId()
  const headerValues = CROSS_PROTOCOL_VALUES.join(' | ')
  // `undefined` means "not readable from this console", which is the only
  // honest production state in M4 — never collapse it into `false`, or the
  // panel would claim a live status it never fetched.
  const status =
    enabled === undefined ? 'unknown' : enabled ? 'on' : 'off'

  return (
    <section
      role="note"
      aria-labelledby={headingId}
      data-testid="cross-protocol-info"
      className="flex flex-col gap-3 rounded-xl border border-border-hairline bg-surface-subtle px-4 py-3"
    >
      <div className="flex flex-col gap-1">
        <h2 id={headingId} className="text-body font-semibold text-ink-1">
          {t('common.channels.crossProtocol.title')}
        </h2>
        <p className="text-meta text-ink-2">
          {t('common.channels.crossProtocol.intro', {
            header: CROSS_PROTOCOL_HEADER,
            values: headerValues,
          })}
        </p>
        <p className="text-meta text-ink-2">
          {t('common.channels.crossProtocol.selectionRules')}
        </p>
      </div>

      <dl className="grid grid-cols-1 gap-x-6 gap-y-2 text-meta sm:grid-cols-2">
        <div>
          <dt className="text-ink-3">
            {t('common.channels.crossProtocol.headerLabel')}
          </dt>
          <dd className="mt-0.5 font-mono text-ink-1">
            {CROSS_PROTOCOL_HEADER}: {headerValues}
          </dd>
        </div>
        <div>
          <dt className="text-ink-3">
            {t('common.channels.crossProtocol.switchLabel')}
          </dt>
          <dd className="mt-0.5 font-mono text-ink-1">
            {CROSS_PROTOCOL_SWITCH_KEY}
          </dd>
        </div>
        <div className="sm:col-span-2">
          <dt className="text-ink-3">
            {t('common.channels.crossProtocol.statusLabel')}
          </dt>
          <dd className="mt-0.5 text-ink-1" data-testid="cross-protocol-status">
            {t(`common.channels.crossProtocol.status.${status}`)}
          </dd>
        </div>
      </dl>

      <p className="text-meta text-ink-2">
        {t('common.channels.crossProtocol.defaultOff')}
      </p>
      <p className="text-meta text-ink-2">
        {t('common.channels.crossProtocol.managedBy')}
      </p>
      <p className="text-meta text-ink-3">
        {t('common.channels.crossProtocol.editableDeferred')}
      </p>
    </section>
  )
}
