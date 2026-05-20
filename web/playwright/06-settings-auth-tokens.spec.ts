import { expect, test } from '@playwright/test'
import { recordConsoleErrors } from './helpers'

// Scenario 6: Settings → Auth & tokens loads.
//
// Click into Settings, navigate to the Auth & tokens section via
// the rail, and verify the form is interactive: the token input is
// enabled and the save button is present. This protects the
// SessionToken form's contract — the "paste a token and it
// becomes the X-Auth-Token for writes" UX — from accidental
// regressions.

test('settings → auth & tokens form is interactive', async ({ page }) => {
  const errors = recordConsoleErrors(page)

  await page.goto('/')
  await page.getByTestId('tab-settings').click()
  await expect(page.getByTestId('tab-panel-settings')).toBeVisible()

  // Settings sections are tabs within the Settings shell; the rail
  // on the left exposes one button per section.
  await page.getByTestId('settings-section-auth').click()
  await expect(page.getByTestId('auth-tokens-section')).toBeVisible()

  // The browser-session-token form is now demoted behind an
  // "advanced" disclosure (LoginPage owns the primary paste flow).
  // On a fresh browser with no token saved the disclosure is
  // collapsed — click to reveal the form before asserting the input
  // is interactive.
  await page.getByTestId('auth-session-token-show').click()

  // Token input and save button: the regression target. If either
  // disappears or stops accepting input, this test fails.
  const input = page.getByTestId('auth-session-token-input')
  await expect(input).toBeVisible()
  await expect(input).toBeEnabled()

  const save = page.getByTestId('auth-session-token-save')
  await expect(save).toBeVisible()
  await expect(save).toBeEnabled()

  // Typing into the input must populate its value — proves the
  // controlled component is wired correctly. We don't click save:
  // setSettingsAuthToken writes to localStorage, which would
  // bleed between tests if we did.
  await input.fill('fls_e2e_smoke_dummy_not_used')
  await expect(input).toHaveValue('fls_e2e_smoke_dummy_not_used')

  expect(errors, `unexpected console errors:\n${errors.join('\n')}`).toEqual([])
})
