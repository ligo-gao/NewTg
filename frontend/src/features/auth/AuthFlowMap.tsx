import {
  useLayoutEffect,
  useRef,
  useState,
  type CSSProperties,
} from 'react'

import {
  CLIENT_FLOW_ICONS,
  MODEL_FLOW_ICONS,
  type FlowIconData,
} from './auth-flow-icons'

/**
 * The sign-in "hall" diagram: clients on the left, upstream models on the
 * right, the routing mark in the middle of the stage. Nothing here is
 * interactive or announced — the whole map is decorative, and the page never
 * repeats the brand names as text.
 *
 * Geometry is measured rather than hard-coded: the stage owns the viewBox, the
 * hub is the mark's measured centre, and both columns are queried from the DOM.
 * That keeps the curves horizontal at both ends at any viewport size.
 */

interface Geometry {
  width: number
  height: number
  clientPaths: string[]
  modelPaths: string[]
}

/** Per-line speeds keep the twelve streams from pulsing as one metronome. */
const CLIENT_SPEEDS = ['2.2s', '2.65s', '2.35s', '2.8s', '2.45s', '2.9s'] as const
const MODEL_SPEEDS = ['2.4s', '2.75s', '2.1s', '2.55s', '2.3s', '2.85s'] as const

const COMPACT_QUERY = '(max-width: 1023px)'

function round(value: number): number {
  return Math.round(value * 100) / 100
}

/**
 * Left curves run client → hub; right curves run hub → model. The path
 * direction is what makes the same dash animation stream inbound on the left
 * and outbound on the right.
 */
function clientPath(startX: number, startY: number, hubX: number, hubY: number): string {
  const span = Math.max(1, hubX - startX)
  const c1x = round(startX + span * 0.5)
  const c2x = round(hubX - span * 0.5)
  return (
    'M' + round(startX) + ' ' + round(startY) +
    ' C' + c1x + ' ' + round(startY) + ' ' + c2x + ' ' + round(hubY) +
    ' ' + round(hubX) + ' ' + round(hubY)
  )
}

function modelPath(startX: number, startY: number, hubX: number, hubY: number): string {
  const span = Math.max(1, startX - hubX)
  const c1x = round(startX - span * 0.5)
  const c2x = round(hubX + span * 0.5)
  return (
    'M' + round(hubX) + ' ' + round(hubY) +
    ' C' + c2x + ' ' + round(hubY) + ' ' + c1x + ' ' + round(startY) +
    ' ' + round(startX) + ' ' + round(startY)
  )
}

function FlowMark({
  icon,
  className,
}: {
  icon: FlowIconData
  className: string
}) {
  return (
    <svg
      className={className}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.3"
      strokeLinecap="round"
      strokeLinejoin="round"
      focusable="false"
    >
      {icon.paths.map((d) => (
        <path key={d} d={d} />
      ))}
    </svg>
  )
}

function flowStyle(speed: string, stagger: string): CSSProperties {
  return { '--speed': speed, '--stagger': stagger } as CSSProperties
}

export function AuthFlowMap() {
  const stageRef = useRef<HTMLDivElement>(null)
  const gateRef = useRef<HTMLDivElement>(null)
  const [geometry, setGeometry] = useState<Geometry | null>(null)

  useLayoutEffect(() => {
    const stage = stageRef.current
    const gate = gateRef.current
    if (stage === null || gate === null) return

    let cancelled = false

    const measure = () => {
      if (cancelled) return
      const width = stage.clientWidth
      const height = stage.clientHeight
      if (width === 0 || height === 0) return
      if (
        typeof window.matchMedia === 'function' &&
        window.matchMedia(COMPACT_QUERY).matches
      ) {
        return
      }

      const markSize = Math.max(20, Math.min(38, height * 0.1))
      stage.style.setProperty('--mark', `${markSize.toFixed(1)}px`)

      const stageRect = stage.getBoundingClientRect()
      const gateRect = gate.getBoundingClientRect()
      const hubX = gateRect.left + gateRect.width / 2 - stageRect.left
      const hubY = gateRect.top + gateRect.height / 2 - stageRect.top

      const clientItems = stage.querySelectorAll<HTMLElement>('.auth-hall-clients li')
      const modelItems = stage.querySelectorAll<HTMLElement>('.auth-hall-providers li')

      const clientPaths = Array.prototype.map.call(
        clientItems,
        (item: HTMLElement) => {
          const rect = item.getBoundingClientRect()
          const startX = rect.right - stageRect.left + 6
          const startY = rect.top + rect.height / 2 - stageRect.top
          return clientPath(startX, startY, hubX, hubY)
        },
      ) as string[]

      const modelPaths = Array.prototype.map.call(
        modelItems,
        (item: HTMLElement) => {
          const rect = item.getBoundingClientRect()
          const startX = rect.left - stageRect.left - 6
          const startY = rect.top + rect.height / 2 - stageRect.top
          return modelPath(startX, startY, hubX, hubY)
        },
      ) as string[]

      setGeometry({ width, height, clientPaths, modelPaths })
    }

    measure()

    // The stage resizes with the pane, and webfonts change the statement's
    // height, so both need a re-measure.
    let observer: ResizeObserver | undefined
    if (typeof ResizeObserver !== 'undefined') {
      observer = new ResizeObserver(measure)
      observer.observe(stage)
    }
    window.addEventListener('resize', measure)
    if (document.fonts !== undefined) {
      void document.fonts.ready.then(measure)
    }

    return () => {
      cancelled = true
      observer?.disconnect()
      window.removeEventListener('resize', measure)
    }
  }, [])

  return (
    <div className="auth-hall-stage" ref={stageRef} aria-hidden="true">
      <svg
        className="auth-hall-routing"
        viewBox={
          geometry === null
            ? undefined
            : `0 0 ${geometry.width} ${geometry.height}`
        }
        focusable="false"
      >
        {geometry?.clientPaths.map((d, index) => (
          <path
            key={`client-line-${CLIENT_FLOW_ICONS[index]?.name ?? index}`}
            className="line"
            d={d}
            pathLength={100}
            style={{ animationDelay: `${index * 70}ms` }}
          />
        ))}
        {geometry?.clientPaths.map((d, index) => (
          <path
            key={`client-flow-${CLIENT_FLOW_ICONS[index]?.name ?? index}`}
            className="flow"
            d={d}
            style={flowStyle(
              CLIENT_SPEEDS[index % CLIENT_SPEEDS.length] ?? '2.4s',
              `${(-(index * 0.31)).toFixed(2)}s`,
            )}
          />
        ))}
        {geometry?.modelPaths.map((d, index) => (
          <path
            key={`model-line-${MODEL_FLOW_ICONS[index]?.name ?? index}`}
            className="line"
            d={d}
            pathLength={100}
            style={{ animationDelay: `${420 + index * 70}ms` }}
          />
        ))}
        {geometry?.modelPaths.map((d, index) => (
          <path
            key={`model-flow-${MODEL_FLOW_ICONS[index]?.name ?? index}`}
            className="flow"
            d={d}
            style={flowStyle(
              MODEL_SPEEDS[index % MODEL_SPEEDS.length] ?? '2.4s',
              `${(-(index * 0.37)).toFixed(2)}s`,
            )}
          />
        ))}
      </svg>

      <div className="auth-hall-gate" ref={gateRef}>
        <svg className="auth-hall-gate-ring" viewBox="0 0 100 100" focusable="false">
          <circle cx="50" cy="50" r="49" />
          <circle
            className="arc"
            cx="50"
            cy="50"
            r="49"
            pathLength={100}
            strokeDasharray="25 75"
            transform="rotate(-90 50 50)"
          />
        </svg>
        <svg
          className="auth-hall-gate-mark"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.15"
          strokeLinecap="round"
          focusable="false"
        >
          <path d="M3 6h3.5c2 0 3.5 1.2 4.5 3l2 6c1 1.8 2.5 3 4.5 3H21" />
          <path d="M3 18h3.5c2 0 3.5-1.2 4.5-3l2-6c1-1.8 2.5-3 4.5-3H21" />
        </svg>
      </div>

      <ul className="auth-hall-clients">
        {CLIENT_FLOW_ICONS.map((icon) => (
          <li key={icon.name}>
            <FlowMark icon={icon} className="client-mark" />
          </li>
        ))}
      </ul>

      <ul className="auth-hall-providers">
        {MODEL_FLOW_ICONS.map((icon) => (
          <li key={icon.name}>
            <FlowMark icon={icon} className="provider-mark" />
          </li>
        ))}
      </ul>
    </div>
  )
}
