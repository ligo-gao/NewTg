/** One request's path through the gateway, in order (F-09-01, F-09-07). */
export const TRACE_STAGES = ['auth', 'quota', 'route', 'translate', 'stream', 'settle'] as const
export type TraceStage = (typeof TRACE_STAGES)[number]

/** The six shipped capabilities; each maps to `public.home.features.<id>.*`. */
export type CapabilityId =
  | 'multi_upstream'
  | 'billing'
  | 'streaming'
  | 'ratelimit'
  | 'stateless'
  | 'credentials'
