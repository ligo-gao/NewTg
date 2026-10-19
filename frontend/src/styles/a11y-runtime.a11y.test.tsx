import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

/**
 * UI-5 runtime checks that a normal component test cannot express:
 *  - the token sheet keeps every text-bearing pair at WCAG AA;
 *  - a global `prefers-reduced-motion` rule disables non-essential motion
 *    (there is no JS-driven animation in the app, so the media rule is the
 *    single switch).
 */

const stylesDir = fileURLToPath(new URL('.', import.meta.url))
const tokensCss = readFileSync(`${stylesDir}tokens.css`, 'utf8')
const tailwindCss = readFileSync(`${stylesDir}tailwind.css`, 'utf8')

function tokenValue(name: string): string {
  const match = new RegExp(`--${name}:\\s*(#[0-9a-fA-F]{6})`).exec(tokensCss)
  if (match?.[1] === undefined) throw new Error(`missing token --${name}`)
  return match[1]
}

function luminance(hex: string): number {
  const value = hex.replace('#', '')
  const channels = [0, 2, 4].map((offset) =>
    Number.parseInt(value.slice(offset, offset + 2), 16),
  )
  const linear = channels.map((channel) => {
    const normalized = channel / 255
    return normalized <= 0.03928
      ? normalized / 12.92
      : ((normalized + 0.055) / 1.055) ** 2.4
  })
  return 0.2126 * linear[0]! + 0.7152 * linear[1]! + 0.0722 * linear[2]!
}

function contrast(foreground: string, background: string): number {
  const [lighter, darker] = [luminance(foreground), luminance(background)].sort(
    (a, b) => b - a,
  )
  return (lighter! + 0.05) / (darker! + 0.05)
}

describe('design token contrast (UI-5)', () => {
  it.each([
    ['ink-1 on card', 'ink-1', 'surface-card'],
    ['ink-2 on card', 'ink-2', 'surface-card'],
    ['ink-3 on card', 'ink-3', 'surface-card'],
    ['ink-3 on page', 'ink-3', 'surface-page'],
    // Regression: ink-3 was #6b7280, which measured only 4.31:1 here.
    ['ink-3 on subtle', 'ink-3', 'surface-subtle'],
    ['ink-3 on brand-subtle', 'ink-3', 'brand-subtle'],
    ['success on success-bg', 'success-fg', 'success-bg'],
    ['warning on warning-bg', 'warning-fg', 'warning-bg'],
    ['danger on danger-bg', 'danger-fg', 'danger-bg'],
    ['info on info-bg', 'info-fg', 'info-bg'],
    ['neutral on neutral-bg', 'neutral-fg', 'neutral-bg'],
    ['amount-out on card', 'amount-out', 'surface-card'],
    ['amount-in on card', 'amount-in', 'surface-card'],
  ])('%s is at least 4.5:1', (_label, foreground, background) => {
    expect(
      contrast(tokenValue(foreground), tokenValue(background)),
    ).toBeGreaterThanOrEqual(4.5)
  })

  it('white-on-brand stays at AA', () => {
    expect(contrast(tokenValue('on-brand'), tokenValue('brand'))).toBeGreaterThanOrEqual(4.5)
  })
})

describe('prefers-reduced-motion (UI-5)', () => {
  it('ships a global motion-reset media rule', () => {
    expect(tailwindCss).toMatch(/@media\s*\(prefers-reduced-motion:\s*reduce\)/)
    expect(tailwindCss).toMatch(/animation-duration:\s*0(?:\.01ms|ms)/)
    expect(tailwindCss).toMatch(/transition-duration:\s*0(?:\.01ms|ms)/)
    expect(tailwindCss).toMatch(/scroll-behavior:\s*auto\s*!important/)
  })
})
