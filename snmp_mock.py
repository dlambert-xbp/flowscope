"""
FlowScope — mock SNMP poller.

This module is a *scaffold* for the eventual SNMP feature. It does not perform
any real SNMP I/O. Instead, when enabled it deterministically synthesizes
ifDescr / ifAlias / ifSpeed values for every (exporter, ifindex) FlowScope has
observed via flow telemetry, so the rest of the system (state model, API,
frontend) can be exercised end-to-end before a real SNMP stack is wired in.

Why a mock first:
  - Keeps `requirements.txt` to "Flask only" until we commit to a library
    (pysnmp pulls in pyasn1 and pycryptodome; easysnmp needs net-snmp libs
    on the host). The single-file/minimal-deps philosophy in CLAUDE.md says
    the dependency choice deserves an explicit decision, not a drive-by.
  - Lets us validate the integration points (locking, API shape, frontend
    rendering) with synthetic data that's reproducible across restarts.
  - The replacement of `MockSNMPClient.walk_iftable` with a real client is
    the *only* change needed to switch from mock to live; everything else
    in this file is reusable.

Public surface used by app.py:
  - SNMP_ENABLED, SNMP_INTERVAL, SNMP_COMMUNITY (read from environment)
  - start_snmp_poller(devices, interface_stats, state_lock) — kicks off the
    background thread. Returns the Thread object (already started).
  - poller_status() — JSON-serializable dict for /api/snmp/status
"""

import hashlib
import os
import threading
import time

# ---------------------------------------------------------------------------
# Configuration (env-driven, mirroring the rest of FlowScope)
# ---------------------------------------------------------------------------

SNMP_ENABLED  = os.environ.get("FLOWSCOPE_SNMP_ENABLED", "0") in ("1", "true", "True", "yes")
SNMP_INTERVAL = int(os.environ.get("FLOWSCOPE_SNMP_INTERVAL", 60))   # seconds between polls
SNMP_COMMUNITY = os.environ.get("FLOWSCOPE_SNMP_COMMUNITY", "public")
SNMP_VERSION  = os.environ.get("FLOWSCOPE_SNMP_VERSION", "2c")        # 2c only for the mock

# Status (read by the API endpoint; guarded by _status_lock)
_status_lock = threading.Lock()
_status = {
    "enabled":      SNMP_ENABLED,
    "mock":         True,
    "interval":     SNMP_INTERVAL,
    "version":      SNMP_VERSION,
    "last_poll_ts": None,
    "last_error":   None,
    "per_device":   {},   # exporter_ip -> {"last_poll_ts", "last_error", "iface_count"}
}


# ---------------------------------------------------------------------------
# Mock client
# ---------------------------------------------------------------------------

class MockSNMPClient:
    """Stand-in for a real SNMP client. Returns deterministic synthetic data
    so reloads/restarts produce the same names for the same (host, ifindex).
    Replace `walk_iftable` with a real implementation to go live."""

    # Speed tiers cycled deterministically by ifindex hash
    _SPEEDS = [
        1_000_000_000,         # 1 Gb
        10_000_000_000,        # 10 Gb
        25_000_000_000,        # 25 Gb
        100_000_000_000,       # 100 Gb
    ]
    _PORT_PREFIXES = ["Gi0/", "Te1/", "Hu2/", "Eth1/"]

    def __init__(self, community="public", version="2c", timeout=2.0):
        self.community = community
        self.version   = version
        self.timeout   = timeout

    def _hash(self, host, ifindex, salt=""):
        h = hashlib.sha1(f"{host}|{ifindex}|{salt}".encode()).digest()
        return int.from_bytes(h[:4], "big")

    def walk_iftable(self, host, known_ifindexes):
        """Return {ifindex: {"name", "alias", "speed_bps"}} for the given host.

        A real implementation would do an SNMP walk of:
          ifDescr     1.3.6.1.2.1.2.2.1.2
          ifAlias     1.3.6.1.2.1.31.1.1.1.18
          ifHighSpeed 1.3.6.1.2.1.31.1.1.1.15  (Mb/s)
        and merge by ifindex. The mock just synthesizes plausible values
        for the ifindexes FlowScope has already observed.
        """
        out = {}
        for ifindex in known_ifindexes:
            h = self._hash(host, ifindex)
            prefix = self._PORT_PREFIXES[h % len(self._PORT_PREFIXES)]
            slot   = (h >> 8) % 48 + 1
            speed  = self._SPEEDS[(h >> 16) % len(self._SPEEDS)]
            # Aliases: most interfaces are blank; some are tagged "uplink",
            # "downlink-to-X", or a synthetic customer name.
            alias = ""
            tag = (h >> 24) % 8
            if tag == 0:
                alias = f"uplink-to-core-{(h >> 4) % 8 + 1}"
            elif tag == 1:
                alias = f"downlink-{ifindex}"
            elif tag == 2:
                alias = f"customer-{(h >> 12) % 200:03d}"
            out[ifindex] = {
                "name":      f"{prefix}{slot}",
                "alias":     alias,
                "speed_bps": speed,
            }
        return out


# ---------------------------------------------------------------------------
# Polling loop
# ---------------------------------------------------------------------------

def _poll_once(client, devices, interface_stats, state_lock):
    """One pass over all known exporters. Pulls the ifindex set out under
    the lock, walks the (mock) SNMP table outside the lock, then writes the
    enrichment back under the lock. This mirrors the lock discipline already
    used by the rDNS thread — no blocking I/O while holding state_lock."""
    now = time.time()

    with state_lock:
        targets = list(devices.keys())
        # Snapshot of ifindexes per exporter; outside the lock we can't
        # safely iterate interface_stats.
        per_host_ifs = {}
        for (exp, ifindex) in interface_stats.keys():
            per_host_ifs.setdefault(exp, []).append(ifindex)

    overall_error = None
    for host in targets:
        ifindexes = per_host_ifs.get(host, [])
        if not ifindexes:
            continue
        try:
            walk = client.walk_iftable(host, ifindexes)
        except Exception as e:
            err = f"{type(e).__name__}: {e}"
            overall_error = err
            with _status_lock:
                _status["per_device"][host] = {
                    "last_poll_ts": now,
                    "last_error":   err,
                    "iface_count":  0,
                }
            continue

        # Write the enrichment back into interface_stats. We only fill the
        # SNMP-derived fields; we never touch the counter fields (those are
        # owned by record_flow / parse_sflow_counters_sample — see CLAUDE.md
        # "counter samples win").
        with state_lock:
            for ifindex, info in walk.items():
                k = (host, ifindex)
                s = interface_stats.get(k)
                if s is None:
                    continue   # interface aged out between snapshot and write
                s["name"]  = info["name"]
                # alias is a new field; tolerate older defaultdict shapes.
                s["alias"] = info.get("alias", "")
                # Don't clobber a non-zero speed already learned from sFlow
                # counter samples (those are authoritative for live links).
                if not s.get("speed_bps"):
                    s["speed_bps"] = info["speed_bps"]

        with _status_lock:
            _status["per_device"][host] = {
                "last_poll_ts": now,
                "last_error":   None,
                "iface_count":  len(walk),
            }

    with _status_lock:
        _status["last_poll_ts"] = now
        _status["last_error"]   = overall_error


def _poll_loop(client, devices, interface_stats, state_lock):
    while True:
        try:
            _poll_once(client, devices, interface_stats, state_lock)
        except Exception as e:
            with _status_lock:
                _status["last_error"] = f"{type(e).__name__}: {e}"
            print(f"[snmp] poll error: {e}")
        time.sleep(SNMP_INTERVAL)


def start_snmp_poller(devices, interface_stats, state_lock):
    """Kick off the background poller. No-op if SNMP is disabled."""
    if not SNMP_ENABLED:
        return None
    client = MockSNMPClient(community=SNMP_COMMUNITY, version=SNMP_VERSION)
    t = threading.Thread(
        target=_poll_loop,
        args=(client, devices, interface_stats, state_lock),
        daemon=True,
        name="snmp-poller",
    )
    t.start()
    print(f"[snmp]   mock poller running every {SNMP_INTERVAL}s "
          f"(community={SNMP_COMMUNITY!r}, version={SNMP_VERSION})")
    return t


def poller_status():
    with _status_lock:
        # Shallow copy so the caller can jsonify safely
        return {
            "enabled":      _status["enabled"],
            "mock":         _status["mock"],
            "interval":     _status["interval"],
            "version":      _status["version"],
            "community":    SNMP_COMMUNITY,
            "last_poll_ts": _status["last_poll_ts"],
            "last_error":   _status["last_error"],
            "per_device":   dict(_status["per_device"]),
        }
