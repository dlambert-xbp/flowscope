import { expect, test } from '@playwright/test'
import { recordConsoleErrors } from './helpers'

// Scenario 2: Tab navigation.
//
// Walks the top nav in order — Overview / Flows / Devices / Alerts /
// Settings — and verifies each tab's panel becomes visible. No JS
// errors in console along the way.
//
// The SPA uses tab state (App.tsx) rather than React Router; each
// click swaps the panel under <main data-testid="tab-panel-<id>">.
// A future router migration won't break this test because the
// `data-testid="tab-<id>"` buttons and `data-testid="tab-panel-<id>"`
// container are the stable contract.

const TABS = ['overview', 'flows', 'devices', 'alerts', 'settings'] as const

test('all top tabs render their panels', async ({ page }) => {
  const errors = recordConsoleErrors(page)

  await page.goto('/')

  for (const id of TABS) {
    await page.getByTestId(`tab-${id}`).click()
    await expect(page.getByTestId(`tab-${id}`)).toHaveAttribute(
      'aria-selected',
      'true',
    )
    await expect(page.getByTestId(`tab-panel-${id}`)).toBeVisible()
  }

  expect(errors, `unexpected console errors:\n${errors.join('\n')}`).toEqual([])
})
