#!/usr/bin/env python3
"""
FlowScope — NetFlow/sFlow collector and visualization server.

Runs three things concurrently:
  1. UDP listener for NetFlow v5 / v9 / IPFIX (default port 2055)
  2. UDP listener for sFlow v5 (default port 6343)
  3. Flask web server with REST API + dashboard (default port 8080)

All flow records go into an in-memory ring buffer plus rolling per-interface
counters. SQLite is used for longer-term persistence (last N hours).
"""

import argparse
import hmac
import json
import os
import socket
import sqlite3
import struct
import threading
import time
from collections import defaultdict, deque
from datetime import datetime, timezone
from ipaddress import ip_address

from flask import Flask, jsonify, request, send_from_directory, make_response

import snmp_mock

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

NETFLOW_PORT = int(os.environ.get("FLOWSCOPE_NETFLOW_PORT", 2055))
SFLOW_PORT   = int(os.environ.get("FLOWSCOPE_SFLOW_PORT",   6343))
WEB_PORT     = int(os.environ.get("FLOWSCOPE_WEB_PORT",     8080))
WEB_HOST     = os.environ.get("FLOWSCOPE_WEB_HOST", "0.0.0.0")
DB_PATH      = os.environ.get("FLOWSCOPE_DB_PATH", "flowscope.db")

# Optional bearer-token auth for the /api/* surface.
# Unset (empty) → no auth, current behavior preserved.
# Set         → every /api/* request must carry  X-Auth-Token: <value>.
# The static dashboard is NOT gated; the browser fetches it, gets 401 on the
# first API call, prompts for the token, stores it in sessionStorage, retries.
AUTH_TOKEN = os.environ.get("FLOWSCOPE_AUTH_TOKEN", "")

# How many recent flow records to keep in memory for the live feed
RING_SIZE = 5000

# ---------------------------------------------------------------------------
# In-memory state (shared across threads with a lock)
# ---------------------------------------------------------------------------

state_lock = threading.Lock()

# Recent flow records, newest last
recent_flows = deque(maxlen=RING_SIZE)

# Per-device-and-interface rolling counters.
# Key = (exporter_ip, ifindex). Value = dict with counters.
interface_stats = defaultdict(lambda: {
    "ingress_bytes":   0,
    "ingress_packets": 0,
    "egress_bytes":    0,
    "egress_packets":  0,
    "last_seen":       0,
    "name":            None,    # filled if SNMP/sFlow tells us
    "alias":           None,    # filled by SNMP poller (ifAlias)
    "speed_bps":       None,
})

# Devices we've heard from
devices = {}  # exporter_ip -> {first_seen, last_seen, flow_count, type, hostname}

# User-defined display labels for exporters and interfaces, persisted in SQLite.
# Loaded at startup, mutated through the label PUT endpoints.
exporter_labels  = {}  # exporter_ip -> str
interface_labels = {}  # (exporter_ip, ifindex) -> str

# NetFlow v9 / IPFIX templates, keyed by (exporter, source_id, template_id)
templates = {}

# Counters
stats = {
    "netflow_packets": 0,
    "sflow_packets":   0,
    "flows_recorded":  0,
    "started":         time.time(),
}


# ---------------------------------------------------------------------------
# SQLite persistence (best-effort, append-only)
# ---------------------------------------------------------------------------

def db_connect():
    conn = sqlite3.connect(DB_PATH, check_same_thread=False, timeout=5)
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA synchronous=NORMAL")
    return conn


def db_init():
    conn = db_connect()
    conn.execute("""
        CREATE TABLE IF NOT EXISTS flows (
            id          INTEGER PRIMARY KEY AUTOINCREMENT,
            ts          REAL NOT NULL,
            exporter    TEXT NOT NULL,
            proto_name  TEXT NOT NULL,   -- 'netflow' or 'sflow'
            src_ip      TEXT,
            dst_ip      TEXT,
            src_port    INTEGER,
            dst_port    INTEGER,
            protocol    INTEGER,
            input_if    INTEGER,
            output_if   INTEGER,
            bytes       INTEGER,
            packets     INTEGER,
            tcp_flags   INTEGER
        )
    """)
    conn.execute("CREATE INDEX IF NOT EXISTS idx_flows_ts ON flows(ts)")
    conn.execute("CREATE INDEX IF NOT EXISTS idx_flows_exp ON flows(exporter)")
    conn.execute("""
        CREATE TABLE IF NOT EXISTS exporter_labels (
            exporter TEXT PRIMARY KEY,
            label    TEXT NOT NULL
        )
    """)
    conn.execute("""
        CREATE TABLE IF NOT EXISTS interface_labels (
            exporter TEXT NOT NULL,
            ifindex  INTEGER NOT NULL,
            label    TEXT NOT NULL,
            PRIMARY KEY (exporter, ifindex)
        )
    """)
    for row in conn.execute("SELECT exporter, label FROM exporter_labels"):
        exporter_labels[row[0]] = row[1]
    for row in conn.execute("SELECT exporter, ifindex, label FROM interface_labels"):
        interface_labels[(row[0], row[1])] = row[2]
    conn.commit()
    return conn


db_conn = db_init()
db_lock = threading.Lock()


def db_insert_flow(rec):
    try:
        with db_lock:
            db_conn.execute("""
                INSERT INTO flows (ts, exporter, proto_name, src_ip, dst_ip,
                    src_port, dst_port, protocol, input_if, output_if,
                    bytes, packets, tcp_flags)
                VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
            """, (
                rec["ts"], rec["exporter"], rec["proto_name"],
                rec.get("src_ip"), rec.get("dst_ip"),
                rec.get("src_port"), rec.get("dst_port"),
                rec.get("protocol"), rec.get("input_if"),
                rec.get("output_if"), rec.get("bytes"),
                rec.get("packets"), rec.get("tcp_flags"),
            ))
            db_conn.commit()
    except Exception as e:
        # don't let DB errors crash the collector
        print(f"[db] insert error: {e}")


def db_prune_loop():
    """Keep ~6 hours of flow history in SQLite."""
    while True:
        try:
            cutoff = time.time() - 6 * 3600
            with db_lock:
                db_conn.execute("DELETE FROM flows WHERE ts < ?", (cutoff,))
                db_conn.commit()
        except Exception as e:
            print(f"[db] prune error: {e}")
        time.sleep(300)


def rdns_loop():
    """Best-effort reverse DNS for exporter IPs.

    Runs in its own daemon thread so the (potentially blocking) gethostbyaddr
    calls never delay the UDP parser threads. Each device is resolved once;
    a failed lookup is recorded as an empty string so we don't keep retrying.
    """
    while True:
        try:
            with state_lock:
                need = [ip for ip, d in devices.items() if "hostname" not in d]
            for ip in need:
                hn = ""
                try:
                    hn = socket.gethostbyaddr(ip)[0]
                except Exception:
                    pass
                with state_lock:
                    if ip in devices:
                        devices[ip]["hostname"] = hn
        except Exception as e:
            print(f"[rdns] error: {e}")
        time.sleep(30)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def proto_name(num):
    return {1: "ICMP", 6: "TCP", 17: "UDP", 47: "GRE", 50: "ESP",
            51: "AH", 58: "ICMPv6", 89: "OSPF", 132: "SCTP"}.get(num, str(num))


def record_flow(rec):
    """Record one flow (NetFlow record or sFlow sampled flow) into all stores."""
    rec["ts"] = rec.get("ts", time.time())
    exporter = rec["exporter"]

    with state_lock:
        recent_flows.append(rec)
        stats["flows_recorded"] += 1

        # Update device tracking. Note: "hostname" key is intentionally absent
        # on first sight — its absence is the signal rdns_loop uses to know it
        # still needs to resolve this exporter.
        d = devices.setdefault(exporter, {
            "first_seen": rec["ts"],
            "last_seen":  rec["ts"],
            "flow_count": 0,
            "type":       rec["proto_name"],
        })
        d["last_seen"] = rec["ts"]
        d["flow_count"] += 1

        # Per-interface counters: input is ingress, output is egress
        b = rec.get("bytes", 0) or 0
        p = rec.get("packets", 0) or 0
        if rec.get("input_if") is not None:
            k = (exporter, rec["input_if"])
            interface_stats[k]["ingress_bytes"]   += b
            interface_stats[k]["ingress_packets"] += p
            interface_stats[k]["last_seen"]       = rec["ts"]
        if rec.get("output_if") is not None:
            k = (exporter, rec["output_if"])
            interface_stats[k]["egress_bytes"]    += b
            interface_stats[k]["egress_packets"]  += p
            interface_stats[k]["last_seen"]       = rec["ts"]

    db_insert_flow(rec)


# ===========================================================================
# NetFlow v5 parser
# ===========================================================================
# Header: 24 bytes
#   uint16 version, uint16 count, uint32 sys_uptime, uint32 unix_secs,
#   uint32 unix_nsecs, uint32 flow_sequence, uint8 engine_type,
#   uint8 engine_id, uint16 sampling_interval
# Record: 48 bytes per flow
NF5_HDR  = struct.Struct("!HHIIIIBBH")
NF5_REC  = struct.Struct("!IIIHHIIIIHHBBBBHHBBH")


def parse_netflow_v5(data, exporter):
    if len(data) < NF5_HDR.size:
        return
    version, count, _uptime, secs, _nsecs, _seq, _et, _eid, _samp = \
        NF5_HDR.unpack_from(data, 0)
    off = NF5_HDR.size
    for _ in range(count):
        if off + NF5_REC.size > len(data):
            break
        (srcaddr, dstaddr, _nexthop, input_if, output_if, dPkts, dOctets,
         _first, _last, srcport, dstport, _pad, tcp_flags, prot, _tos,
         _src_as, _dst_as, _src_mask, _dst_mask, _pad2) = NF5_REC.unpack_from(data, off)
        off += NF5_REC.size
        rec = {
            "ts":         secs,
            "exporter":   exporter,
            "proto_name": "netflow",
            "src_ip":     str(ip_address(srcaddr)),
            "dst_ip":     str(ip_address(dstaddr)),
            "src_port":   srcport,
            "dst_port":   dstport,
            "protocol":   prot,
            "input_if":   input_if,
            "output_if":  output_if,
            "bytes":      dOctets,
            "packets":    dPkts,
            "tcp_flags":  tcp_flags,
        }
        record_flow(rec)


# ===========================================================================
# NetFlow v9 / IPFIX parser (templates + data)
# ===========================================================================
# v9 header:
#   uint16 version, uint16 count, uint32 uptime, uint32 unix_secs,
#   uint32 sequence, uint32 source_id
NF9_HDR = struct.Struct("!HHIIII")

# Field IDs we care about (NetFlow v9 / IPFIX use the same numbers for these)
NF9_FIELDS = {
    1:   ("bytes",     None),   # IN_BYTES
    2:   ("packets",   None),   # IN_PKTS
    4:   ("protocol",  1),      # PROTOCOL
    6:   ("tcp_flags", 1),      # TCP_FLAGS
    7:   ("src_port",  2),      # L4_SRC_PORT
    8:   ("src_ip",    "ipv4"), # IPV4_SRC_ADDR
    10:  ("input_if",  None),   # INPUT_SNMP
    11:  ("dst_port",  2),      # L4_DST_PORT
    12:  ("dst_ip",    "ipv4"), # IPV4_DST_ADDR
    14:  ("output_if", None),   # OUTPUT_SNMP
    27:  ("src_ip",    "ipv6"), # IPV6_SRC_ADDR
    28:  ("dst_ip",    "ipv6"), # IPV6_DST_ADDR
}


def _read_int(buf, off, length):
    if length <= 0 or off + length > len(buf):
        return 0
    return int.from_bytes(buf[off:off+length], "big")


def parse_netflow_v9(data, exporter):
    if len(data) < NF9_HDR.size:
        return
    _version, count, _uptime, secs, _seq, source_id = NF9_HDR.unpack_from(data, 0)
    off = NF9_HDR.size
    flowsets_seen = 0
    while off + 4 <= len(data) and flowsets_seen < count:
        fs_id, fs_len = struct.unpack_from("!HH", data, off)
        if fs_len < 4 or off + fs_len > len(data):
            break
        body = data[off+4:off+fs_len]
        off += fs_len

        if fs_id == 0:
            # Template flowset
            i = 0
            while i + 4 <= len(body):
                tid, fcount = struct.unpack_from("!HH", body, i)
                i += 4
                fields = []
                for _ in range(fcount):
                    if i + 4 > len(body):
                        break
                    ftype, flen = struct.unpack_from("!HH", body, i)
                    i += 4
                    fields.append((ftype, flen))
                templates[(exporter, source_id, tid)] = fields
        elif fs_id == 1:
            # Options template — skip for now, not needed for traffic stats
            pass
        else:
            # Data flowset; fs_id is the template id
            tmpl = templates.get((exporter, source_id, fs_id))
            if not tmpl:
                continue
            rec_size = sum(flen for _, flen in tmpl)
            if rec_size == 0:
                continue
            for r in range(0, len(body) - rec_size + 1, rec_size):
                rec = {
                    "ts":         secs,
                    "exporter":   exporter,
                    "proto_name": "netflow",
                }
                p = r
                for ftype, flen in tmpl:
                    spec = NF9_FIELDS.get(ftype)
                    if spec:
                        name, kind = spec
                        if kind == "ipv4" and flen == 4:
                            rec[name] = str(ip_address(body[p:p+4]))
                        elif kind == "ipv6" and flen == 16:
                            rec[name] = str(ip_address(body[p:p+16]))
                        else:
                            rec[name] = _read_int(body, p, flen)
                    p += flen
                record_flow(rec)
        flowsets_seen += 1


# ===========================================================================
# sFlow v5 parser
# ===========================================================================
# sFlow datagram header:
#   uint32 version (=5)
#   uint32 ip_version (1=v4, 2=v6)
#   uint32[1 or 4] agent_address
#   uint32 sub_agent_id
#   uint32 sequence_number
#   uint32 sysuptime_ms
#   uint32 num_samples
# Then `num_samples` samples, each with a tag, length, and body.

def parse_sflow_v5(data, exporter):
    if len(data) < 28:
        return
    off = 0
    version = struct.unpack_from("!I", data, off)[0]; off += 4
    if version != 5:
        return
    ip_ver  = struct.unpack_from("!I", data, off)[0]; off += 4
    if ip_ver == 1:
        agent = ".".join(str(b) for b in data[off:off+4]); off += 4
    elif ip_ver == 2:
        agent = str(ip_address(data[off:off+16])); off += 16
    else:
        return
    _sub_agent = struct.unpack_from("!I", data, off)[0]; off += 4
    _seq       = struct.unpack_from("!I", data, off)[0]; off += 4
    _uptime    = struct.unpack_from("!I", data, off)[0]; off += 4
    num_samples = struct.unpack_from("!I", data, off)[0]; off += 4

    # Use the agent address as the canonical exporter ID, falling back to
    # the UDP source IP if agent is 0.0.0.0
    if agent and agent != "0.0.0.0":
        exporter = agent

    for _ in range(num_samples):
        if off + 8 > len(data):
            return
        sample_tag = struct.unpack_from("!I", data, off)[0]; off += 4
        sample_len = struct.unpack_from("!I", data, off)[0]; off += 4
        if off + sample_len > len(data):
            return
        sample_body = data[off:off+sample_len]
        off += sample_len

        # Common formats:
        #   1 = flow_sample,            3 = flow_sample_expanded
        #   2 = counters_sample,        4 = counters_sample_expanded
        fmt = sample_tag & 0xFFF
        if fmt == 1:
            parse_sflow_flow_sample(sample_body, exporter, expanded=False)
        elif fmt == 3:
            parse_sflow_flow_sample(sample_body, exporter, expanded=True)
        elif fmt == 2:
            parse_sflow_counters_sample(sample_body, exporter, expanded=False)
        elif fmt == 4:
            parse_sflow_counters_sample(sample_body, exporter, expanded=True)


def parse_sflow_flow_sample(body, exporter, expanded=False):
    """Parse a flow_sample. We extract input/output ifindex and the sampled
    raw packet header to get src/dst IP and ports."""
    o = 0
    try:
        _seq  = struct.unpack_from("!I", body, o)[0]; o += 4
        if expanded:
            _src_type  = struct.unpack_from("!I", body, o)[0]; o += 4
            _src_index = struct.unpack_from("!I", body, o)[0]; o += 4
        else:
            o += 4   # source_id (type<<24 | index)
        _samp_rate = struct.unpack_from("!I", body, o)[0]; o += 4
        _samp_pool = struct.unpack_from("!I", body, o)[0]; o += 4
        _drops     = struct.unpack_from("!I", body, o)[0]; o += 4
        if expanded:
            _in_fmt  = struct.unpack_from("!I", body, o)[0]; o += 4
            input_if = struct.unpack_from("!I", body, o)[0]; o += 4
            _out_fmt = struct.unpack_from("!I", body, o)[0]; o += 4
            output_if = struct.unpack_from("!I", body, o)[0]; o += 4
        else:
            input_if  = struct.unpack_from("!I", body, o)[0] & 0x3FFFFFFF; o += 4
            output_if = struct.unpack_from("!I", body, o)[0] & 0x3FFFFFFF; o += 4
        num_records = struct.unpack_from("!I", body, o)[0]; o += 4
    except struct.error:
        return

    rec_template = {
        "ts":         time.time(),
        "exporter":   exporter,
        "proto_name": "sflow",
        "input_if":   input_if,
        "output_if":  output_if,
        "bytes":      0,
        "packets":    1,
    }

    for _ in range(num_records):
        if o + 8 > len(body):
            break
        rec_tag = struct.unpack_from("!I", body, o)[0]; o += 4
        rec_len = struct.unpack_from("!I", body, o)[0]; o += 4
        if o + rec_len > len(body):
            break
        rec_body = body[o:o+rec_len]
        o += rec_len

        # rec_tag 1 = sampled raw header (Ethernet)
        if (rec_tag & 0xFFF) == 1:
            parse_sflow_raw_header(rec_body, rec_template)

    record_flow(rec_template)


def parse_sflow_raw_header(body, rec):
    """rec gets src_ip/dst_ip/ports/protocol/bytes filled in if we can decode."""
    if len(body) < 16:
        return
    try:
        _proto    = struct.unpack_from("!I", body, 0)[0]
        frame_len = struct.unpack_from("!I", body, 4)[0]
        _stripped = struct.unpack_from("!I", body, 8)[0]
        hdr_len   = struct.unpack_from("!I", body, 12)[0]
    except struct.error:
        return
    rec["bytes"] = frame_len
    pkt = body[16:16+hdr_len]
    if len(pkt) < 14:
        return
    # Ethernet
    eth_type = int.from_bytes(pkt[12:14], "big")
    p = 14
    # Skip VLAN tags (0x8100, 0x88A8)
    while eth_type in (0x8100, 0x88A8) and p + 4 <= len(pkt):
        eth_type = int.from_bytes(pkt[p+2:p+4], "big")
        p += 4
    if eth_type == 0x0800 and len(pkt) >= p + 20:
        # IPv4
        ihl = (pkt[p] & 0x0F) * 4
        if ihl < 20 or len(pkt) < p + ihl:
            return
        rec["protocol"] = pkt[p+9]
        rec["src_ip"]   = ".".join(str(b) for b in pkt[p+12:p+16])
        rec["dst_ip"]   = ".".join(str(b) for b in pkt[p+16:p+20])
        l4 = p + ihl
        if rec["protocol"] in (6, 17) and len(pkt) >= l4 + 4:
            rec["src_port"] = int.from_bytes(pkt[l4:l4+2],   "big")
            rec["dst_port"] = int.from_bytes(pkt[l4+2:l4+4], "big")
    elif eth_type == 0x86DD and len(pkt) >= p + 40:
        # IPv6
        rec["protocol"] = pkt[p+6]
        rec["src_ip"]   = str(ip_address(pkt[p+8:p+24]))
        rec["dst_ip"]   = str(ip_address(pkt[p+24:p+40]))
        if rec["protocol"] in (6, 17) and len(pkt) >= p + 40 + 4:
            rec["src_port"] = int.from_bytes(pkt[p+40:p+42], "big")
            rec["dst_port"] = int.from_bytes(pkt[p+42:p+44], "big")


def parse_sflow_counters_sample(body, exporter, expanded=False):
    """Counters samples carry per-interface octet/packet totals.
    We update interface_stats with the absolute counters and the interface name."""
    o = 0
    try:
        _seq = struct.unpack_from("!I", body, o)[0]; o += 4
        if expanded:
            _src_type  = struct.unpack_from("!I", body, o)[0]; o += 4
            _src_index = struct.unpack_from("!I", body, o)[0]; o += 4
        else:
            o += 4
        num_records = struct.unpack_from("!I", body, o)[0]; o += 4
    except struct.error:
        return

    for _ in range(num_records):
        if o + 8 > len(body):
            return
        rec_tag = struct.unpack_from("!I", body, o)[0]; o += 4
        rec_len = struct.unpack_from("!I", body, o)[0]; o += 4
        if o + rec_len > len(body):
            return
        rec_body = body[o:o+rec_len]
        o += rec_len

        # tag 1 = generic interface counters (sFlow if_counters, IF-MIB-like)
        # XDR layout (88 bytes):
        #   ifIndex u32, ifType u32, ifSpeed u64, ifDirection u32, ifStatus u32,
        #   ifInOctets u64, ifInUcastPkts u32, ifInMulticastPkts u32,
        #   ifInBroadcastPkts u32, ifInDiscards u32, ifInErrors u32,
        #   ifInUnknownProtos u32, ifOutOctets u64, ifOutUcastPkts u32,
        #   ifOutMulticastPkts u32, ifOutBroadcastPkts u32, ifOutDiscards u32,
        #   ifOutErrors u32, ifPromiscuousMode u32
        if (rec_tag & 0xFFF) == 1 and len(rec_body) >= 88:
            try:
                ifindex   = int.from_bytes(rec_body[0:4],   "big")
                ifspeed   = int.from_bytes(rec_body[8:16],  "big")
                in_oct    = int.from_bytes(rec_body[24:32], "big")
                in_ucast  = int.from_bytes(rec_body[32:36], "big")
                out_oct   = int.from_bytes(rec_body[56:64], "big")
                out_ucast = int.from_bytes(rec_body[64:68], "big")
            except Exception:
                continue

            with state_lock:
                k = (exporter, ifindex)
                s = interface_stats[k]
                # For counter samples, replace running totals with the absolutes
                # reported by the device (these are authoritative).
                s["ingress_bytes"]   = in_oct
                s["ingress_packets"] = in_ucast
                s["egress_bytes"]    = out_oct
                s["egress_packets"]  = out_ucast
                s["speed_bps"]       = ifspeed if ifspeed else s["speed_bps"]
                s["last_seen"]       = time.time()


# ===========================================================================
# UDP listeners
# ===========================================================================

def netflow_listener():
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind(("0.0.0.0", NETFLOW_PORT))
    print(f"[netflow] listening on UDP/{NETFLOW_PORT}")
    while True:
        try:
            data, addr = sock.recvfrom(65535)
            stats["netflow_packets"] += 1
            if len(data) < 4:
                continue
            version = struct.unpack_from("!H", data, 0)[0]
            if version == 5:
                parse_netflow_v5(data, addr[0])
            elif version in (9, 10):  # 9 = NetFlow v9, 10 = IPFIX (same on the wire for our subset)
                parse_netflow_v9(data, addr[0])
        except Exception as e:
            print(f"[netflow] parse error from {addr}: {e}")


def sflow_listener():
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind(("0.0.0.0", SFLOW_PORT))
    print(f"[sflow]  listening on UDP/{SFLOW_PORT}")
    while True:
        try:
            data, addr = sock.recvfrom(65535)
            stats["sflow_packets"] += 1
            parse_sflow_v5(data, addr[0])
        except Exception as e:
            print(f"[sflow] parse error from {addr}: {e}")


# ===========================================================================
# Web / API
# ===========================================================================

app = Flask(__name__, static_folder="web", static_url_path="")
# Don't let browsers cache the dashboard. Flask defaults to a 12h max-age,
# which made every UI change require a hard refresh in the field.
app.config["SEND_FILE_MAX_AGE_DEFAULT"] = 0


@app.before_request
def _require_auth():
    """Gate /api/* behind FLOWSCOPE_AUTH_TOKEN when set.

    Static assets (the dashboard shell) stay public so the browser can load
    its login UI before it has a token. CORS preflights (OPTIONS) pass
    through; we don't expose a CORS surface anyway, but it costs nothing
    to be polite to browsers."""
    if not AUTH_TOKEN:
        return None
    if request.method == "OPTIONS":
        return None
    if not request.path.startswith("/api/"):
        return None
    sent = request.headers.get("X-Auth-Token", "")
    if not sent or not hmac.compare_digest(sent, AUTH_TOKEN):
        return jsonify({"ok": False, "error": "unauthorized"}), 401
    return None


@app.route("/api/auth/check")
def api_auth_check():
    """Lightweight endpoint the frontend hits to validate a token without
    triggering side effects. Returns 200 if the token is good (or auth is
    disabled), 401 otherwise — handled by _require_auth above."""
    return jsonify({"ok": True, "auth_required": bool(AUTH_TOKEN)})


@app.route("/")
def index():
    # Belt-and-suspenders no-cache. Flask's SEND_FILE_MAX_AGE_DEFAULT=0
    # (set above) just means max-age=0; this also prevents disk caching
    # and forces revalidation, so a deploy is reflected on the next load
    # without users having to remember to hard-refresh.
    resp = make_response(send_from_directory("web", "index.html"))
    resp.headers["Cache-Control"] = "no-store, no-cache, must-revalidate, max-age=0"
    resp.headers["Pragma"]  = "no-cache"
    resp.headers["Expires"] = "0"
    return resp


@app.route("/api/summary")
def api_summary():
    with state_lock:
        return jsonify({
            "uptime_seconds":  int(time.time() - stats["started"]),
            "netflow_packets": stats["netflow_packets"],
            "sflow_packets":   stats["sflow_packets"],
            "flows_recorded":  stats["flows_recorded"],
            "device_count":    len(devices),
            "interface_count": len(interface_stats),
            "ports": {
                "netflow": NETFLOW_PORT,
                "sflow":   SFLOW_PORT,
            },
        })


@app.route("/api/devices")
def api_devices():
    with state_lock:
        out = []
        for ip, d in devices.items():
            out.append({
                "exporter":   ip,
                "label":      exporter_labels.get(ip, ""),
                "hostname":   d.get("hostname", ""),
                "first_seen": d["first_seen"],
                "last_seen":  d["last_seen"],
                "flow_count": d["flow_count"],
                "type":       d["type"],
                "interface_count": sum(1 for (e, _) in interface_stats if e == ip),
            })
    out.sort(key=lambda x: x["last_seen"], reverse=True)
    return jsonify(out)


@app.route("/api/interfaces")
def api_interfaces():
    """Return per-interface ingress/egress totals.
    Optional query: ?exporter=10.0.0.1"""
    exporter_filter = request.args.get("exporter")
    with state_lock:
        rows = []
        for (exp, ifidx), s in interface_stats.items():
            if exporter_filter and exp != exporter_filter:
                continue
            rows.append({
                "exporter":        exp,
                "ifindex":         ifidx,
                "name":            s["name"] or f"if{ifidx}",
                "alias":           s.get("alias") or "",
                "label":           interface_labels.get((exp, ifidx), ""),
                "speed_bps":       s["speed_bps"],
                "ingress_bytes":   s["ingress_bytes"],
                "ingress_packets": s["ingress_packets"],
                "egress_bytes":    s["egress_bytes"],
                "egress_packets":  s["egress_packets"],
                "last_seen":       s["last_seen"],
            })
    rows.sort(key=lambda r: (r["exporter"], r["ifindex"]))
    return jsonify(rows)


@app.route("/api/exporters/<exporter>/label", methods=["PUT", "POST", "DELETE"])
def api_set_exporter_label(exporter):
    """Set or clear the user-defined display label for an exporter.
    Body: {"label": "..."}; empty/missing label or DELETE clears it."""
    payload = request.get_json(silent=True) or {}
    label = "" if request.method == "DELETE" else (payload.get("label") or "").strip()
    try:
        with db_lock:
            if label:
                db_conn.execute(
                    "INSERT OR REPLACE INTO exporter_labels(exporter, label) VALUES(?, ?)",
                    (exporter, label))
            else:
                db_conn.execute(
                    "DELETE FROM exporter_labels WHERE exporter = ?", (exporter,))
            db_conn.commit()
    except Exception as e:
        return jsonify({"ok": False, "error": str(e)}), 500
    with state_lock:
        if label:
            exporter_labels[exporter] = label
        else:
            exporter_labels.pop(exporter, None)
    return jsonify({"ok": True, "exporter": exporter, "label": label})


@app.route("/api/interfaces/<exporter>/<int:ifindex>/label", methods=["PUT", "POST", "DELETE"])
def api_set_interface_label(exporter, ifindex):
    """Set or clear the user-defined display label for an interface."""
    payload = request.get_json(silent=True) or {}
    label = "" if request.method == "DELETE" else (payload.get("label") or "").strip()
    try:
        with db_lock:
            if label:
                db_conn.execute(
                    "INSERT OR REPLACE INTO interface_labels(exporter, ifindex, label) VALUES(?, ?, ?)",
                    (exporter, ifindex, label))
            else:
                db_conn.execute(
                    "DELETE FROM interface_labels WHERE exporter = ? AND ifindex = ?",
                    (exporter, ifindex))
            db_conn.commit()
    except Exception as e:
        return jsonify({"ok": False, "error": str(e)}), 500
    with state_lock:
        if label:
            interface_labels[(exporter, ifindex)] = label
        else:
            interface_labels.pop((exporter, ifindex), None)
    return jsonify({"ok": True, "exporter": exporter, "ifindex": ifindex, "label": label})


@app.route("/api/flows/recent")
def api_recent_flows():
    """Return the most recent flows. Optional ?limit=200&exporter=...&ifindex=..."""
    limit = min(int(request.args.get("limit", 200)), RING_SIZE)
    exporter_filter = request.args.get("exporter")
    ifindex_filter  = request.args.get("ifindex")
    if ifindex_filter is not None:
        ifindex_filter = int(ifindex_filter)
    with state_lock:
        flows = list(recent_flows)
    flows.reverse()
    out = []
    for f in flows:
        if exporter_filter and f.get("exporter") != exporter_filter:
            continue
        if ifindex_filter is not None and ifindex_filter not in (
                f.get("input_if"), f.get("output_if")):
            continue
        out.append({
            **f,
            "protocol_name": proto_name(f.get("protocol", 0)),
        })
        if len(out) >= limit:
            break
    return jsonify(out)


@app.route("/api/top/talkers")
def api_top_talkers():
    """Top src/dst IP pairs by bytes over the recent buffer."""
    limit = int(request.args.get("limit", 20))
    exporter_filter = request.args.get("exporter")
    agg = defaultdict(lambda: {"bytes": 0, "packets": 0, "flows": 0})
    with state_lock:
        flows = list(recent_flows)
    for f in flows:
        if exporter_filter and f.get("exporter") != exporter_filter:
            continue
        key = (f.get("src_ip"), f.get("dst_ip"))
        if not all(key):
            continue
        agg[key]["bytes"]   += f.get("bytes", 0) or 0
        agg[key]["packets"] += f.get("packets", 0) or 0
        agg[key]["flows"]   += 1
    rows = [{"src_ip": k[0], "dst_ip": k[1], **v} for k, v in agg.items()]
    rows.sort(key=lambda r: r["bytes"], reverse=True)
    return jsonify(rows[:limit])


@app.route("/api/top/ports")
def api_top_ports():
    limit = int(request.args.get("limit", 15))
    exporter_filter = request.args.get("exporter")
    agg = defaultdict(lambda: {"bytes": 0, "flows": 0, "protocol": None})
    with state_lock:
        flows = list(recent_flows)
    for f in flows:
        if exporter_filter and f.get("exporter") != exporter_filter:
            continue
        port = f.get("dst_port")
        if port is None:
            continue
        agg[port]["bytes"] += f.get("bytes", 0) or 0
        agg[port]["flows"] += 1
        agg[port]["protocol"] = proto_name(f.get("protocol", 0))
    rows = [{"port": k, **v} for k, v in agg.items()]
    rows.sort(key=lambda r: r["bytes"], reverse=True)
    return jsonify(rows[:limit])


@app.route("/api/protocols")
def api_protocols():
    exporter_filter = request.args.get("exporter")
    agg = defaultdict(lambda: {"bytes": 0, "flows": 0})
    with state_lock:
        flows = list(recent_flows)
    for f in flows:
        if exporter_filter and f.get("exporter") != exporter_filter:
            continue
        name = proto_name(f.get("protocol", 0))
        agg[name]["bytes"] += f.get("bytes", 0) or 0
        agg[name]["flows"] += 1
    rows = [{"protocol": k, **v} for k, v in agg.items()]
    rows.sort(key=lambda r: r["bytes"], reverse=True)
    return jsonify(rows)


@app.route("/api/snmp/status")
def api_snmp_status():
    """Report SNMP poller state. Currently always reports mock=true; the
    field exists so the frontend can warn that names/speeds are synthetic."""
    return jsonify(snmp_mock.poller_status())


@app.route("/api/timeseries")
def api_timeseries():
    """Bucketed bytes/sec for the last N seconds.
    Optional ?seconds=300 ?exporter=... ?ifindex=...

    Short windows (<= 300s) read from the in-memory ring; longer windows
    read from SQLite, which holds the full ~6h of pruned history. The
    response is downsampled to ~600 buckets so payload stays bounded for
    long windows; values returned are bytes/sec averaged over each
    bucket."""
    # Clamp to [60s, 6h] — 6h matches the SQLite prune horizon.
    seconds = int(request.args.get("seconds", 300))
    seconds = max(60, min(seconds, 6 * 3600))
    exporter_filter = request.args.get("exporter")
    ifindex_filter  = request.args.get("ifindex")
    if ifindex_filter is not None:
        ifindex_filter = int(ifindex_filter)
    now = int(time.time())
    cutoff = now - seconds

    TARGET_BUCKETS = 600
    bucket_size = max(1, seconds // TARGET_BUCKETS)
    n_buckets   = (seconds + bucket_size - 1) // bucket_size
    sum_in  = [0] * n_buckets
    sum_out = [0] * n_buckets

    def add(ts, b, in_if, out_if):
        bucket = (int(ts) - cutoff) // bucket_size
        if bucket < 0 or bucket >= n_buckets:
            return
        b = b or 0
        if ifindex_filter is None:
            sum_in[bucket] += b
        else:
            if in_if == ifindex_filter:
                sum_in[bucket]  += b
            if out_if == ifindex_filter:
                sum_out[bucket] += b

    if seconds <= 300:
        with state_lock:
            flows = list(recent_flows)
        for f in flows:
            ts = f.get("ts", 0)
            if ts < cutoff or ts > now:
                continue
            if exporter_filter and f.get("exporter") != exporter_filter:
                continue
            add(ts, f.get("bytes"), f.get("input_if"), f.get("output_if"))
    else:
        sql = "SELECT ts, bytes, input_if, output_if FROM flows WHERE ts >= ?"
        params = [cutoff]
        if exporter_filter:
            sql += " AND exporter = ?"
            params.append(exporter_filter)
        with db_lock:
            rows = db_conn.execute(sql, params).fetchall()
        for ts, bts, in_if, out_if in rows:
            if ts > now:
                continue
            add(ts, bts, in_if, out_if)

    ingress = [s / bucket_size for s in sum_in]
    egress  = [s / bucket_size for s in sum_out]
    return jsonify({
        "start_ts":    cutoff,
        "seconds":     seconds,
        "bucket_size": bucket_size,
        "ingress":     ingress,
        "egress":      egress,
    })


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    global WEB_PORT, NETFLOW_PORT, SFLOW_PORT
    parser = argparse.ArgumentParser(description="FlowScope: NetFlow/sFlow collector + dashboard")
    parser.add_argument("--web-port",     type=int, default=WEB_PORT)
    parser.add_argument("--netflow-port", type=int, default=NETFLOW_PORT)
    parser.add_argument("--sflow-port",   type=int, default=SFLOW_PORT)
    args = parser.parse_args()

    WEB_PORT     = args.web_port
    NETFLOW_PORT = args.netflow_port
    SFLOW_PORT   = args.sflow_port

    threading.Thread(target=netflow_listener, daemon=True).start()
    threading.Thread(target=sflow_listener,   daemon=True).start()
    threading.Thread(target=db_prune_loop,    daemon=True).start()
    threading.Thread(target=rdns_loop,        daemon=True).start()
    snmp_mock.start_snmp_poller(devices, interface_stats, state_lock)

    print(f"[web]    http://0.0.0.0:{WEB_PORT}")
    app.run(host=WEB_HOST, port=WEB_PORT, threaded=True, use_reloader=False)


if __name__ == "__main__":
    main()
