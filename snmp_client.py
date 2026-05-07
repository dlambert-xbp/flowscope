"""
FlowScope — real SNMP client (pysnmp 7.x).

Replaces the mock client when FLOWSCOPE_SNMP_MOCK is unset (the default).
Walks IF-MIB to populate per-interface metadata (name, alias, speed,
admin/oper status, errors, discards, MTU, MAC) for every interface
FlowScope already knows about on a given exporter.

Public surface (matches snmp_mock.MockSNMPClient.walk_iftable):
    walk_iftable(profile_dict, host, ifindexes) -> {ifindex: {...}}
    Returned dicts may contain any of:
        name, alias, speed_bps,
        admin_status, oper_status,
        in_errors, out_errors, in_discards, out_discards,
        mtu, mac

Threading model:
    pysnmp 7.x dropped the synchronous high-level API in v3arch; only the
    asyncio path supports v3 USM cleanly. Our scheduler is thread-based,
    so each call here spins up a private event loop with `asyncio.run`,
    runs the walks, and tears the loop down. Cost is a few ms of loop
    setup per poll which is fine at 15s cadence.

OIDs (IF-MIB):
    ifDescr        1.3.6.1.2.1.2.2.1.2
    ifMtu          1.3.6.1.2.1.2.2.1.4
    ifPhysAddress  1.3.6.1.2.1.2.2.1.6     (MAC, OctetString)
    ifAdminStatus  1.3.6.1.2.1.2.2.1.7     (1=up,2=down,3=testing)
    ifOperStatus   1.3.6.1.2.1.2.2.1.8
    ifInDiscards   1.3.6.1.2.1.2.2.1.13
    ifInErrors     1.3.6.1.2.1.2.2.1.14
    ifOutDiscards  1.3.6.1.2.1.2.2.1.19
    ifOutErrors    1.3.6.1.2.1.2.2.1.20
    ifAlias        1.3.6.1.2.1.31.1.1.1.18
    ifHighSpeed    1.3.6.1.2.1.31.1.1.1.15 (Mb/s)

ifSpeed (1.3.6.1.2.1.2.2.1.5) caps at 4.29 Gb; ifHighSpeed is the modern
accessor and reports Mb/s, so we multiply by 1_000_000 to get bps.
"""

import asyncio

from pysnmp.hlapi.v3arch.asyncio import (
    SnmpEngine,
    CommunityData,
    UsmUserData,
    UdpTransportTarget,
    ContextData,
    ObjectType,
    ObjectIdentity,
    walk_cmd,
    # Auth protocols (USM)
    usmHMACMD5AuthProtocol,
    usmHMACSHAAuthProtocol,
    usmHMAC128SHA224AuthProtocol,
    usmHMAC192SHA256AuthProtocol,
    usmHMAC256SHA384AuthProtocol,
    usmHMAC384SHA512AuthProtocol,
    usmNoAuthProtocol,
    # Priv protocols
    usmDESPrivProtocol,
    usm3DESEDEPrivProtocol,
    usmAesCfb128Protocol,
    usmAesCfb192Protocol,
    usmAesCfb256Protocol,
    usmNoPrivProtocol,
)


OID_IFDESCR        = "1.3.6.1.2.1.2.2.1.2"
OID_IFMTU          = "1.3.6.1.2.1.2.2.1.4"
OID_IFPHYSADDR     = "1.3.6.1.2.1.2.2.1.6"
OID_IFADMINSTATUS  = "1.3.6.1.2.1.2.2.1.7"
OID_IFOPERSTATUS   = "1.3.6.1.2.1.2.2.1.8"
OID_IFINDISCARDS   = "1.3.6.1.2.1.2.2.1.13"
OID_IFINERRORS     = "1.3.6.1.2.1.2.2.1.14"
OID_IFOUTDISCARDS  = "1.3.6.1.2.1.2.2.1.19"
OID_IFOUTERRORS    = "1.3.6.1.2.1.2.2.1.20"
OID_IFALIAS        = "1.3.6.1.2.1.31.1.1.1.18"
OID_IFHIGHSPEED    = "1.3.6.1.2.1.31.1.1.1.15"


_AUTH_PROTOS = {
    "MD5":    usmHMACMD5AuthProtocol,
    "SHA":    usmHMACSHAAuthProtocol,
    "SHA224": usmHMAC128SHA224AuthProtocol,
    "SHA256": usmHMAC192SHA256AuthProtocol,
    "SHA384": usmHMAC256SHA384AuthProtocol,
    "SHA512": usmHMAC384SHA512AuthProtocol,
}

_PRIV_PROTOS = {
    "DES":    usmDESPrivProtocol,
    "3DES":   usm3DESEDEPrivProtocol,
    "AES128": usmAesCfb128Protocol,
    "AES192": usmAesCfb192Protocol,
    "AES256": usmAesCfb256Protocol,
}


def walk_iftable(profile, host, ifindexes):
    """Synchronous wrapper around the async walk. Raises on transport,
    auth, or PDU errors so the scheduler records last_error."""
    return asyncio.run(_walk_iftable_async(profile, host, set(int(i) for i in ifindexes)))


def _auth_for(profile):
    """Build a pysnmp auth object from a FlowScope profile dict."""
    version = profile.get("version")
    if version == "2c":
        community = profile.get("community") or "public"
        # mpModel=1 selects SNMPv2c; mpModel=0 would be v1.
        return CommunityData(community, mpModel=1)
    if version == "3":
        username = profile.get("v3_username")
        if not username:
            raise ValueError("v3 profile is missing v3_username")
        sec_level = profile.get("v3_security_level") or "noAuthNoPriv"
        auth_proto = _AUTH_PROTOS.get(profile.get("v3_auth_proto"), usmNoAuthProtocol)
        priv_proto = _PRIV_PROTOS.get(profile.get("v3_priv_proto"), usmNoPrivProtocol)
        # pysnmp distinguishes "no auth" from "auth, key=None" by which
        # protocol is set; passing None for the key while specifying the
        # auth protocol is an error. Mirror that here.
        if sec_level == "noAuthNoPriv":
            return UsmUserData(username)
        if sec_level == "authNoPriv":
            return UsmUserData(
                userName=username,
                authKey=profile.get("v3_auth_pass"),
                authProtocol=auth_proto,
            )
        if sec_level == "authPriv":
            return UsmUserData(
                userName=username,
                authKey=profile.get("v3_auth_pass"),
                privKey=profile.get("v3_priv_pass"),
                authProtocol=auth_proto,
                privProtocol=priv_proto,
            )
        raise ValueError(f"unknown v3_security_level: {sec_level}")
    raise ValueError(f"unsupported snmp version: {version}")


async def _walk_iftable_async(profile, host, want):
    """Walk the IF-MIB columns we care about and merge by ifindex.
    Filters to the `want` set so we only return interfaces the caller
    asked about."""
    auth = _auth_for(profile)
    port = int(profile.get("port") or 161)
    timeout = float(profile.get("timeout_s") or 2.0)
    retries = int(profile.get("retries") or 1)
    target = await UdpTransportTarget.create(
        (host, port),
        timeout=timeout,
        retries=retries,
    )
    context = ContextData(contextName=profile.get("v3_context") or "")
    engine = SnmpEngine()

    descr        = await _walk_one(engine, auth, target, context, OID_IFDESCR,       want)
    alias        = await _walk_one(engine, auth, target, context, OID_IFALIAS,       want)
    speed        = await _walk_one(engine, auth, target, context, OID_IFHIGHSPEED,   want)
    mtu          = await _walk_one(engine, auth, target, context, OID_IFMTU,         want)
    phys         = await _walk_one(engine, auth, target, context, OID_IFPHYSADDR,    want, raw=True)
    admin_status = await _walk_one(engine, auth, target, context, OID_IFADMINSTATUS, want)
    oper_status  = await _walk_one(engine, auth, target, context, OID_IFOPERSTATUS,  want)
    in_errors    = await _walk_one(engine, auth, target, context, OID_IFINERRORS,    want)
    out_errors   = await _walk_one(engine, auth, target, context, OID_IFOUTERRORS,   want)
    in_discards  = await _walk_one(engine, auth, target, context, OID_IFINDISCARDS,  want)
    out_discards = await _walk_one(engine, auth, target, context, OID_IFOUTDISCARDS, want)

    out = {}
    for ifindex in want:
        name = descr.get(ifindex)
        if name is None:
            # Device didn't return an ifDescr for this index — skip it
            # rather than synthesize a placeholder.
            continue
        speed_mb = speed.get(ifindex)
        try:
            speed_bps = int(speed_mb) * 1_000_000 if speed_mb else None
        except (TypeError, ValueError):
            speed_bps = None
        out[ifindex] = {
            "name":         name.strip(),
            "alias":        (alias.get(ifindex) or "").strip(),
            "speed_bps":    speed_bps,
            "admin_status": _maybe_int(admin_status.get(ifindex)),
            "oper_status":  _maybe_int(oper_status.get(ifindex)),
            "in_errors":    _maybe_int(in_errors.get(ifindex)),
            "out_errors":   _maybe_int(out_errors.get(ifindex)),
            "in_discards":  _maybe_int(in_discards.get(ifindex)),
            "out_discards": _maybe_int(out_discards.get(ifindex)),
            "mtu":          _maybe_int(mtu.get(ifindex)),
            "mac":          _format_mac(phys.get(ifindex)),
        }
    return out


def _maybe_int(v):
    if v is None or v == "":
        return None
    try:
        return int(v)
    except (TypeError, ValueError):
        return None


def _format_mac(raw):
    """Format a 6-byte ifPhysAddress as 'aa:bb:cc:dd:ee:ff'.
    Empty / non-6-byte values return None — many logical interfaces
    (loopback, tunnels, SVIs) report a zero-length physaddr."""
    if not raw:
        return None
    if isinstance(raw, str):
        # pysnmp returns OctetString — its str() is the printable form,
        # which for a MAC is garbage. We work with the raw bytes via
        # asOctets() in _walk_one (raw=True), so this branch shouldn't
        # be hit, but keep it as a safe fallback.
        try:
            raw = raw.encode("latin-1")
        except Exception:
            return None
    if len(raw) != 6:
        return None
    return ":".join(f"{b:02x}" for b in raw)


async def _walk_one(engine, auth, target, context, oid_root, want, raw=False):
    """Walk a single column OID, return {ifindex: value} for entries
    whose ifindex is in `want`. With raw=True, return raw bytes (used
    for ifPhysAddress); otherwise stringify via prettyPrint(). Stops
    walking once the OID prefix changes (lexicographicMode=False)."""
    out = {}
    iterator = walk_cmd(
        engine, auth, target, context,
        ObjectType(ObjectIdentity(oid_root)),
        lexicographicMode=False,
    )
    async for errInd, errStat, errIdx, varBinds in iterator:
        if errInd:
            raise RuntimeError(f"snmp transport: {errInd}")
        if errStat:
            raise RuntimeError(f"snmp pdu error: {errStat.prettyPrint()} "
                               f"at index {errIdx}")
        for oid, val in varBinds:
            tail = oid.prettyPrint().rsplit(".", 1)[-1]
            try:
                ifindex = int(tail)
            except ValueError:
                continue
            if ifindex in want:
                if raw:
                    # OctetString.asOctets() returns bytes
                    try:
                        out[ifindex] = bytes(val.asOctets())
                    except Exception:
                        out[ifindex] = None
                else:
                    out[ifindex] = str(val)
    return out
