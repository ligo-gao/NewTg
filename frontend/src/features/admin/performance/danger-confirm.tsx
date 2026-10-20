import { useEffect, useRef } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'

export interface DangerConfirmProps {
  open: boolean
  title: string
  description: string
  confirmLabel: string
  pending: boolean
  onCancel: () => void
  onConfirm: () => void
}

/**
 * 危险确认（UI §4 DangerConfirm）：`role=alertdialog`，初始焦点落在「取消」
 * 上，避免回车误触不可逆操作；请求进行中两个按钮都禁用，防止重复提交。
 *
 * Dialog 基元挂载时先把焦点放到面板，所以这里等一帧再显式聚焦取消按钮。
 * 焦点必须限定在本实例的 footer 内：页面上同时挂载三个 DangerConfirm，
 * 用 document 级选择器会抓到别家的按钮（甚至已关闭弹窗的残留节点）。
 */
export function DangerConfirm({
  open,
  title,
  description,
  confirmLabel,
  pending,
  onCancel,
  onConfirm,
}: DangerConfirmProps) {
  const { t } = useI18n()
  const footerRef = useRef<HTMLSpanElement>(null)

  useEffect(() => {
    if (!open) return
    const timer = globalThis.setTimeout(() => {
      footerRef.current
        ?.querySelector<HTMLElement>('[data-danger-cancel]')
        ?.focus()
    }, 0)
    return () => globalThis.clearTimeout(timer)
  }, [open])

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) onCancel()
      }}
      title={title}
      description={description}
      role="alertdialog"
      footer={
        <span ref={footerRef} className="contents">
          <Button
            variant="secondary"
            data-danger-cancel
            disabled={pending}
            onClick={onCancel}
          >
            {t('admin.performance.common.cancel')}
          </Button>
          <Button variant="danger" disabled={pending} onClick={onConfirm}>
            {pending
              ? t('admin.performance.common.working')
              : confirmLabel}
          </Button>
        </span>
      }
    />
  )
}
