import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { Sparkline } from './sparkline'

function svgOf(container: HTMLElement): SVGSVGElement {
  const svg = container.querySelector('svg')
  if (svg === null) throw new Error('sparkline svg missing')
  return svg
}

describe('Sparkline', () => {
  it('renders a decorative svg without a path for an empty series', () => {
    const { container } = render(<Sparkline values={[]} />)
    const svg = svgOf(container)

    expect(svg.getAttribute('aria-hidden')).toBe('true')
    expect(svg.querySelector('path')).toBeNull()
    expect(svg.getAttribute('viewBox')).toMatch(/^0 0 \d+ \d+$/)
  })

  it('renders no path when every value is null', () => {
    const { container } = render(<Sparkline values={[null, null]} />)

    expect(svgOf(container).querySelector('path')).toBeNull()
  })

  it('draws the line with currentColor and a non-scaling stroke', () => {
    render(<Sparkline values={[3, 5, 4, 8]} testId="model-spark" />)
    const svg = screen.getByTestId('model-spark')
    const path = svg.querySelector('path')

    expect(path).not.toBeNull()
    expect(path?.getAttribute('d')?.startsWith('M')).toBe(true)
    expect(path?.getAttribute('stroke')).toBe('currentColor')
    expect(path?.getAttribute('vector-effect')).toBe('non-scaling-stroke')
  })

  it('breaks the line where a value is null', () => {
    const { container } = render(<Sparkline values={[1, null, 2, null, 3]} />)
    const d = svgOf(container).querySelector('path')?.getAttribute('d') ?? ''

    expect(d.match(/M/g)).toHaveLength(3)
    expect(d.match(/L/g) ?? []).toHaveLength(0)
  })

  it.each([
    ['default', 'text-ink-4'],
    ['warning', 'text-warning'],
    ['danger', 'text-danger'],
  ] as const)('applies the %s tone as %s', (tone, expected) => {
    const { container } = render(
      <Sparkline values={[1, 2]} tone={tone} />,
    )

    expect(svgOf(container).getAttribute('class')).toContain(expected)
  })

  it('carries the base 22×72 box and lets className override a dimension', () => {
    const { container: base } = render(<Sparkline values={[1, 2]} />)
    expect(svgOf(base).getAttribute('class')).toContain('h-[22px]')
    expect(svgOf(base).getAttribute('class')).toContain('w-[72px]')

    const { container } = render(
      <Sparkline values={[1, 2]} className="w-[120px]" />,
    )
    const className = svgOf(container).getAttribute('class') ?? ''

    expect(className).toContain('w-[120px]')
    expect(className).not.toContain('w-[72px]')
    expect(className).toContain('h-[22px]')
  })
})
