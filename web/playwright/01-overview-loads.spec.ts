import { expect, test } from '@playwright/test'
import { recordConsoleErrors } from './helpers'

// Scenario 1: Overview loads.
//
// Hits `/` and verifies the SPA boots: the brand bar renders the
// FlowScope wordmark (the default display_name from /api/config/
// effective), the Overview tab is the active one on first load, and
// the system-health panel renders. No JS errors in console.
//
// This is the canary for "did the bundle even ship?" — if it fails,
// the rest of the suite is meaningless.

test('overview loads with brand bar and health panel', async ({ page }) => {
  const errors = recordConsoleErrors(page)

  await page.goto('/')

  // Brand text from /api/config/effective → default "FlowScope".
  // Lives in the bar to the left of the tabs.
  await expect(page.getByText('FlowScope', { exact: false })).toBeVisible()

  // Overview is the default tab on first paint; aria-selected reflects
  // it. We look up by data-testid so a future label rename doesn't
  // churn this test.
  const overviewTab = page.getByTestId('tab-overview')
  await expect(overviewTab).toBeVisible()
  await expect(overviewTab).toHaveAttribute('aria-selected', 'true')

  // The tab-panel container carries the active tab's id; "overview"
  // confirms Overview is mounted.
  await expect(page.getByTestId('tab-panel-overview')).toBeVisible()

  // System-health banner — the Overview's headline panel. Anchors
  // the test against the right component even if visible copy churns.
  await expect(page.getByTestId('overview-health-panel')).toBeVisible()

  expect(errors, `unexpected console errors:\n${errors.join('\n')}`).toEqual([])
})
