# FlowScope — TODO

Short list of decided-on changes. Keep tight; not a wishlist.

## Done

- [x] **Theme switcher** — Light / Dark / System, persisted in
  `localStorage["flowscope:theme"]`, bottom-left of the footer.
  Implemented in `web/index.html`.

- [x] **Configurable time range on graphs (3m / 15m / 1h / 6h).**
  Selector lives in the Overview timeseries panel head, persisted in
  `localStorage["flowscope:range"]`. Backend (`/api/timeseries`) reads
  from the in-memory ring for windows ≤ 5 min and from SQLite for longer
  windows, downsampled server-side to ~600 buckets returned as bytes/sec.
  6 h cap matches the SQLite prune horizon.

## Planned

- [ ] **Time-window the other Overview aggregations (top talkers / top
  ports / protocols).** Right now those still aggregate over whatever
  happens to be in the 5000-entry `recent_flows` ring, regardless of the
  range selector. Honoring the range means routing them through SQLite.
  Pre-aggregation (or SQL `GROUP BY src_ip, dst_ip` with a `LIMIT`) will
  matter at 6 h windows.
