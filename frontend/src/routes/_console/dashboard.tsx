import { createFileRoute } from '@tanstack/react-router'

function DashboardPage() {
  return (
    <section className="mt-6 rounded-xl border border-border-hairline bg-surface-card p-5">
      <h2 className="text-base font-semibold">Dashboard</h2>
      <p className="mt-1 text-sm text-ink-2">M0 scaffold</p>
    </section>
  )
}

export const Route = createFileRoute('/_console/dashboard')({
  component: DashboardPage,
})
