import { createFileRoute } from '@tanstack/react-router'

import { ErrorStatusPage } from '@/app/error-status-page'

/** Signed-in but unauthorized landing (web-00 §8.3). */
export const Route = createFileRoute('/_errors/403')({
  component: () => <ErrorStatusPage status={403} />,
})
