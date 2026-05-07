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
        """Return {ifindex: {...}} for the given host. The mock synthesizes
        plausible values for the ifindexes FlowScope has already observed.

        Returned dict shape mirrors the real client's:
            name, alias, speed_bps,
            admin_status, oper_status,
            in_errors, out_errors, in_discards, out_discards,
            mtu, mac"""
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
            mac_bytes = [(h >> (i * 4)) & 0xFF for i in range(6)]
            mac_bytes[0] |= 0x02
            mac_bytes[0] &= 0xFE
            mac = ":".join(f"{b:02x}" for b in mac_bytes)
            # Most ports are up; a small fraction admin-down or oper-down
            # so the modal has something to show besides green.
            tag2 = (h >> 28) & 0xF
            admin = 2 if tag2 == 0 else 1
            oper  = 2 if (admin == 2 or tag2 == 1) else 1
            out[ifindex] = {
                "name":         f"{prefix}{slot}",
                "alias":        alias,
                "speed_bps":    speed,
                "admin_status": admin,
                "oper_status":  oper,
                "in_errors":    h % 7,
                "out_errors":   (h >> 3) % 5,
                "in_discards":  (h >> 5) % 11,
                "out_discards": (h >> 7) % 9,
                "mtu":          1500 if (h >> 9) & 1 else 9000,
                "mac":          mac,
            }
        return out
