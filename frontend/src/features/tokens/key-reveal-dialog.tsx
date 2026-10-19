import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { useToastStore } from '@/shared/ui/toast'

export interface RevealedKey {
  name: string
  key: string
}

export interface KeyRevealDialogProps {
  entries: RevealedKey[]
  onClose: () => void
  /** Secondary entry: hand the key to the CC Switch importer. */
  onImport?: (entry: RevealedKey) => void
}

/**
 * Blocking one-time key result (modules-02 §2.7, F-02-08).
 *
 * The plaintext exists only in this component's props and DOM; closing clears
 * the caller's state. Escape is deliberately disabled (the `dismissible`
 * exception in modules-00 §4.8) so a stray key press cannot lose the only copy.
 */
export function KeyRevealDialog({ entries, onClose, onImport }: KeyRevealDialogProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)

  async function copy(text: string): Promise<void> {
    try {
      await navigator.clipboard?.writeText(text)
      push({ tone: 'success', message: t('common.tokens.toast.copied') })
    } catch {
      push({ tone: 'error', message: t('common.tokens.toast.copyFailed') })
    }
  }

  const copyAll = entries.map((entry) => `${entry.name},${entry.key}`).join('\n')

  return (
    <Dialog
      open={entries.length > 0}
      onOpenChange={(open) => {
        // Only the explicit Done button may close this dialog.
        if (!open) onClose()
      }}
      title={t('common.tokens.keyReveal.title')}
      dismissible={false}
      panelClassName="max-w-[560px]"
      footer={
        <>
          <Button
            variant="secondary"
            onClick={() => {
              void copy(copyAll)
            }}
          >
            {t('common.tokens.keyReveal.copyAll')}
          </Button>
          <Button
            onClick={() => {
              onClose()
            }}
          >
            {t('common.tokens.keyReveal.done')}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <div
          role="note"
          className="rounded-md border-l-4 border-warning bg-warning-bg px-3 py-2 text-xs text-warning"
        >
          {t('common.tokens.keyReveal.warning')}
        </div>
        {entries.length > 1 ? (
          <p className="text-xs text-ink-2">
            {t('common.tokens.keyReveal.total', { count: entries.length })}
          </p>
        ) : null}
        <ul className="flex max-h-60 flex-col gap-2 overflow-y-auto">
          {entries.map((entry) => (
            <li key={`${entry.name}-${entry.key}`} className="flex flex-col gap-1">
              <span className="text-xs text-ink-2">{entry.name}</span>
              <div className="flex items-center gap-2">
                <code
                  aria-readonly="true"
                  aria-label={t('common.tokens.keyReveal.plainAria')}
                  className="flex-1 overflow-x-auto rounded-md bg-surface-hover px-2 py-1.5 font-mono text-[13px] text-ink-1"
                >
                  {entry.key}
                </code>
                <Button
                  variant="secondary"
                  aria-label={t('common.tokens.keyReveal.copyAria', { name: entry.name })}
                  onClick={() => {
                    void copy(entry.key)
                  }}
                >
                  {t('common.tokens.keyReveal.copy')}
                </Button>
                {onImport !== undefined ? (
                  <Button
                    variant="ghost"
                    onClick={() => {
                      onImport(entry)
                    }}
                  >
                    {t('common.tokens.ccSwitch.title')}
                  </Button>
                ) : null}
              </div>
            </li>
          ))}
        </ul>
      </div>
    </Dialog>
  )
}
