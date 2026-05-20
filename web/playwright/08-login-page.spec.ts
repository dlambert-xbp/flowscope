import { expect, test } from '@playwright/test'
import { recordConsoleErrors } from './helpers'

// Scenario 8: AuthGate → LoginPage.
//
// Forces the boot-time probe (/api/summary?seconds=1) to return 401
// and verifies the cold-boot login screen renders, branching its UI
// on the WWW-Authenticate header per AuthGate's contract.
//
//   • With "WWW-Authenticate: oidc": SSO button is the primary
//     action; the API-token form is collapsed behind a disclosure.
//   • Without the header: SSO is absent, the API-token form is open
//     by default — there is no SSO to choose between.
//
// These are the two states LoginPage was designed to switch on; if
// the gate stops branching the un-authed visitor will see the wrong
// affordance, which is the exact regression this test protects.

test.describe('login page', () => {
  test('SSO is primary when WWW-Authenticate: oidc is present', async ({ page }) => {
    const errors = recordConsoleErrors(page)

    // Intercept the gate's probe — and ONLY the gate's probe. Every
    // other request flows through to the dev backend. The route runs
    // before any navigation so AuthGate's first /api/summary call
    // sees the synthetic 401 instead of the dev-server's 200.
    await page.route('**/api/summary*', (route) => {
      route.fulfill({
        status: 401,
        headers: { 'WWW-Authenticate': 'oidc realm="flowscope"' },
        body: 'unauthenticated',
      })
    })

    await page.goto('/')

    // SSO button must be visible and the API-token disclosure must
    // be collapsed (the token input is hidden behind the disclosure
    // button when SSO is the primary path).
    await expect(page.getByTestId('login-sso-button')).toBeVisible()
    await expect(page.getByTestId('login-token-disclosure')).toBeVisible()
    await expect(page.getByTestId('login-token-input')).not.toBeVisible()

    // Clicking the disclosure expands the API-token form. This is
    // the escape hatch for operators who prefer not to round-trip
    // through the IdP.
    await page.getByTestId('login-token-disclosure').click()
    await expect(page.getByTestId('login-token-input')).toBeVisible()
    await expect(page.getByTestId('login-token-submit')).toBeVisible()

    expect(errors, `unexpected console errors:\n${errors.join('\n')}`).toEqual([])
  })

  test('token form is primary when OIDC is not advertised', async ({ page }) => {
    const errors = recordConsoleErrors(page)

    await page.route('**/api/summary*', (route) => {
      // Plain 401 — no WWW-Authenticate header. AuthGate reads this
      // as "auth required, but no SSO configured" and LoginPage
      // surfaces the API-token form without the SSO button.
      route.fulfill({
        status: 401,
        body: 'unauthenticated',
      })
    })

    await page.goto('/')

    // SSO button should NOT be in the DOM at all — there's nothing
    // to sign in with. The token input is open from first paint.
    await expect(page.getByTestId('login-sso-button')).toHaveCount(0)
    await expect(page.getByTestId('login-token-input')).toBeVisible()

    // Typing into the input proves the controlled component works.
    // We do NOT click submit — that writes to localStorage and would
    // bleed between tests.
    const input = page.getByTestId('login-token-input')
    await input.fill('fls_e2e_smoke_login_dummy')
    await expect(input).toHaveValue('fls_e2e_smoke_login_dummy')

    expect(errors, `unexpected console errors:\n${errors.join('\n')}`).toEqual([])
  })
})
