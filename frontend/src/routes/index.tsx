import { createFileRoute } from '@tanstack/react-router'

function HomePage() {
  return (
    <main
      id="main"
      className="flex min-h-screen flex-col justify-center gap-2 bg-surface-page p-8 text-ink-1"
    >
      <p className="text-xs font-medium uppercase tracking-wide text-ink-3">
        NewTG
      </p>
      <h1 className="text-xl font-semibold">M0 scaffold</h1>
      <p className="max-w-prose text-sm text-ink-2">
        The public home page is a placeholder until the public pages land.
      </p>
    </main>
  )
}

export const Route = createFileRoute('/')({
  component: HomePage,
})
