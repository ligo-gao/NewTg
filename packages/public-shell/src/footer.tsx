import type { CSSProperties } from 'react'

import { ShellBrandMark } from './brand'
import type { ShellCopy } from './copy'
import { formatShellCopy } from './copy'
import { ShellLanguageControl } from './language-control'
import type { ShellFooterColumn, ShellFooterLink, ShellFooterProps } from './types'

function FooterLinkList({ links, copy }: { links: readonly ShellFooterLink[]; copy: ShellCopy }) {
  if (links.length === 0) return null
  return (
    <ul>
      {links.map((link) => (
        <li key={`${link.label}-${link.href}`}>
          <a
            href={link.href}
            target={link.external ? '_blank' : undefined}
            rel={link.external ? 'noopener noreferrer' : undefined}
            aria-label={
              link.external ? `${link.label} (${copy['nav.external']})` : undefined
            }
            className={link.dot ? 'ps-footer-status' : undefined}
          >
            {link.dot ? <span aria-hidden="true" className="ps-footer-dot" /> : null}
            {link.label}
          </a>
        </li>
      ))}
    </ul>
  )
}

function FooterColumnBody({ column, copy }: { column: ShellFooterColumn; copy: ShellCopy }) {
  // Operator HTML is already sanitized by the host; it replaces the link list
  // rather than joining it, so a configured footer never renders twice.
  if (column.html) {
    return <div className="ps-footer-operator" dangerouslySetInnerHTML={{ __html: column.html }} />
  }
  return <FooterLinkList links={column.links ?? []} copy={copy} />
}

/**
 * The public footer used by every page of both apps. The link columns are data
 * (marketing copy in one app, operator switches in the other); the language
 * column, brand block and legal line are part of the shell.
 */
export function ShellFooter({
  copy,
  brand,
  tagline,
  columns,
  locale,
  copyright,
  filing,
}: ShellFooterProps) {
  const columnCount = columns.length + (locale ? 1 : 0)
  // `repeat()` needs a literal count, so the whole template travels as one
  // custom property instead of just the number.
  const gridStyle = {
    '--ps-footer-grid': `minmax(0, 1.6fr) repeat(${columnCount}, minmax(0, 1fr))`,
  } as CSSProperties
  return (
    <footer role="contentinfo" className="ps-shell ps-footer">
      <div className="ps-wrap">
        <div className="ps-footer-grid" style={gridStyle}>
          <div>
            <ShellBrandMark brand={brand} ariaLabel={formatShellCopy(copy['footer.home_aria'], { site: brand.title })} />
            {tagline ? <p className="ps-footer-tag">{tagline}</p> : null}
          </div>

          {columns.map((column) => (
            <div key={column.key} className="ps-footer-col">
              <h2>{column.title}</h2>
              <FooterColumnBody column={column} copy={copy} />
            </div>
          ))}

          {locale ? (
            <div className="ps-footer-col">
              <h2>{copy['footer.language']}</h2>
              <ShellLanguageControl control={locale} copy={copy} variant="footer" />
            </div>
          ) : null}
        </div>

        <div className="ps-footer-bottom">
          <p>{copyright}</p>
          {filing ? (
            <a href={filing.href} target="_blank" rel="noopener noreferrer">
              {filing.label}
            </a>
          ) : null}
        </div>
      </div>
    </footer>
  )
}
