import { useEffect, useRef, useState } from 'react'

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

/** Old-browser path: a hidden, non-focusable textarea selected and copied. */
function legacyCopy(text: string): boolean {
  const area = document.createElement('textarea')
  area.value = text
  area.setAttribute('readonly', '')
  area.style.position = 'fixed'
  area.style.top = '0'
  area.style.opacity = '0'
  document.body.appendChild(area)
  area.select()
  try {
    return document.execCommand('copy')
  } catch {
    return false
  } finally {
    area.remove()
  }
}

/**
 * Copies `text`, resolving false when no clipboard path is available. The
 * async clipboard API is missing on insecure origins and can reject on a
 * denied permission; neither may throw into the click handler.
 */
async function writeClipboard(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    // Fall through to the legacy path.
  }
  try {
    return legacyCopy(text)
  } catch {
    return false
  }
}

/**
 * The one distinctive element of the footer: the real OpenAI-compatible base
 * URL, with a copy affordance. The host decides whether to pass it (it hides
 * it when no public address is configured).
 */
function FooterEndpoint({ endpoint, copy }: { endpoint: string; copy: ShellCopy }) {
  const [copied, setCopied] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const live = useRef(true)

  useEffect(() => {
    live.current = true
    return () => {
      live.current = false
      if (timer.current !== null) clearTimeout(timer.current)
    }
  }, [])

  async function copyEndpoint() {
    const ok = await writeClipboard(endpoint)
    if (!ok || !live.current) return
    setCopied(true)
    if (timer.current !== null) clearTimeout(timer.current)
    timer.current = setTimeout(() => {
      timer.current = null
      if (live.current) setCopied(false)
    }, 1600)
  }

  return (
    <div className="ps-footer-endpoint" role="group" aria-label={copy['footer.endpoint']}>
      <span className="ps-footer-endpoint-label">{copy['footer.endpoint']}</span>
      <code title={endpoint}>{endpoint}</code>
      <button
        type="button"
        className="ps-footer-copy"
        aria-label={`${copy['footer.copy']} ${endpoint}`}
        onClick={() => {
          void copyEndpoint()
        }}
      >
        <span aria-live="polite">
          {copied ? copy['footer.copied'] : copy['footer.copy']}
        </span>
      </button>
    </div>
  )
}

/**
 * The public footer used by every page of both apps. The link columns are data
 * (marketing copy in one app, operator switches in the other); the language
 * column, brand block and legal line are part of the shell.
 */
export function ShellFooter({
  copy,
  brand,
  statement,
  endpoint,
  columns,
  locale,
  copyright,
  filing,
}: ShellFooterProps) {
  return (
    <footer role="contentinfo" className="ps-shell ps-footer">
      <div className="ps-wrap">
        <div className="ps-footer-top">
          <div className="ps-footer-id">
            <ShellBrandMark brand={brand} ariaLabel={formatShellCopy(copy['footer.home_aria'], { site: brand.title })} />
            {statement ? <p className="ps-footer-statement">{statement}</p> : null}
            {endpoint ? <FooterEndpoint endpoint={endpoint} copy={copy} /> : null}
          </div>

          <nav className="ps-footer-nav" aria-label={copy['footer.nav_label']}>
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
          </nav>
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
