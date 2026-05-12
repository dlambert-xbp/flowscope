import { expect, test } from '@playwright/test'
import { recordConsoleErrors } from './helpers'

// Scenario 4: Walk-now click reaches the API.
//
// This is the regression-canary for the PR #49 bug class: a click
// handler that should issue a POST `/api/devices/<exporter>/snmp/
// walk` but, due to missing auth headers or a broken click handler,
// silently no-ops in the browser.
//
// The test:
//   1. Opens the Devices tab.
//   2. Skips if the directory is empty (no device, no walk-now
//      button — synthetic traffic injection is deferred to a later
//      task; with an empty stack this regression is not testable).
//   3. Listens for any POST to /api/devices/*/snmp/walk.
//   4. Clicks the first walk-now button.
//   5. Asserts the network call fired. The status code is ignored —
//      a 503 ("snmp service no creds yet") still proves the click
//      reached the api, which is the contract we care about.
//
// Without this test, a regression where the auth middleware ate
// the request (PR #38 → PR #49) or where the click handler stopped
// calling api.requestSnmpWalk would only surface in manual QA.

test('walk-now button issues POST /api/devices/*/snmp/walk', async ({ page }) => {
  const errors = recordConsoleErrors(page)

  await page.goto('/')
  await page.getByTestId('tab-devices').click()
  await expect(page.getByTestId('tab-panel-devices')).toBeVisible()

  // Wait for the directory to settle into either rows or empty.
  const firstRow = page.getByTestId('device-row').first()
  const empty = page.getByTestId('devices-empty')
  await expect(firstRow.or(empty)).toBeVisible({ timeout: 10_000 })

  // Empty stack → no walk-now button exists. Skip; this regression
  // is gated on having at least one observed exporter. See PR body
  // for the deferred synthetic-flow-injection follow-up.
  if (await empty.isVisible().catch(() => false)) {
    test.skip(true, 'empty stack: no device rows to walk-now against')
  }

  // First device is auto-selected on first paint (Devices.tsx auto-
  // selects index 0 when nothing is in the URL). Wait for the
  // FeatureHeader's walk-now button to appear.
  const walkNow = page.getByTestId('walk-now-button').first()
  await expect(walkNow).toBeVisible({ timeout: 10_000 })

  // Arm the network watcher BEFORE the click so we don't miss the
  // request — Playwright resolves waitForRequest from the moment
  // it's called.
  const reqPromise = page.waitForRequest(
    (req) =>
      req.method() === 'POST' &&
      /\/api\/devices\/[^/]+\/snmp\/walk$/.test(new URL(req.url()).pathname),
    { timeout: 10_000 },
  )

  await walkNow.click()
  const req = await reqPromise

  // Verify the request was actually issued. The match condition
  // above is the assertion — if waitForRequest times out, the test
  // fails. We still record the URL so the report is useful.
  expect(req.method()).toBe('POST')

  expect(errors, `unexpected console errors:\n${errors.join('\n')}`).toEqual([])
})
