import { useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'

import { TwoFaSetupDialog } from '@/features/auth/TwoFaSetupDialog'

/**
 * F-01-10 entry card. `/api/me` does not expose `totp_enabled`, so the card
 * cannot render an authoritative on/off state; the setup endpoint's 409 is the
 * first reliable signal and the dialog surfaces it as "already enabled".
 */
export function TwoFaCard() {
  const { t } = useI18n()
  const [open, setOpen] = useState(false)

  return (
    <section className="rounded-xl border border-border-hairline bg-surface-card p-5">
      <h2 className="text-base font-semibold text-ink-1">
        {t('auth.two_fa_setup.title')}
      </h2>
      <p className="mt-1 text-[13px] text-ink-2">
        {/* Card copy, not the login-challenge instruction: the two contexts
            differ ("enable" vs "enter a code"). */}
        {t('auth.two_fa_setup.card_description')}
      </p>
      <Button variant="secondary" className="mt-4" onClick={() => setOpen(true)}>
        {t('auth.two_fa_setup.enable')}
      </Button>
      <TwoFaSetupDialog open={open} onOpenChange={setOpen} />
    </section>
  )
}
