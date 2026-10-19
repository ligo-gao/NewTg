import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { useToastStore } from '@/shared/ui/toast'

export interface ApiAddressesDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
}

/**
 * API address reference (modules-02 §2.9).
 *
 * The public status contract does not carry `server_address`, so the endpoints
 * are derived from the same-origin gateway the console itself is served from
 * (no hardcoded host). Copying never injects a key.
 */
export function ApiAddressesDialog({ open, onOpenChange }: ApiAddressesDialogProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)
  const origin =
    typeof location !== 'undefined' ? location.origin : 'http://localhost'

  const rows = [
    { protocol: t('common.tokens.apiAddresses.openai'), url: `${origin}/v1/chat/completions` },
    { protocol: t('common.tokens.apiAddresses.anthropic'), url: `${origin}/v1/messages` },
    { protocol: t('common.tokens.apiAddresses.gemini'), url: `${origin}/v1beta/models` },
  ]

  async function copy(text: string): Promise<void> {
    try {
      await navigator.clipboard?.writeText(text)
      push({ tone: 'success', message: t('common.tokens.apiAddresses.copied') })
    } catch {
      push({ tone: 'error', message: t('common.tokens.toast.copyFailed') })
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={t('common.tokens.apiAddresses.title')}
      panelClassName="max-w-[440px]"
      footer={
        <Button
          variant="secondary"
          onClick={() => {
            onOpenChange(false)
          }}
        >
          {t('common.action.close')}
        </Button>
      }
    >
      <div className="flex flex-col gap-3">
        {rows.map((row) => (
          <div key={row.protocol} className="flex flex-col gap-1">
            <span className="text-xs font-medium text-ink-2">{row.protocol}</span>
            <div className="flex items-center gap-2">
              <code
                title={row.url}
                className="min-w-0 flex-1 truncate rounded-md bg-surface-subtle px-2 py-1.5 font-mono text-[13px] text-ink-1"
              >
                {row.url}
              </code>
              <Button
                variant="secondary"
                aria-label={t('common.tokens.apiAddresses.copyAddress', {
                  protocol: row.protocol,
                })}
                onClick={() => {
                  void copy(row.url)
                }}
              >
                {t('common.action.copy')}
              </Button>
            </div>
          </div>
        ))}
        <p className="text-xs text-ink-3">{t('common.tokens.apiAddresses.hint')}</p>
      </div>
    </Dialog>
  )
}
