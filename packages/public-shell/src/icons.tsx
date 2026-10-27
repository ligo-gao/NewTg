/** Shared glyphs for the public shell. All are decorative (`aria-hidden`). */

export function RelayMark({ className = 'ps-brand-mark' }: { className?: string }) {
  return (
    <svg
      className={className}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      aria-hidden="true"
    >
      <path d="M3 6h3.5c2 0 3.5 1.2 4.5 3l2 6c1 1.8 2.5 3 4.5 3H21" />
      <path d="M3 18h3.5c2 0 3.5-1.2 4.5-3l2-6c1-1.8 2.5-3 4.5-3H21" />
    </svg>
  )
}

export function MenuGlyph({ size = 20 }: { size?: number }) {
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 16 16"
      width={size}
      height={size}
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
    >
      <path d="M2 4h12M2 8h12M2 12h12" />
    </svg>
  )
}

export function CloseGlyph({ size = 16 }: { size?: number }) {
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 16 16"
      width={size}
      height={size}
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
    >
      <path d="M3 3l10 10M13 3L3 13" />
    </svg>
  )
}

export function GlobeGlyph() {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
      aria-hidden="true"
      className="ps-lang-globe"
    >
      <circle cx="12" cy="12" r="9" />
      <path d="M3 12h18M12 3c2.5 2.4 4 5.6 4 9s-1.5 6.6-4 9c-2.5-2.4-4-5.6-4-9s1.5-6.6 4-9Z" />
    </svg>
  )
}

export function ChevronGlyph() {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      className="ps-account-chevron"
    >
      <path d="m6 9 6 6 6-6" />
    </svg>
  )
}

/** Marks the chosen language inside the globe menu. */
export function CheckGlyph() {
  return (
    <svg
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      className="ps-lang-check"
    >
      <path d="m3.5 8.5 3 3 6-7" />
    </svg>
  )
}

/** Moon in light mode, sun in dark mode: the icon shows the target theme. */
export function ThemeGlyph({ target }: { target: 'dark' | 'light' }) {
  if (target === 'dark') {
    return (
      <svg
        aria-hidden="true"
        viewBox="0 0 24 24"
        width={16}
        height={16}
        fill="none"
        stroke="currentColor"
        strokeWidth={1.7}
        strokeLinecap="round"
        strokeLinejoin="round"
      >
        <path d="M20.5 14.6A8.5 8.5 0 0 1 9.4 3.5a8.5 8.5 0 1 0 11.1 11.1Z" />
      </svg>
    )
  }
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 24 24"
      width={16}
      height={16}
      fill="none"
      stroke="currentColor"
      strokeWidth={1.7}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
    </svg>
  )
}

export function AvatarGlyph() {
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 16 16"
      width={16}
      height={16}
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
    >
      <circle cx="8" cy="5.5" r="2.5" />
      <path d="M3.5 13c.8-2 2.5-3 4.5-3s3.7 1 4.5 3" strokeLinecap="round" />
    </svg>
  )
}

export function ExternalGlyph() {
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 16 16"
      className="ps-external-glyph"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
    >
      <path d="M5 11 11 5M6 5h5v5" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}
