# FlowScope — TODO

Short list of decided-on changes. Keep tight; not a wishlist.

## Done

- [x] **Theme switcher** — Light / Dark / System, persisted in
  `localStorage["flowscope:theme"]`, bottom-left of the footer.
  Implemented in `web/index.html`.

## Planned

- [ ] **Configurable time range on graphs.** Everything is currently
  hard-coded to 3 minutes (`/api/timeseries?seconds=180` in `refreshAll`).
  Add a selector — e.g. `5m / 15m / 1h / 6h` — that drives the timeseries
  query and any other windowed views. Notes:
    - SQLite prune in `db_prune_loop` caps real history at ~6 h, so that's
      the upper bound unless retention is extended.
    - `recent_flows` is a 5000-entry ring (no time bound) and feeds top
      talkers / top ports / protocols. A user-selected window for those
      means routing aggregations through SQLite instead of the ring.
    - The `seconds` query param on `/api/timeseries` already exists;
      change is mostly frontend.
