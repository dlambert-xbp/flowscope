import { defineConfig, devices } from '@playwright/test'

// FlowScope Playwright smoke-test config.
//
// These tests run against an already-running docker-compose stack —
// they do NOT launch a dev server (no `webServer` block). CI is
// responsible for bringing up the stack via `docker compose up -d`
// and waiting for /api/health/ingest to be ready before invoking the
// runner. Locally, the dev's expected to do the same.
//
// v1 scope (TASKS.md #18): seven smoke scenarios against the
// auth-bypass compose stack (no FLOWSCOPE_AUTH_TOKEN set → empty
// api_tokens table → unauth-bypass fires, every request passes).
// Auth-on variants are deferred.
//
// One chromium project for v1. Firefox / WebKit can land later
// without changing the test bodies; selectors are data-testid only.

export default defineConfig({
  testDir: './playwright',

  // The stack is a single shared resource (one ClickHouse, one api,
  // one ingest). Running tests in parallel would race on
  // localStorage state and walk-queue side-effects. Serial is the
  // correct default until we shard by suite + stack.
  fullyParallel: false,
  workers: 1,

  // CI tolerates a single retry per test — flaky network startup or
  // ClickHouse-warming hiccups shouldn't fail a PR. Local runs get
  // no retries so flakes surface immediately.
  retries: process.env.CI ? 1 : 0,

  // 30s gives slow ClickHouse-warming queries room without masking
  // a wedged dashboard. The wait-for-healthy gate in CI should
  // already ensure the api is responsive by the time tests start.
  timeout: 30_000,

  reporter: [
    ['list'],
    // HTML report lives under web/playwright-report/ and is what CI
    // uploads as an artifact on failure. `open: 'never'` keeps the
    // runner from trying to spawn a browser in headless CI.
    ['html', { open: 'never' }],
  ],

  use: {
    baseURL: 'http://localhost',
    // Screenshots + traces tell the post-mortem story when CI fails.
    // `only-on-failure` keeps green runs cheap; `on-first-retry`
    // catches the flake-pattern where the first run failed and the
    // retry passed (we still want the trace from the first attempt).
    screenshot: 'only-on-failure',
    trace: 'on-first-retry',
    video: 'retain-on-failure',
    // Ignore self-signed certs etc. — compose stack is plain HTTP at
    // the edge anyway, but this guards against future config drift.
    ignoreHTTPSErrors: true,
  },

  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
})
