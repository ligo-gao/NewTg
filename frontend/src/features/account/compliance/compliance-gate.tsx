import { useEffect, useId, useState } from 'react'

import { LEGAL_DOC_LINKS } from '@/features/auth/legal-meta'
import {
  isExternalHref,
  isSafePublicHref,
} from '@/features/public/nav-visibility'
import { useI18n } from '@/shared/lib/i18n-context'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'

export interface ComplianceGateProps {
  open: boolean
  /** Escape, backdrop or the Cancel button. The caller keeps its own dialog open. */
  onOpenChange: (open: boolean) => void
  /** The viewer actively ticked the box and chose to continue. */
  onConfirm: () => void
  /**
   * Legal link targets. When a runtime `site.legal.*` setting lands, pass it
   * here; until then the bundled legal routes are used. Unsafe values (for
   * example `javascript:` or `//host`) are rejected and the label is rendered
   * as plain text instead of an anchor.
   */
  termsHref?: string
  privacyHref?: string
}

interface LegalLinkProps {
  href: string | undefined
  children: string
}

/**
 * One legal link, validated by the same rule used by the public navigation
 * (rejects `javascript:`/`data:`/protocol-relative targets). Content is a
 * React text node — the gate never uses `dangerouslySetInnerHTML`, so legal
 * copy cannot inject markup.
 */
function LegalLink({ href, children }: LegalLinkProps) {
  const value = href?.trim() ?? ''
  if (!isSafePublicHref(value)) {
    return <span>{children}</span>
  }
  const external = isExternalHref(value)
  return (
    <a
      href={value}
      {...(external ? { target: '_blank', rel: 'noopener noreferrer' } : {})}
      className="font-medium text-brand underline-offset-2 hover:underline"
    >
      {children}
    </a>
  )
}

/**
 * Payment compliance gate (G-M6C-19).
 *
 * Every paid action renders this modal immediately before the request that
 * creates an order. The viewer must actively tick the acknowledgement checkbox
 * before the primary button becomes enabled; closing/cancelling discards the
 * pending action and never submits anything.
 *
 * The checkbox resets on every open so a previously held action cannot be
 * confirmed by a stale checked state. Acknowledgement persistence is handled
 * by `useComplianceGate` (tab session only; see `compliance-session.ts`).
 */
export function ComplianceGate({
  open,
  onOpenChange,
  onConfirm,
  termsHref = LEGAL_DOC_LINKS.terms,
  privacyHref = LEGAL_DOC_LINKS.privacy,
}: ComplianceGateProps) {
  const { t } = useI18n()
  const checkboxId = useId()
  const [acknowledged, setAcknowledged] = useState(false)

  useEffect(() => {
    if (open) setAcknowledged(false)
  }, [open])

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={t('account.compliance.title')}
      description={t('account.compliance.description')}
      role="alertdialog"
      footer={
        <>
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            {t('account.compliance.cancel')}
          </Button>
          <Button disabled={!acknowledged} onClick={onConfirm}>
            {t('account.compliance.continue')}
          </Button>
        </>
      }
    >
      <ul className="flex list-disc flex-col gap-1.5 pl-5">
        <li>{t('account.compliance.payment_point')}</li>
        <li>{t('account.compliance.refund_point')}</li>
        <li>
          {t('account.compliance.terms_point')}{' '}
          <LegalLink href={termsHref}>
            {t('account.compliance.terms_link')}
          </LegalLink>
        </li>
        <li>
          {t('account.compliance.privacy_point')}{' '}
          <LegalLink href={privacyHref}>
            {t('account.compliance.privacy_link')}
          </LegalLink>
        </li>
      </ul>

      <label
        htmlFor={checkboxId}
        className="mt-4 flex cursor-pointer items-start gap-2 rounded-lg border border-border-default bg-surface-page p-3 text-sm text-ink-1"
      >
        <input
          id={checkboxId}
          type="checkbox"
          checked={acknowledged}
          onChange={(event) => {
            setAcknowledged(event.target.checked)
          }}
          className="mt-0.5 accent-brand"
        />
        <span>{t('account.compliance.acknowledge')}</span>
      </label>
    </Dialog>
  )
}
