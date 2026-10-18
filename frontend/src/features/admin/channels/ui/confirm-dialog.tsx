import { Button } from '@/shared/ui/base/button'
import { Dialog } from '@/shared/ui/base/dialog'

export interface ConfirmDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description: string
  confirmLabel: string
  cancelLabel: string
  /** Destructive confirmations use the danger button (modules-00 §4.8). */
  destructive?: boolean
  busy?: boolean
  onConfirm: () => void
}

/**
 * Minimal destructive-confirmation dialog. `Dialog` moves focus to the panel
 * rather than the confirm button, so the first frame cannot be dismissed or
 * confirmed by an accidental Enter (modules-00 §4.8).
 */
export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel,
  cancelLabel,
  destructive = false,
  busy = false,
  onConfirm,
}: ConfirmDialogProps) {
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!busy) onOpenChange(next)
      }}
      title={title}
      description={description}
      footer={
        <>
          <Button
            variant="secondary"
            disabled={busy}
            onClick={() => {
              onOpenChange(false)
            }}
          >
            {cancelLabel}
          </Button>
          <Button
            variant={destructive ? 'danger' : 'primary'}
            disabled={busy}
            onClick={onConfirm}
          >
            {confirmLabel}
          </Button>
        </>
      }
    />
  )
}
