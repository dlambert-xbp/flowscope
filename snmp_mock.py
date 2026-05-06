"""
FlowScope — mock SNMP client.

This module is a stand-in for the real pysnmp-lextudio client introduced in
Phase 3. It does no network I/O. Given a host and a list of ifindexes it
synthesizes deterministic ifDescr / ifAlias / ifSpeed values keyed by
sha1(host|ifindex), so the rest of the system (storage, scheduling,
bindings, dashboard) can be exercised end-to-end without real switches.

The scheduler in app.py picks between this and snmp_client.py via the
FLOWSCOPE_SNMP_MOCK env var. When mock mode is on, profile credentials are
*ignored* — the client just looks at host + ifindex.

Public surface:
  MockSNMPClient(community="public", version="2c", timeout=2.0)
    .walk_iftable(host, ifindexes) -> {ifindex: {"name", "alias", "speed_bps"}}
"""

import hashlib


class MockSNMPClient:
    """Deterministic synthetic SNMP responses for development."""

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

    def walk_iftable(self, host, ifindexes):
        """Return {ifindex: {"name", "alias", "speed_bps"}} for the given host.

        A real implementation would walk the IF-MIB OIDs:
          ifDescr     1.3.6.1.2.1.2.2.1.2
          ifAlias     1.3.6.1.2.1.31.1.1.1.18
          ifHighSpeed 1.3.6.1.2.1.31.1.1.1.15  (Mb/s)
        and merge by ifindex. The mock just synthesizes plausible values
        for the ifindexes FlowScope has already observed."""
        out = {}
        for ifindex in ifindexes:
            h = self._hash(host, ifindex)
            prefix = self._PORT_PREFIXES[h % len(self._PORT_PREFIXES)]
            slot   = (h >> 8) % 48 + 1
            speed  = self._SPEEDS[(h >> 16) % len(self._SPEEDS)]
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
