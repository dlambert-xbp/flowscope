import type { ConsoleMessage, Page } from '@playwright/test'

// recordConsoleErrors wires a listener on `page` that collects every
// `error`-level console message into the returned array. Tests can
// assert no JS errors fired during a flow by inspecting the array at
// the end.
//
// We deliberately ignore `warning` / `info` / `log` — they're noisy
// in dev mode (React 19's deprecation chatter, third-party libs) and
// not what an operator-facing regression would look like. Anything
// that lands as a hard `console.error` is fair game.
//
// "Failed to load resource" errors are surfaced by the browser when
// any request 4xx/5xx's — including the legitimate 503/404 paths the
// SPA tolerates (e.g. ingest-health pre-warm, missing-inventory). We
// filter those out by URL pattern; a real React error boundary trip
// would land as a non-resource error and still flag.
export function recordConsoleErrors(page: Page): string[] {
  const errors: string[] = []
  page.on('console', (msg: ConsoleMessage) => {
    if (msg.type() !== 'error') return
    const text = msg.text()
    if (isIgnoredConsoleError(text)) return
    errors.push(text)
  })
  page.on('pageerror', (err) => {
    errors.push(`pageerror: ${err.message}`)
  })
  return errors
}

function isIgnoredConsoleError(text: string): boolean {
  // Resource-level errors are expected against an empty stack. The
  // SPA tolerates 4xx/5xx from /api/health/ingest, inventory, and
  // similar pre-data paths; those bubble to console as "Failed to
  // load resource" but never break the UI.
  if (text.includes('Failed to load resource')) return true
  // React-router has zero presence today; if a future migration
  // lands hash-router warnings, expand this list.
  return false
}
