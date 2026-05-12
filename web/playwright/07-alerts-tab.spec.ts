import { expect, test } from '@playwright/test'
import { recordConsoleErrors } from './helpers'

// Scenario 7: Alerts tab loads.
//
// The Alerts list renders either rows or an empty state, and
// clicking a row opens the AlertDetail modal (PR #31 feature).
//
// Against an empty stack with no rules firing, the Alerts list is
// empty → the click-to-detail path is exercised only when synth
// flow injection lands in a later task. We still verify the empty-
// state copy renders without errors today.

test('alerts tab loads and opens detail modal when rows exist', async ({ page }) => {
  const errors = recordConsoleErrors(page)

  await page.goto('/')
  await page.getByTestId('tab-alerts').click()
  await expect(page.getByTestId('tab-panel-alerts')).toBeVisible()

  // Wait for either the alerts list or its empty state.
  const list = page.getByTestId('alerts-list')
  const empty = page.getByTestId('alerts-empty')
  await expect(list.or(empty)).toBeVisible({ timeout: 10_000 })

  // If we have rows, exercise the click-to-detail-modal path. If
  // the stack is empty, the row test is skipped: the AlertDetail
  // regression surface is gated on having an alert to click. The
  // empty-state assertion above still passes regardless.
  if (await empty.isVisible().catch(() => false)) {
    test.info().annotations.push({
      type: 'empty-stack-skip',
      description: 'no alerts in window → modal click path skipped',
    })
    expect(errors, `unexpected console errors:\n${errors.join('\n')}`).toEqual([])
    return
  }

  const firstOpenBtn = page.getByTestId('alert-row-open').first()
  await expect(firstOpenBtn).toBeVisible()
  await firstOpenBtn.click()
  await expect(page.getByTestId('alert-detail-modal')).toBeVisible({
    timeout: 5_000,
  })

  expect(errors, `unexpected console errors:\n${errors.join('\n')}`).toEqual([])
})
