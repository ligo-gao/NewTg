import { useCallback, useRef, useState } from 'react'

import {
  acknowledgeCompliance,
  isComplianceAcknowledged,
} from './compliance-session'

export interface ComplianceGateController {
  /** Render the gate when true. */
  gateOpen: boolean
  /**
   * Run `action` immediately when this tab already acknowledged the gate;
   * otherwise hold it and open the gate. The action only runs after the viewer
   * actively ticks the checkbox and continues.
   */
  requireCompliance: (action: () => void) => void
  /** Gate confirmed: remember the acknowledgement and run the held action. */
  confirmCompliance: () => void
  /** Gate cancelled/Esc/backdrop: discard the held action, keep the caller open. */
  cancelCompliance: () => void
}

/**
 * Controller shared by every payment surface (top-up, subscription purchase,
 * subscription renewal). Keeping the pending action here means a cancelled
 * gate can never leak a payment request: the callback is only invoked from
 * `confirmCompliance`.
 */
export function useComplianceGate(): ComplianceGateController {
  const pendingRef = useRef<(() => void) | null>(null)
  const [gateOpen, setGateOpen] = useState(false)

  const requireCompliance = useCallback((action: () => void) => {
    if (isComplianceAcknowledged()) {
      action()
      return
    }
    pendingRef.current = action
    setGateOpen(true)
  }, [])

  const confirmCompliance = useCallback(() => {
    acknowledgeCompliance()
    const action = pendingRef.current
    pendingRef.current = null
    setGateOpen(false)
    action?.()
  }, [])

  const cancelCompliance = useCallback(() => {
    pendingRef.current = null
    setGateOpen(false)
  }, [])

  return { gateOpen, requireCompliance, confirmCompliance, cancelCompliance }
}
