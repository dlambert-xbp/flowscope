-- 000018_bgp_peers.sql
--
-- BGP peering inventory + state samples. The snmp service walks
-- bgpPeerTable (RFC 4273 / 1657 BGP4-MIB) and the vendor analogues
-- (cbgpPeer2Table for IPv4+IPv6 on Cisco IOS / IOS-XR / NX-OS,
-- jnxBgpM2PeerTable on Junos) on its own slower cadence and writes
-- one row per (exporter, peer_addr) per poll. The alert engine reads
-- this table for the BGPNeighborDown template; the Devices view will
-- read it in a follow-up to surface a "BGP peers" section.
--
-- Source-of-truth column resolves vendor preference at read time:
-- jnxbgp > cbgp > bgp4. Operators with mixed fleets see the richer
-- vendor view where available without the engine having to merge
-- across MIBs in the SQL.
--
-- Forward-only migration. Re-applying is a no-op on a populated
-- cluster.

CREATE TABLE IF NOT EXISTS bgp_peers
(
    polled_at        DateTime64(3, 'UTC'),
    exporter         IPv6,                                -- canonical exporter id
    peer_addr        IPv6,                                -- v4-mapped for IPv4 peers
    peer_asn         UInt32,                              -- 32-bit ASN; 16-bit fits naturally
    local_asn        UInt32,
    state            LowCardinality(String),              -- 'idle' | 'connect' | 'active' | 'opensent' | 'openconfirm' | 'established' | 'unknown'
    admin_status     LowCardinality(String) DEFAULT '',   -- 'start' | 'stop' | ''
    established_at   DateTime64(3, 'UTC') DEFAULT toDateTime64(0, 3, 'UTC'),  -- when transitioned to established (when known)
    last_change_at   DateTime64(3, 'UTC') DEFAULT toDateTime64(0, 3, 'UTC'),  -- bgpPeerInUpdateElapsedTime, when known
    afi              LowCardinality(String) DEFAULT '',   -- 'ipv4' | 'ipv6' | '' (pre-cbgp / pre-jnxbgp)
    safi             LowCardinality(String) DEFAULT '',   -- 'unicast' | 'multicast' | 'mpls-vpn' | ''
    peer_description String  DEFAULT '',                  -- bgpPeer2Description / jnxBgpM2PeerDescription, when present
    source           LowCardinality(String)               -- 'bgp4' | 'cbgp' | 'jnxbgp'
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(polled_at)
ORDER BY (exporter, peer_addr, polled_at)
TTL toDateTime(polled_at) + INTERVAL 30 DAY
SETTINGS index_granularity = 8192
