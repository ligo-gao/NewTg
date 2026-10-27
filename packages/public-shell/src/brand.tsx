import { RelayMark } from './icons'
import type { ShellBrand } from './types'

/** Only site-relative paths or absolute http(s) URLs may become an `<img src>`. */
function safeLogoUrl(raw: string | null | undefined): string | null {
  const value = (raw ?? '').trim()
  if (value === '') return null
  if (value.startsWith('/') && !value.startsWith('//')) return value
  if (/^https?:\/\//i.test(value)) return value
  return null
}

export interface ShellBrandProps {
  brand: ShellBrand
  /** Accessible name; the visible wordmark already reads the site title. */
  ariaLabel?: string
  className?: string
}

/**
 * Brand lockup shared by the header and the footer: the configured logo when
 * there is one, otherwise the wave mark, always followed by the site title.
 */
export function ShellBrandMark({ brand, ariaLabel, className }: ShellBrandProps) {
  const logo = safeLogoUrl(brand.logoUrl)
  return (
    <a
      href={brand.href}
      aria-label={ariaLabel}
      className={['ps-brand', className].filter(Boolean).join(' ')}
    >
      {logo !== null ? (
        // Intrinsic size is set so the header does not reflow when the image
        // arrives (CLS budget).
        <img src={logo} alt="" width={22} height={22} className="ps-brand-logo" />
      ) : (
        <RelayMark />
      )}
      <span className="ps-brand-name">{brand.title}</span>
    </a>
  )
}
