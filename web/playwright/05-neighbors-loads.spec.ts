import { expect, test } from '@playwright/test'
import { recordConsoleErrors } from './helpers'

// Scenario 5: Devices → Neighbors loads.
//
// Sanity-check the LLDP/CDP topology component (PR #48 surface).
// The component renders a fully-functional react-flow canvas even
// with zero data — it falls back to an empty-state panel. We just
// want to confirm the sub-tab mounts without crashing and without
// JS errors.
//
// Skips on an empty Devices directory (no selected exporter → no
// Neighbors sub-tab to navigate to). Same caveat as scenario 4.

test('devices → neighbors sub-tab mounts', async ({ page }) => {
  const errors = recordConsoleErrors(page)

  await page.goto('/')
  await page.getByTestId('tab-devices').click()
  await expect(page.getByTestId('tab-panel-devices')).toBeVisible()

  const firstRow = page.getByTestId('device-row').first()
  const empty = page.getByTestId('devices-empty')
  await expect(firstRow.or(empty)).toBeVisible({ timeout: 10_000 })

  if (await empty.isVisible().catch(() => false)) {
    test.skip(true, 'empty stack: no devices → no Neighbors sub-tab')
  }

  // The Neighbors sub-tab only renders once an exporter is selected.
  // Auto-selection handles that on first paint; the sub-tab button
  // appears in the header.
  await page.getByTestId('devices-subtab-neighbors').click()

  // The TopologyGraph canvas wrapper renders in three branches
  // (loading / error / empty / data). All four paths carry the same
  // data-testid so this test is stable regardless of how much data
  // SNMP has walked.
  await expect(page.getByTestId('topology-canvas')).toBeVisible({
    timeout: 10_000,
  })

  expect(errors, `unexpected console errors:\n${errors.join('\n')}`).toEqual([])
})
