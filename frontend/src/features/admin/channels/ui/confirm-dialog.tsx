import { useEffect, useState } from 'react'

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

/** modules-00 §4.8: the confirm key must not react during the first 300ms. */
const ARM_DELAY_MS = 300

/**
 * Minimal destructive-confirmation dialog. `Dialog` moves focus to the panel
 * rather than the confirm button, and the confirm action stays disabled for
 * one frame-time so an accidental Enter at open cannot destroy anything
 * (modules-00 §4.8).
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
  const [armed, setArmed] = useState(false)

  useEffect(() => {
    if (!open) {
      setArmed(false)
      return
    }
    const timer = setTimeout(() => {
      setArmed(true)
    }, ARM_DELAY_MS)
    return () => {
      clearTimeout(timer)
    }
  }, [open])

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
            disabled={busy || !armed}
            onClick={() => {
              if (!armed) return
              onConfirm()
            }}
          >
            {confirmLabel}
          </Button>
        </>
      }
    />
  )
}
