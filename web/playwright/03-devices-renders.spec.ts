import { expect, test } from '@playwright/test'
import { recordConsoleErrors } from './helpers'

// Scenario 3: Devices table renders.
//
// On the Devices tab, the directory either shows one or more device
// rows OR shows the "no exporters seen in window" empty state. Both
// are acceptable against a fresh, empty stack with no exporters
// configured. No JS errors.
//
// The empty-stack branch is what CI hits today (no synthetic traffic
// is driven before the suite runs). Once flow-injection lands in a
// follow-up, this same test will see device-row elements and pass
// the same way — the disjunction keeps it stable across both.

test('devices tab renders rows or empty state', async ({ page }) => {
  const errors = recordConsoleErrors(page)

  await page.goto('/')
  await page.getByTestId('tab-devices').click()
  await expect(page.getByTestId('tab-panel-devices')).toBeVisible()

  // Wait for either a device row or the empty-state to appear. The
  // initial `devices` query takes ~tens-of-ms; the OR keeps us from
  // racing the React mount.
  const rows = page.getByTestId('device-row')
  const empty = page.getByTestId('devices-empty')
  await expect(rows.first().or(empty)).toBeVisible({ timeout: 10_000 })

  expect(errors, `unexpected console errors:\n${errors.join('\n')}`).toEqual([])
})
