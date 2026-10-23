import type { ReactElement, ReactNode } from 'react'

export type NavIconKey =
  | 'grid'
  | 'key'
  | 'wallet'
  | 'receipt'
  | 'ticket'
  | 'list'
  | 'clipboard'
  | 'shield'
  | 'radio'
  | 'box'
  | 'users'
  | 'group'
  | 'card'
  | 'tag'
  | 'settings'
  | 'activity'
  | 'server'
  | 'puzzle'
  | 'gauge'
  | 'user'
  | 'help'
  | 'book'
  | 'sliders'
  | 'flask'
  | 'panel'
  | 'search'

const ICONS: Record<NavIconKey, ReactNode> = {
  grid: (
    <>
      <rect x="3" y="3" width="5" height="5" rx="1" />
      <rect x="12" y="3" width="5" height="5" rx="1" />
      <rect x="3" y="12" width="5" height="5" rx="1" />
      <rect x="12" y="12" width="5" height="5" rx="1" />
    </>
  ),
  key: (
    <>
      <circle cx="7" cy="13" r="3" />
      <path d="M9.5 10.5 16 4m-2 2 2 2m-4 0 2 2" />
    </>
  ),
  wallet: (
    <>
      <rect x="3" y="5" width="14" height="11" rx="2" />
      <path d="M3 8h14" />
      <circle cx="14" cy="12" r="1" />
    </>
  ),
  receipt: (
    <>
      <path d="M5 3h10v15l-2-1.5L11 18l-2-1.5L7 18l-2-1.5V3Z" />
      <path d="M8 7h4M8 10h4" />
    </>
  ),
  ticket: (
    <>
      <path d="M3 7a2 2 0 0 0 0 4v3h14v-3a2 2 0 0 1 0-4V4H3v3Z" />
      <path d="M8 6v8" />
    </>
  ),
  list: (
    <>
      <path d="M7 5h10M7 10h10M7 15h10" />
      <circle cx="4" cy="5" r=".75" fill="currentColor" />
      <circle cx="4" cy="10" r=".75" fill="currentColor" />
      <circle cx="4" cy="15" r=".75" fill="currentColor" />
    </>
  ),
  clipboard: (
    <>
      <rect x="5" y="4" width="10" height="14" rx="1.5" />
      <path d="M8 4V3h4v1M8 9h4M8 12h4" />
    </>
  ),
  shield: (
    <>
      <path d="M10 3 16 5v5c0 4-2.5 6-6 7-3.5-1-6-3-6-7V5l6-2Z" />
      <path d="m7.5 10 1.5 1.5 3.5-3.5" />
    </>
  ),
  radio: (
    <>
      <circle cx="10" cy="10" r="2" />
      <path d="M6.5 6.5a5 5 0 0 0 0 7M13.5 6.5a5 5 0 0 1 0 7" />
    </>
  ),
  box: (
    <>
      <path d="m10 3 7 3.5v7L10 17l-7-3.5v-7L10 3Z" />
      <path d="M3 6.5 10 10l7-3.5M10 10v7" />
    </>
  ),
  users: (
    <>
      <circle cx="7" cy="7" r="2.5" />
      <circle cx="14" cy="8" r="2" />
      <path d="M2.5 16c.5-2.5 2.2-4 4.5-4s4 1.5 4.5 4M12 12.5c2 .2 3.2 1.4 3.5 3.5" />
    </>
  ),
  group: (
    <>
      <circle cx="10" cy="6" r="2" />
      <circle cx="5" cy="13" r="2" />
      <circle cx="15" cy="13" r="2" />
      <path d="M10 8v3M6.5 12l2-1M13.5 12l-2-1M5 15v1M15 15v1" />
    </>
  ),
  card: (
    <>
      <rect x="3" y="5" width="14" height="10" rx="2" />
      <path d="M3 8h14M6 12h3" />
    </>
  ),
  tag: (
    <>
      <path d="M3 10V4h6l8 8-6 6-8-8Z" />
      <circle cx="6.5" cy="6.5" r="1" />
    </>
  ),
  settings: (
    <>
      <circle cx="10" cy="10" r="2.5" />
      <path d="M10 2v2M10 16v2M2 10h2M16 10h2M4.5 4.5l1.5 1.5M14 14l1.5 1.5M15.5 4.5 14 6M6 14l-1.5 1.5" />
    </>
  ),
  activity: <path d="M3 12h3l2-6 3 9 2-5h4" />,
  server: (
    <>
      <rect x="4" y="3" width="12" height="6" rx="1.5" />
      <rect x="4" y="11" width="12" height="6" rx="1.5" />
      <path d="M7 6h.01M7 14h.01" />
    </>
  ),
  puzzle: (
    <path d="M4 4h5v3a2 2 0 1 0 2 0V4h5v5h-3a2 2 0 1 0 0 2h3v5h-5v-3a2 2 0 1 0-2 0v3H4v-5h3a2 2 0 1 0 0-2H4V4Z" />
  ),
  gauge: (
    <>
      <path d="M4 14a6 6 0 1 1 12 0" />
      <path d="m10 11 3-3" />
      <circle cx="10" cy="11" r="1" />
    </>
  ),
  user: (
    <>
      <circle cx="10" cy="6.5" r="2.5" />
      <path d="M5 16c.5-3 2.5-4.5 5-4.5s4.5 1.5 5 4.5" />
    </>
  ),
  help: (
    <>
      <circle cx="10" cy="10" r="7" />
      <path d="M8 7.5a2 2 0 1 1 2.5 2c-.5.2-.5.6-.5 1M10 14h.01" />
    </>
  ),
  book: (
    <path d="M4 4h5a2 2 0 0 1 2 2v10a2 2 0 0 0-2-2H4V4ZM16 4h-3a2 2 0 0 0-2 2v10a2 2 0 0 1 2-2h3V4Z" />
  ),
  sliders: (
    <>
      <path d="M4 6h12M4 14h12" />
      <circle cx="8" cy="6" r="2" />
      <circle cx="13" cy="14" r="2" />
    </>
  ),
  flask: (
    <>
      <path d="M8 3h4M9 3v5l-4 7a1.5 1.5 0 0 0 1.3 2.2h7.4A1.5 1.5 0 0 0 15 15l-4-7V3" />
      <path d="M7 13h6" />
    </>
  ),
  panel: (
    <>
      <rect x="3" y="4" width="14" height="12" rx="2" />
      <path d="M8 4v12" />
    </>
  ),
  search: (
    <>
      <circle cx="9" cy="9" r="5" />
      <path d="m13 13 4 4" />
    </>
  ),
}

export function NavIcon({
  name,
  className = 'size-4',
}: {
  name: NavIconKey
  className?: string
}): ReactElement {
  return (
    <svg
      className={className}
      viewBox="0 0 20 20"
      aria-hidden="true"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.5}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      {ICONS[name]}
    </svg>
  )
}
