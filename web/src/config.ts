// Effective runtime config from /api/config/effective. Hydrated once
// at boot before React mounts; readers consume it lazily so module
// import order doesn't matter.
//
// Operator-set localStorage / URL values still win over these defaults
// — the config seeds first-time visitors and provides a tenant-wide
// brand name. See web/src/theme.tsx and web/src/timeRange.ts for
// consumers.

export type EffectiveConfig = {
  display_name: string
  default_theme: 'light' | 'dark' | 'system'
  default_time_range: string
  timezone: string
}

const DEFAULTS: EffectiveConfig = {
  display_name: 'FlowScope',
  default_theme: 'system',
  default_time_range: '5m',
  timezone: 'UTC',
}

let cached: EffectiveConfig = DEFAULTS

// hydrateConfig fetches /api/config/effective with a short timeout so
// a slow or down api can't block the SPA from rendering. On failure,
// readers see DEFAULTS — every consumer must tolerate that.
export async function hydrateConfig(): Promise<void> {
  try {
    const r = await fetch('/api/config/effective', {
      signal: AbortSignal.timeout(750),
    })
    if (!r.ok) return
    const body = (await r.json()) as Partial<EffectiveConfig>
    cached = { ...DEFAULTS, ...body }
  } catch {
    // network error / timeout / abort — keep defaults
  }
}

export function getConfig(): EffectiveConfig {
  return cached
}
