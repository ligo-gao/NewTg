import { createFileRoute, Outlet } from '@tanstack/react-router'

function ConsoleLayout() {
  return (
    <div className="min-h-screen bg-surface-page text-ink-1">
      <header className="border-b border-border-hairline bg-surface-card px-6 py-3">
        <p className="text-sm font-semibold">NewTG console</p>
      </header>
      <main id="main" className="p-6">
        <h1 className="text-xl font-semibold">M0 scaffold</h1>
        <p className="mt-1 text-sm text-ink-2">
          Session guards and the console shell arrive in M1.
        </p>
        <Outlet />
      </main>
    </div>
  )
}

export const Route = createFileRoute('/_console')({
  component: ConsoleLayout,
})
