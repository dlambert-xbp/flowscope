# Playwright smoke tests

End-to-end smoke tests for the FlowScope web SPA, run against the
docker-compose stack (CI) or a local stack (dev). See TASKS.md #18
and the PR that landed this directory for context.

## One-time setup

From the `web/` directory:

```bash
npm install
npm run e2e:install
```

The second command downloads the Chromium browser Playwright
uses. On Linux it also installs the OS packages Chromium needs
(`--with-deps`).

## Running locally

The tests do NOT start a dev server — they expect the compose
stack to already be running. From the repo root:

```bash
make bootstrap-secrets      # one-time: create secrets/snmp_master
docker compose up -d --build
```

Then from `web/`:

```bash
npm run e2e
```

The `baseURL` in `playwright.config.ts` points at
`http://localhost`, which is the nginx in the `web` container.

## Debugging

```bash
npm run e2e:ui
```

opens the Playwright UI mode for interactive test development —
hover any step in the timeline to see the DOM state at that moment.

## v1 scope

Seven smoke scenarios covering the critical user paths an operator
would notice immediately if broken:

1. Overview loads (brand bar + health panel).
2. Top tab navigation (Overview / Flows / Devices / Alerts / Settings).
3. Devices table renders rows or empty state.
4. Walk-now click reaches the API (regression-canary for PR #49).
5. Devices → Neighbors sub-tab mounts (topology canvas).
6. Settings → Auth & tokens form is interactive.
7. Alerts tab renders; click-to-detail-modal opens when rows exist.

Tests 4, 5, and 7's click-through path are skipped when the stack
is empty (no exporters / no alerts). Synthetic flow injection is
deferred to a follow-up task.

## Selectors

All assertions use `data-testid` attributes. **Never** select by
visible text or CSS class — both churn frequently. If a test needs
a new anchor point, add a `data-testid` to the component in the
same PR.
