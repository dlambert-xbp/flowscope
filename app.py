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
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
from ipaddress import ip_address

from apscheduler.schedulers.background import BackgroundScheduler
from flask import Flask, jsonify, request, send_from_directory, make_response
from waitress import serve as waitress_serve

import snmp_crypto
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
# Extended SNMP fields (admin/oper/errors/discards/mtu/mac) are populated by
# _run_poll on a successful walk and persisted in interface_meta.
interface_stats = defaultdict(lambda: {
    "ingress_bytes":   0,
    "ingress_packets": 0,
    "egress_bytes":    0,
    "egress_packets":  0,
    "last_seen":       0,
    "name":            None,    # filled if SNMP/sFlow tells us
    "alias":           None,    # filled by SNMP poller (ifAlias)
    "speed_bps":       None,
    "admin_status":    None,    # 1=up, 2=down, 3=testing
    "oper_status":     None,    # 1=up, 2=down, 3=testing, 4=unknown, 5=dormant, 6=notPresent, 7=lowerLayerDown
    "in_errors":       None,
    "out_errors":      None,
    "in_discards":     None,
    "out_discards":    None,
    "mtu":             None,
    "mac":             None,
})

# Devices we've heard from
devices = {}  # exporter_ip -> {first_seen, last_seen, flow_count, type, hostname}

# User-defined display labels for exporters and interfaces, persisted in SQLite.
# Loaded at startup, mutated through the label PUT endpoints.
exporter_labels  = {}  # exporter_ip -> str
interface_labels = {}  # (exporter_ip, ifindex) -> str

# Server-side folders (promoted from localStorage). All under state_lock.
folders          = {}  # folder_id -> {"name", "sort_order"}
folder_members   = {}  # folder_id -> set(exporter_ip)
folder_of        = {}  # exporter_ip -> folder_id (inverse index for fast lookup)

# SNMP profiles (named credential sets) and their bindings.
# All access under state_lock. v3 passphrases live decrypted in memory once
# loaded; only ciphertext goes to SQLite (snmp_crypto.encrypt/decrypt).
# A profile dict has all the fields from the snmp_profiles table; '_pt'
# suffix on v3_auth_pass / v3_priv_pass holds plaintext for poll use.
snmp_profiles_mem = {}   # name -> {version, community, v3_username, v3_security_level,
                         #         v3_auth_proto, v3_auth_pass, v3_priv_proto, v3_priv_pass,
                         #         v3_context, port, timeout_s, retries, created_ts, updated_ts}
snmp_bindings_mem = {}   # (scope, scope_id) -> {profile, poll_interval_s, enabled}
                         #   scope ∈ {'global','folder','device'}
                         #   scope_id: '' (global), str(folder_id) (folder), exporter_ip (device)
snmp_poll_status  = {}   # exporter -> {last_poll_ts, last_ok_ts, last_error,
                         #              iface_count, consecutive_failures}

DEFAULT_POLL_INTERVAL_S = 15
SNMP_DEFAULT_PORT       = 161

# NetFlow v9 / IPFIX templates, keyed by (exporter, source_id, template_id)
templates = {}

# Endpoint reverse-DNS cache. Public src/dst IPs harvested from record_flow get
# resolved by endpoint_rdns_loop. Values: None = queued, "" = negative cache,
# anything else = resolved hostname. Has its own lock so the worker doesn't
# contend with the UDP parsers writing to recent_flows.
endpoint_dns = {}
endpoint_dns_lock = threading.Lock()
ENDPOINT_DNS_MAX = 10000

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
    # Enabled so folder_members.folder_id ON DELETE CASCADE works when a
    # folder is removed. Has no effect on tables without FK declarations.
    conn.execute("PRAGMA foreign_keys=ON")
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
    # Server-side folders (promoted from localStorage in v0.x). A device may
    # belong to at most one folder; the unique index on exporter enforces this.
    conn.execute("""
        CREATE TABLE IF NOT EXISTS folders (
            id          INTEGER PRIMARY KEY AUTOINCREMENT,
            name        TEXT NOT NULL UNIQUE,
            sort_order  INTEGER NOT NULL DEFAULT 0
        )
    """)
    conn.execute("""
        CREATE TABLE IF NOT EXISTS folder_members (
            folder_id  INTEGER NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
            exporter   TEXT    NOT NULL,
            PRIMARY KEY (folder_id, exporter)
        )
    """)
    conn.execute("""
        CREATE UNIQUE INDEX IF NOT EXISTS idx_folder_members_exporter
        ON folder_members(exporter)
    """)
    # SNMP profiles (named credential sets). v3 passphrases are stored
    # ENCRYPTED via snmp_crypto.encrypt; never plaintext on disk.
    conn.execute("""
        CREATE TABLE IF NOT EXISTS snmp_profiles (
            name              TEXT PRIMARY KEY,
            version           TEXT NOT NULL,
            community         TEXT,
            v3_username       TEXT,
            v3_security_level TEXT,
            v3_auth_proto     TEXT,
            v3_auth_pass_enc  BLOB,
            v3_priv_proto     TEXT,
            v3_priv_pass_enc  BLOB,
            v3_context        TEXT,
            port              INTEGER NOT NULL DEFAULT 161,
            timeout_s         REAL    NOT NULL DEFAULT 2.0,
            retries           INTEGER NOT NULL DEFAULT 1,
            created_ts        REAL    NOT NULL,
            updated_ts        REAL    NOT NULL
        )
    """)
    # SNMP bindings: which profile applies at which scope, with optional
    # per-scope overrides for poll interval and enabled flag. Resolution
    # walks device -> folder -> global.
    conn.execute("""
        CREATE TABLE IF NOT EXISTS snmp_bindings (
            scope           TEXT NOT NULL CHECK(scope IN ('global','folder','device')),
            scope_id        TEXT NOT NULL,
            profile         TEXT,
            poll_interval_s INTEGER,
            enabled         INTEGER,
            PRIMARY KEY (scope, scope_id),
            FOREIGN KEY (profile) REFERENCES snmp_profiles(name) ON DELETE SET NULL
        )
    """)
    # Per-device poll telemetry. Survives restart so the dashboard "last
    # error" / "consecutive failures" counters don't reset on bounce.
    conn.execute("""
        CREATE TABLE IF NOT EXISTS snmp_poll_status (
            exporter             TEXT PRIMARY KEY,
            last_poll_ts         REAL,
            last_ok_ts           REAL,
            last_error           TEXT,
            iface_count          INTEGER,
            consecutive_failures INTEGER NOT NULL DEFAULT 0
        )
    """)
    # Persisted interface metadata so SNMP-discovered names survive restarts
    # without re-polling. Populated by _run_poll on success; loaded into
    # interface_stats below so the dashboard shows the saved names from the
    # moment the process is up.
    conn.execute("""
        CREATE TABLE IF NOT EXISTS interface_meta (
            exporter      TEXT NOT NULL,
            ifindex       INTEGER NOT NULL,
            name          TEXT,
            alias         TEXT,
            speed_bps     INTEGER,
            admin_status  INTEGER,
            oper_status   INTEGER,
            in_errors     INTEGER,
            out_errors    INTEGER,
            in_discards   INTEGER,
            out_discards  INTEGER,
            mtu           INTEGER,
            mac           TEXT,
            updated_ts    REAL NOT NULL,
            PRIMARY KEY (exporter, ifindex)
        )
    """)
    # Idempotent column migration for older DBs created before the extended
    # SNMP fields existed. SQLite has no IF NOT EXISTS for ALTER TABLE, so
    # we inspect PRAGMA table_info and add what's missing.
    existing_cols = {row[1] for row in conn.execute("PRAGMA table_info(interface_meta)")}
    _NEW_META_COLS = [
        ("admin_status", "INTEGER"),
        ("oper_status",  "INTEGER"),
        ("in_errors",    "INTEGER"),
        ("out_errors",   "INTEGER"),
        ("in_discards",  "INTEGER"),
        ("out_discards", "INTEGER"),
        ("mtu",          "INTEGER"),
        ("mac",          "TEXT"),
    ]
    for col, typ in _NEW_META_COLS:
        if col not in existing_cols:
            conn.execute(f"ALTER TABLE interface_meta ADD COLUMN {col} {typ}")
    # Per-interface counter samples (sFlow if_counters arrivals). Used to
    # compute authoritative ingress/egress rates on the per-interface graph
    # by diffing successive samples. Pruned alongside flows to ~6h.
    conn.execute("""
        CREATE TABLE IF NOT EXISTS iface_counter_samples (
            exporter   TEXT NOT NULL,
            ifindex    INTEGER NOT NULL,
            ts         REAL NOT NULL,
            in_octets  INTEGER,
            out_octets INTEGER,
            in_pkts    INTEGER,
            out_pkts   INTEGER
        )
    """)
    conn.execute("""
        CREATE INDEX IF NOT EXISTS idx_iface_counter_samples
        ON iface_counter_samples(exporter, ifindex, ts)
    """)
    for row in conn.execute("SELECT exporter, label FROM exporter_labels"):
        exporter_labels[row[0]] = row[1]
    for row in conn.execute("SELECT exporter, ifindex, label FROM interface_labels"):
        interface_labels[(row[0], row[1])] = row[2]
    for row in conn.execute("SELECT id, name, sort_order FROM folders"):
        folders[row[0]] = {"name": row[1], "sort_order": row[2]}
        folder_members[row[0]] = set()
    for row in conn.execute("SELECT folder_id, exporter FROM folder_members"):
        folder_members.setdefault(row[0], set()).add(row[1])
        folder_of[row[1]] = row[0]
    # SNMP profiles: decrypt v3 passphrases up front. If the master key isn't
    # configured but encrypted blobs exist, leave the passphrases as None and
    # log; the poller will skip those profiles rather than mis-decrypt.
    snmp_unlock_warnings = []
    for row in conn.execute("""
        SELECT name, version, community, v3_username, v3_security_level,
               v3_auth_proto, v3_auth_pass_enc, v3_priv_proto, v3_priv_pass_enc,
               v3_context, port, timeout_s, retries, created_ts, updated_ts
        FROM snmp_profiles
    """):
        (name, version, community, v3_user, v3_sec, v3_ap, v3_apass_enc,
         v3_pp, v3_ppass_enc, v3_ctx, port, timeout, retries, c_ts, u_ts) = row
        v3_apass = v3_ppass = None
        if v3_apass_enc or v3_ppass_enc:
            if not snmp_crypto.is_configured():
                snmp_unlock_warnings.append(name)
            else:
                try:
                    v3_apass = snmp_crypto.decrypt(v3_apass_enc) if v3_apass_enc else None
                    v3_ppass = snmp_crypto.decrypt(v3_ppass_enc) if v3_ppass_enc else None
                except snmp_crypto.CryptoBadInput as e:
                    snmp_unlock_warnings.append(f"{name} ({e})")
        snmp_profiles_mem[name] = {
            "name": name, "version": version, "community": community,
            "v3_username": v3_user, "v3_security_level": v3_sec,
            "v3_auth_proto": v3_ap, "v3_auth_pass": v3_apass,
            "v3_priv_proto": v3_pp, "v3_priv_pass": v3_ppass,
            "v3_context": v3_ctx, "port": port,
            "timeout_s": timeout, "retries": retries,
            "created_ts": c_ts, "updated_ts": u_ts,
            "_has_auth_pass": bool(v3_apass_enc),
            "_has_priv_pass": bool(v3_ppass_enc),
        }
    if snmp_unlock_warnings:
        print(f"[snmp] WARNING: could not decrypt {len(snmp_unlock_warnings)} profile(s); "
              f"set FLOWSCOPE_SNMP_KEY to the same value used when they were created. "
              f"Affected: {', '.join(snmp_unlock_warnings)}")
    for row in conn.execute("""
        SELECT scope, scope_id, profile, poll_interval_s, enabled FROM snmp_bindings
    """):
        snmp_bindings_mem[(row[0], row[1])] = {
            "profile": row[2],
            "poll_interval_s": row[3],
            "enabled": row[4],
        }
    for row in conn.execute("""
        SELECT exporter, last_poll_ts, last_ok_ts, last_error,
               iface_count, consecutive_failures FROM snmp_poll_status
    """):
        snmp_poll_status[row[0]] = {
            "last_poll_ts": row[1], "last_ok_ts": row[2],
            "last_error":   row[3], "iface_count": row[4],
            "consecutive_failures": row[5],
        }
    # Restore SNMP-discovered interface metadata from previous runs.
    # These pre-create interface_stats entries with zeroed counters; the next
    # flow record (or sFlow counter sample) overwrites the counters as usual.
    for row in conn.execute("""
        SELECT exporter, ifindex, name, alias, speed_bps,
               admin_status, oper_status, in_errors, out_errors,
               in_discards, out_discards, mtu, mac
        FROM interface_meta
    """):
        s = interface_stats[(row[0], row[1])]
        s["name"]         = row[2]
        s["alias"]        = row[3]
        s["speed_bps"]    = row[4]
        s["admin_status"] = row[5]
        s["oper_status"]  = row[6]
        s["in_errors"]    = row[7]
        s["out_errors"]   = row[8]
        s["in_discards"]  = row[9]
        s["out_discards"] = row[10]
        s["mtu"]          = row[11]
        s["mac"]          = row[12]
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


def db_prune_tick():
    """Prune flows + counter samples older than ~6h.
    Runs every 5 minutes via APScheduler."""
    try:
        cutoff = time.time() - 6 * 3600
        with db_lock:
            db_conn.execute("DELETE FROM flows WHERE ts < ?", (cutoff,))
            db_conn.execute(
                "DELETE FROM iface_counter_samples WHERE ts < ?", (cutoff,))
            db_conn.commit()
    except Exception as e:
        print(f"[db] prune error: {e}")


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


def _is_resolvable_public(ip):
    """True only for globally routable addresses worth a PTR lookup.

    Filters out RFC1918, loopback, link-local, multicast, reserved, and
    unspecified — they almost never have public PTRs and querying them
    leaks internal addresses to the system resolver.
    """
    try:
        a = ip_address(ip)
    except (ValueError, TypeError):
        return False
    if a.is_private or a.is_loopback or a.is_link_local:
        return False
    if a.is_multicast or a.is_reserved or a.is_unspecified:
        return False
    return True


def endpoint_rdns_loop():
    """Best-effort reverse DNS for endpoint (src/dst) IPs harvested from flows.

    Mirrors rdns_loop but reads from endpoint_dns. Items are inserted by
    record_flow with value None (queued); we resolve them and write back the
    hostname or "" (negative cache). Per-tick batch size is bounded so a
    startup flood can't peg the resolver.
    """
    BATCH = 50
    while True:
        try:
            with endpoint_dns_lock:
                need = [ip for ip, v in endpoint_dns.items() if v is None][:BATCH]
            for ip in need:
                hn = ""
                try:
                    hn = socket.gethostbyaddr(ip)[0]
                except Exception:
                    pass
                with endpoint_dns_lock:
                    if ip in endpoint_dns:
                        endpoint_dns[ip] = hn
        except Exception as e:
            print(f"[endpoint-rdns] error: {e}")
        time.sleep(5)


# ---------------------------------------------------------------------------
# SNMP poll dispatcher + scheduler
# ---------------------------------------------------------------------------
# walk_iftable_dispatch picks between the mock client (Phase 2 + opt-in
# testing in later phases) and the real pysnmp client (Phase 3+). The
# scheduler thread wakes once per second, walks `devices`, resolves each
# one's effective binding, and runs polls whose interval has elapsed.
# Counter-samples-win invariant (CLAUDE.md): we never overwrite a non-zero
# speed_bps already learned from sFlow.

SNMP_USE_MOCK = os.environ.get("FLOWSCOPE_SNMP_MOCK", "0") in ("1", "true", "True", "yes")


def walk_iftable_dispatch(profile, host, ifindexes):
    """Run an SNMP iftable walk against `host` using `profile`.

    Returns {ifindex: {"name", "alias", "speed_bps"}}. Raises on transport
    or auth errors so the caller can record `last_error`."""
    if SNMP_USE_MOCK:
        client = snmp_mock.MockSNMPClient(
            community=profile.get("community") or "public",
            version=profile.get("version") or "2c",
        )
        return client.walk_iftable(host, ifindexes)
    # Phase 3: real pysnmp-lextudio client lives in snmp_client.py.
    try:
        import snmp_client
    except ImportError:
        # The real client module isn't there yet (Phase 2 deploy without
        # FLOWSCOPE_SNMP_MOCK=1). Surface a clear error rather than crash.
        raise RuntimeError(
            "real SNMP client not available; set FLOWSCOPE_SNMP_MOCK=1 "
            "to use the mock, or install pysnmp-lextudio (Phase 3)")
    return snmp_client.walk_iftable(profile, host, ifindexes)


def _run_poll(exporter, profile, ifindexes):
    """Execute one poll and update interface_stats + snmp_poll_status.
    Returns None on success, error string on failure."""
    err_msg = None
    walk = {}
    try:
        walk = walk_iftable_dispatch(profile, exporter, ifindexes)
    except Exception as e:
        err_msg = f"{type(e).__name__}: {e}"

    now = time.time()
    with state_lock:
        if walk:
            for ifindex, info in walk.items():
                k = (exporter, ifindex)
                s = interface_stats.get(k)
                if s is None:
                    continue
                s["name"]  = info.get("name")  or s.get("name")
                s["alias"] = info.get("alias") or s.get("alias")
                # Counter-samples-win: don't clobber a non-zero speed already
                # learned from sFlow if_counters.
                if not s.get("speed_bps") and info.get("speed_bps"):
                    s["speed_bps"] = info["speed_bps"]
                # Extended fields. SNMP is authoritative for admin/oper
                # status, MTU, MAC; for errors/discards the *latest* sample
                # wins (sFlow if_counters and SNMP both report the same
                # IF-MIB counters, so they should track each other).
                for fld in ("admin_status", "oper_status", "mtu", "mac",
                            "in_errors", "out_errors",
                            "in_discards", "out_discards"):
                    v = info.get(fld)
                    if v is not None:
                        s[fld] = v
        st = snmp_poll_status.setdefault(exporter, {
            "last_poll_ts": None, "last_ok_ts": None, "last_error": None,
            "iface_count": 0, "consecutive_failures": 0,
        })
        st["last_poll_ts"] = now
        if err_msg:
            st["last_error"]           = err_msg
            st["consecutive_failures"] = (st.get("consecutive_failures") or 0) + 1
        else:
            st["last_error"]           = None
            st["last_ok_ts"]           = now
            st["iface_count"]          = len(walk)
            st["consecutive_failures"] = 0

    # Persist status outside state_lock (db_lock allowed).
    try:
        with db_lock:
            db_conn.execute("""
                INSERT INTO snmp_poll_status
                  (exporter, last_poll_ts, last_ok_ts, last_error,
                   iface_count, consecutive_failures)
                VALUES (?,?,?,?,?,?)
                ON CONFLICT(exporter) DO UPDATE SET
                  last_poll_ts=excluded.last_poll_ts,
                  last_ok_ts=COALESCE(excluded.last_ok_ts, snmp_poll_status.last_ok_ts),
                  last_error=excluded.last_error,
                  iface_count=COALESCE(excluded.iface_count, snmp_poll_status.iface_count),
                  consecutive_failures=excluded.consecutive_failures
            """, (
                exporter, now,
                now if not err_msg else None,
                err_msg,
                len(walk) if not err_msg else None,
                snmp_poll_status[exporter]["consecutive_failures"],
            ))
            # Persist the iface meta we just learned so names survive restart.
            # Skipped on failure (walk is empty) and on individual rows that
            # didn't return an ifDescr.
            for ifindex, info in walk.items():
                db_conn.execute("""
                    INSERT INTO interface_meta
                        (exporter, ifindex, name, alias, speed_bps,
                         admin_status, oper_status, in_errors, out_errors,
                         in_discards, out_discards, mtu, mac, updated_ts)
                    VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
                    ON CONFLICT(exporter, ifindex) DO UPDATE SET
                        name=excluded.name,
                        alias=excluded.alias,
                        speed_bps=COALESCE(excluded.speed_bps, interface_meta.speed_bps),
                        admin_status=COALESCE(excluded.admin_status, interface_meta.admin_status),
                        oper_status=COALESCE(excluded.oper_status,  interface_meta.oper_status),
                        in_errors=COALESCE(excluded.in_errors,      interface_meta.in_errors),
                        out_errors=COALESCE(excluded.out_errors,    interface_meta.out_errors),
                        in_discards=COALESCE(excluded.in_discards,  interface_meta.in_discards),
                        out_discards=COALESCE(excluded.out_discards,interface_meta.out_discards),
                        mtu=COALESCE(excluded.mtu, interface_meta.mtu),
                        mac=COALESCE(excluded.mac, interface_meta.mac),
                        updated_ts=excluded.updated_ts
                """, (exporter, ifindex,
                      info.get("name"), info.get("alias"),
                      info.get("speed_bps"),
                      info.get("admin_status"), info.get("oper_status"),
                      info.get("in_errors"), info.get("out_errors"),
                      info.get("in_discards"), info.get("out_discards"),
                      info.get("mtu"), info.get("mac"), now))
            db_conn.commit()
    except Exception as e:
        print(f"[snmp] status persist error: {e}")
    return err_msg


# ThreadPoolExecutor for SNMP polls. Sized for "a few hundred devices, each
# polled every 15s" — 8 workers covers 8 concurrent slow walks (~5s each)
# without contention on state_lock or db_lock. Module-level so APScheduler
# tick handlers reuse it instead of constructing a new pool per tick.
SNMP_POLL_WORKERS = int(os.environ.get("FLOWSCOPE_SNMP_WORKERS", 8))
snmp_pool = ThreadPoolExecutor(
    max_workers=SNMP_POLL_WORKERS, thread_name_prefix="snmp-poll")


def snmp_scheduler_tick():
    """One scheduler tick — eligible polls run concurrently in `snmp_pool`.

    Eligibility per device:
      * never polled successfully OR last poll failed → run every tick
        (bounded in practice by the SNMP timeout, since each device is
        capped by its own future)
      * last poll succeeded → run when `now - last_poll_ts >= interval`
    """
    tick_started = time.time()
    try:
        with state_lock:
            exporters = list(devices.keys())
            per_host_ifs = {}
            for (exp, ifindex) in interface_stats.keys():
                per_host_ifs.setdefault(exp, []).append(ifindex)

        for exporter in exporters:
            try:
                eff = resolve_snmp(exporter)
                if not eff.get("profile"):
                    continue
                # enabled semantics: 0 = explicit off, 1 = explicit on,
                # None = inherit. With a profile bound we default to on,
                # so only skip when something in the chain set 0.
                if eff.get("enabled") == 0:
                    continue
                interval = eff.get("poll_interval_s") or DEFAULT_POLL_INTERVAL_S

                with state_lock:
                    st   = snmp_poll_status.get(exporter)
                    prof = snmp_profiles_mem.get(eff["profile"])
                if prof is None:
                    continue
                # Skip if a poll is already in flight for this device — the
                # ThreadPoolExecutor stores the in-flight future on the status
                # entry. Without this guard, a slow walk could be re-queued
                # every tick.
                if st and st.get("_in_flight"):
                    continue
                last    = st["last_poll_ts"] if st and st.get("last_poll_ts") else 0
                had_err = bool(st and st.get("last_error"))
                if not had_err and tick_started - last < interval:
                    continue

                ifs = per_host_ifs.get(exporter, [])
                if not ifs:
                    continue

                with state_lock:
                    snmp_poll_status.setdefault(exporter, {
                        "last_poll_ts": None, "last_ok_ts": None,
                        "last_error":   None, "iface_count": 0,
                        "consecutive_failures": 0,
                    })["_in_flight"] = True
                snmp_pool.submit(_run_poll_async, exporter, prof, ifs)
            except Exception as e:
                print(f"[snmp] poll dispatch error for {exporter}: {e}")
    except Exception as e:
        print(f"[snmp] scheduler error: {e}")


def _run_poll_async(exporter, profile, ifindexes):
    """ThreadPool worker: run one poll and clear the in-flight flag."""
    try:
        _run_poll(exporter, profile, ifindexes)
    finally:
        with state_lock:
            st = snmp_poll_status.get(exporter)
            if st is not None:
                st["_in_flight"] = False


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def proto_name(num):
    return {1: "ICMP", 6: "TCP", 17: "UDP", 47: "GRE", 50: "ESP",
            51: "AH", 58: "ICMPv6", 89: "OSPF", 132: "SCTP"}.get(num, str(num))


def record_flow(rec, _hydrate=False):
    """Record one flow (NetFlow record or sFlow sampled flow) into all stores.

    _hydrate=True replays a row already on disk back through the in-memory
    aggregates without re-inserting it or bumping the since-start counter.
    """
    rec["ts"] = rec.get("ts", time.time())
    exporter = rec["exporter"]

    with state_lock:
        recent_flows.append(rec)
        if not _hydrate:
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

    # Queue public src/dst IPs for PTR lookup. Done outside state_lock so the
    # endpoint_dns_lock ordering stays consistent (UDP parsers never hold both).
    for ip in (rec.get("src_ip"), rec.get("dst_ip")):
        if ip and _is_resolvable_public(ip):
            with endpoint_dns_lock:
                if ip not in endpoint_dns and len(endpoint_dns) < ENDPOINT_DNS_MAX:
                    endpoint_dns[ip] = None

    if not _hydrate:
        db_insert_flow(rec)


def db_hydrate_recent():
    """Replay the most recent ~RING_SIZE flows from SQLite into the in-memory
    ring + aggregates so top talkers / top ports / protocols / interfaces
    aren't blank after a process restart. Live Flows view will also pre-fill
    with these — that's fine; they were the most recent flows on disk.
    """
    fields = ("ts", "exporter", "proto_name", "src_ip", "dst_ip",
              "src_port", "dst_port", "protocol", "input_if",
              "output_if", "bytes", "packets", "tcp_flags")
    try:
        with db_lock:
            rows = db_conn.execute(
                f"SELECT {', '.join(fields)} FROM flows "
                "ORDER BY ts DESC LIMIT ?",
                (RING_SIZE,)
            ).fetchall()
    except Exception as e:
        print(f"[db] hydrate query failed: {e}")
        return

    # Newest-first from SQLite; reverse so the deque mirrors live chronological order.
    rows.reverse()
    for row in rows:
        rec = {k: v for k, v in zip(fields, row) if v is not None}
        record_flow(rec, _hydrate=True)
    print(f"[db] hydrated {len(rows)} flows from disk")


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
    version, count, _uptime, secs, _nsecs, _seq, _et, _eid, samp = \
        NF5_HDR.unpack_from(data, 0)
    # v5 sampling header: top 2 bits = mode, bottom 14 bits = N (1:N).
    # 0 means sampling not configured at the exporter — treat as 1:1.
    samp_rate = (samp & 0x3FFF) or 1
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
            "bytes":      dOctets * samp_rate,
            "packets":    dPkts   * samp_rate,
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
    34:  ("sampling_interval", None),  # SAMPLING_INTERVAL — applied + dropped at parse time
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
                # If the template carried per-record sampling_interval (field 34),
                # scale bytes/packets to estimate actual traffic and drop the key
                # so it doesn't leak into the recorded flow / DB row.
                samp = rec.pop("sampling_interval", None)
                if samp and samp > 1:
                    if "bytes" in rec:
                        rec["bytes"]   = (rec["bytes"]   or 0) * samp
                    if "packets" in rec:
                        rec["packets"] = (rec["packets"] or 0) * samp
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
    """Parse a flow_sample. We extract input/output ifindex, the sampled
    raw packet header (for src/dst IP and ports), and the sampling rate.

    The sampling rate is critical: an sFlow flow_sample represents one
    packet out of N (samp_rate). The byte count from the raw header is
    therefore the size of that single sampled packet — to estimate the
    actual traffic on the wire we multiply both bytes and packets by N."""
    o = 0
    try:
        _seq  = struct.unpack_from("!I", body, o)[0]; o += 4
        if expanded:
            _src_type  = struct.unpack_from("!I", body, o)[0]; o += 4
            _src_index = struct.unpack_from("!I", body, o)[0]; o += 4
        else:
            o += 4   # source_id (type<<24 | index)
        samp_rate  = struct.unpack_from("!I", body, o)[0]; o += 4
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

    if samp_rate == 0:
        samp_rate = 1

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

    # Scale to estimate actual traffic (sample = 1 of samp_rate).
    rec_template["bytes"]   = (rec_template["bytes"]   or 0) * samp_rate
    rec_template["packets"] = (rec_template["packets"] or 1) * samp_rate
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
                ifindex      = int.from_bytes(rec_body[0:4],   "big")
                ifspeed      = int.from_bytes(rec_body[8:16],  "big")
                _ifdir       = int.from_bytes(rec_body[16:20], "big")
                ifstatus     = int.from_bytes(rec_body[20:24], "big")
                in_oct       = int.from_bytes(rec_body[24:32], "big")
                in_ucast     = int.from_bytes(rec_body[32:36], "big")
                in_discards  = int.from_bytes(rec_body[44:48], "big")
                in_errors    = int.from_bytes(rec_body[48:52], "big")
                out_oct      = int.from_bytes(rec_body[56:64], "big")
                out_ucast    = int.from_bytes(rec_body[64:68], "big")
                out_discards = int.from_bytes(rec_body[76:80], "big")
                out_errors   = int.from_bytes(rec_body[80:84], "big")
            except Exception:
                continue

            now = time.time()
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
                s["last_seen"]       = now
                # sFlow if_counters packs ifAdminStatus (low bit) and
                # ifOperStatus (next bit) into a single u32; split them out so
                # the modal can show both even when SNMP isn't available.
                s["admin_status"]    = 1 if (ifstatus & 0x1) else 2
                s["oper_status"]     = 1 if (ifstatus & 0x2) else 2
                s["in_errors"]       = in_errors
                s["out_errors"]      = out_errors
                s["in_discards"]     = in_discards
                s["out_discards"]    = out_discards

            # Persist the counter sample for the per-interface timeseries
            # endpoint. Outside state_lock; db_lock is fine here.
            try:
                with db_lock:
                    db_conn.execute("""
                        INSERT INTO iface_counter_samples
                            (exporter, ifindex, ts, in_octets, out_octets,
                             in_pkts, out_pkts)
                        VALUES (?,?,?,?,?,?,?)
                    """, (exporter, ifindex, now, in_oct, out_oct,
                          in_ucast, out_ucast))
                    db_conn.commit()
            except Exception as e:
                print(f"[sflow] counter-sample persist error: {e}")


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
                "admin_status":    s.get("admin_status"),
                "oper_status":     s.get("oper_status"),
                "in_errors":       s.get("in_errors"),
                "out_errors":      s.get("out_errors"),
                "in_discards":     s.get("in_discards"),
                "out_discards":    s.get("out_discards"),
                "mtu":             s.get("mtu"),
                "mac":             s.get("mac"),
            })
    rows.sort(key=lambda r: (r["exporter"], r["ifindex"]))
    return jsonify(rows)


@app.route("/api/interfaces/<exporter>/<int:ifindex>/timeseries")
def api_interface_timeseries(exporter, ifindex):
    """Per-interface ingress/egress rate timeseries.

    Primary source: sFlow counter samples (authoritative — these are absolute
    interface counters reported by the device). When available we diff
    successive samples for a bytes/sec rate and forward-fill into buckets.
    Fallback: flow-derived rate (sub-sampled from observed flow records),
    used when no counter samples are present (NetFlow-only exporters or a
    device that doesn't send sFlow if_counters).

    Response shape mirrors /api/timeseries with an extra "source" field so
    the dashboard can show whether it's looking at counters or flows."""
    seconds = int(request.args.get("seconds", 300))
    seconds = max(60, min(seconds, 6 * 3600))
    now = int(time.time())
    cutoff = now - seconds
    TARGET_BUCKETS = 600
    bucket_size = max(1, seconds // TARGET_BUCKETS)
    n_buckets   = (seconds + bucket_size - 1) // bucket_size

    # Pull samples with a 10-minute lookback so bucket 0 has a prior sample
    # to diff against; otherwise the first bucket would always be 0.
    lookback = 600
    with db_lock:
        rows = db_conn.execute("""
            SELECT ts, in_octets, out_octets
            FROM iface_counter_samples
            WHERE exporter = ? AND ifindex = ? AND ts >= ?
            ORDER BY ts
        """, (exporter, ifindex, cutoff - lookback)).fetchall()

    if len(rows) >= 2:
        # Compute rate samples between consecutive pairs.
        rates = []
        for i in range(1, len(rows)):
            t1, in1, out1 = rows[i-1]
            t2, in2, out2 = rows[i]
            dt = t2 - t1
            if dt <= 0:
                continue
            # Counter-wrap defense: 64-bit ifHC counters effectively never
            # wrap; if we ever see a negative delta (counter reset, device
            # reboot), drop the sample rather than show a huge spike.
            d_in  = (in2  or 0) - (in1  or 0)
            d_out = (out2 or 0) - (out1 or 0)
            if d_in < 0 or d_out < 0:
                continue
            rates.append((t2, d_in / dt, d_out / dt))

        if rates:
            ingress = [0.0] * n_buckets
            egress  = [0.0] * n_buckets
            ri = 0
            last_in = last_out = 0.0
            for b in range(n_buckets):
                bucket_end = cutoff + (b + 1) * bucket_size
                while ri < len(rates) and rates[ri][0] <= bucket_end:
                    last_in, last_out = rates[ri][1], rates[ri][2]
                    ri += 1
                ingress[b] = last_in
                egress[b]  = last_out
            return jsonify({
                "start_ts":    cutoff,
                "seconds":     seconds,
                "bucket_size": bucket_size,
                "ingress":     ingress,
                "egress":      egress,
                "source":      "counters",
            })

    # Fallback: flow-derived. Reuse the bucketing logic from /api/timeseries
    # with an ifindex filter.
    sum_in  = [0] * n_buckets
    sum_out = [0] * n_buckets

    def add(ts, b, in_if, out_if):
        bucket = (int(ts) - cutoff) // bucket_size
        if bucket < 0 or bucket >= n_buckets:
            return
        b = b or 0
        if in_if  == ifindex: sum_in[bucket]  += b
        if out_if == ifindex: sum_out[bucket] += b

    if seconds <= 300:
        with state_lock:
            flows = list(recent_flows)
        for f in flows:
            ts = f.get("ts", 0)
            if ts < cutoff or ts > now or f.get("exporter") != exporter:
                continue
            add(ts, f.get("bytes"), f.get("input_if"), f.get("output_if"))
    else:
        with db_lock:
            rows2 = db_conn.execute("""
                SELECT ts, bytes, input_if, output_if
                FROM flows WHERE ts >= ? AND exporter = ?
            """, (cutoff, exporter)).fetchall()
        for ts, bts, in_if, out_if in rows2:
            if ts > now:
                continue
            add(ts, bts, in_if, out_if)

    return jsonify({
        "start_ts":    cutoff,
        "seconds":     seconds,
        "bucket_size": bucket_size,
        "ingress":     [s / bucket_size for s in sum_in],
        "egress":      [s / bucket_size for s in sum_out],
        "source":      "flows",
    })


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


# ---------------------------------------------------------------------------
# Folder API (server-side; promoted from localStorage in v0.x)
# ---------------------------------------------------------------------------
# A device may belong to at most one folder. Folders are flat (no nesting).
# All endpoints take state_lock and db_lock together when mutating.

def _folder_to_dict(fid):
    """Build the JSON shape under state_lock. Caller must hold the lock."""
    f = folders[fid]
    return {
        "id":         fid,
        "name":       f["name"],
        "sort_order": f["sort_order"],
        "deviceIps":  sorted(folder_members.get(fid, set())),
    }


@app.route("/api/folders", methods=["GET", "POST"])
def api_folders():
    if request.method == "GET":
        with state_lock:
            out = [_folder_to_dict(fid) for fid in folders]
        out.sort(key=lambda f: (f["sort_order"], f["name"].lower()))
        return jsonify(out)

    # POST: create. Atomic creation with optional initial members.
    payload = request.get_json(silent=True) or {}
    name = (payload.get("name") or "").strip()
    if not name:
        return jsonify({"ok": False, "error": "name required"}), 400
    sort_order = int(payload.get("sort_order", 0))
    incoming = [str(x).strip() for x in (payload.get("exporters") or []) if x]

    try:
        with db_lock, state_lock:
            cur = db_conn.execute(
                "INSERT INTO folders(name, sort_order) VALUES(?, ?)",
                (name, sort_order))
            fid = cur.lastrowid
            # Initial members: silently steal from any pre-existing folder.
            # The unique index on exporter would otherwise raise IntegrityError.
            for exp in incoming:
                db_conn.execute(
                    "DELETE FROM folder_members WHERE exporter = ?", (exp,))
                db_conn.execute(
                    "INSERT INTO folder_members(folder_id, exporter) VALUES(?, ?)",
                    (fid, exp))
            db_conn.commit()
            # Mirror into in-memory state.
            folders[fid] = {"name": name, "sort_order": sort_order}
            folder_members[fid] = set()
            for exp in incoming:
                old = folder_of.pop(exp, None)
                if old is not None:
                    folder_members.get(old, set()).discard(exp)
                folder_members[fid].add(exp)
                folder_of[exp] = fid
            return jsonify(_folder_to_dict(fid)), 201
    except sqlite3.IntegrityError:
        return jsonify({"ok": False, "error": "folder name already exists"}), 409
    except Exception as e:
        return jsonify({"ok": False, "error": str(e)}), 500


@app.route("/api/folders/<int:fid>", methods=["GET", "PATCH", "DELETE"])
def api_folder(fid):
    if request.method == "GET":
        with state_lock:
            if fid not in folders:
                return jsonify({"ok": False, "error": "not found"}), 404
            return jsonify(_folder_to_dict(fid))

    if request.method == "DELETE":
        try:
            with db_lock, state_lock:
                if fid not in folders:
                    return jsonify({"ok": False, "error": "not found"}), 404
                # FK ON DELETE CASCADE removes folder_members rows.
                db_conn.execute("DELETE FROM folders WHERE id = ?", (fid,))
                db_conn.commit()
                for exp in folder_members.pop(fid, set()):
                    folder_of.pop(exp, None)
                folders.pop(fid, None)
            return ("", 204)
        except Exception as e:
            return jsonify({"ok": False, "error": str(e)}), 500

    # PATCH: rename and/or reorder.
    payload = request.get_json(silent=True) or {}
    new_name = payload.get("name")
    new_sort = payload.get("sort_order")
    if new_name is not None:
        new_name = new_name.strip()
        if not new_name:
            return jsonify({"ok": False, "error": "name cannot be empty"}), 400
    try:
        with db_lock, state_lock:
            if fid not in folders:
                return jsonify({"ok": False, "error": "not found"}), 404
            if new_name is not None:
                db_conn.execute("UPDATE folders SET name = ? WHERE id = ?",
                                (new_name, fid))
                folders[fid]["name"] = new_name
            if new_sort is not None:
                db_conn.execute("UPDATE folders SET sort_order = ? WHERE id = ?",
                                (int(new_sort), fid))
                folders[fid]["sort_order"] = int(new_sort)
            db_conn.commit()
            return jsonify(_folder_to_dict(fid))
    except sqlite3.IntegrityError:
        return jsonify({"ok": False, "error": "folder name already exists"}), 409
    except Exception as e:
        return jsonify({"ok": False, "error": str(e)}), 500


@app.route("/api/folders/<int:fid>/members", methods=["PUT"])
def api_folder_members(fid):
    """Replace the entire member list of <fid>. Devices previously assigned
    elsewhere are moved (a device can only be in one folder)."""
    payload = request.get_json(silent=True) or {}
    incoming = set(str(x).strip() for x in (payload.get("exporters") or []) if x)
    try:
        with db_lock, state_lock:
            if fid not in folders:
                return jsonify({"ok": False, "error": "not found"}), 404
            existing = folder_members.get(fid, set())
            to_remove = existing - incoming
            to_add    = incoming - existing
            for exp in to_remove:
                db_conn.execute(
                    "DELETE FROM folder_members WHERE folder_id = ? AND exporter = ?",
                    (fid, exp))
            for exp in to_add:
                # Steal from any other folder first (unique index on exporter).
                db_conn.execute(
                    "DELETE FROM folder_members WHERE exporter = ?", (exp,))
                db_conn.execute(
                    "INSERT INTO folder_members(folder_id, exporter) VALUES(?, ?)",
                    (fid, exp))
            db_conn.commit()
            for exp in to_remove:
                folder_members[fid].discard(exp)
                if folder_of.get(exp) == fid:
                    folder_of.pop(exp, None)
            for exp in to_add:
                old = folder_of.pop(exp, None)
                if old is not None and old != fid:
                    folder_members.get(old, set()).discard(exp)
                folder_members.setdefault(fid, set()).add(exp)
                folder_of[exp] = fid
            return jsonify(_folder_to_dict(fid))
    except Exception as e:
        return jsonify({"ok": False, "error": str(e)}), 500


@app.route("/api/devices/<exporter>/folder", methods=["PUT", "DELETE"])
def api_device_folder(exporter):
    """Move a single device to a folder, or to root (folder_id=null/DELETE)."""
    if request.method == "DELETE":
        target = None
    else:
        payload = request.get_json(silent=True) or {}
        target = payload.get("folder_id")
        if target is not None:
            try:
                target = int(target)
            except (TypeError, ValueError):
                return jsonify({"ok": False, "error": "folder_id must be int or null"}), 400
    try:
        with db_lock, state_lock:
            if target is not None and target not in folders:
                return jsonify({"ok": False, "error": "folder not found"}), 404
            db_conn.execute(
                "DELETE FROM folder_members WHERE exporter = ?", (exporter,))
            if target is not None:
                db_conn.execute(
                    "INSERT INTO folder_members(folder_id, exporter) VALUES(?, ?)",
                    (target, exporter))
            db_conn.commit()
            old = folder_of.pop(exporter, None)
            if old is not None:
                folder_members.get(old, set()).discard(exporter)
            if target is not None:
                folder_members.setdefault(target, set()).add(exporter)
                folder_of[exporter] = target
            return jsonify({
                "ok":        True,
                "exporter":  exporter,
                "folder_id": target,
            })
    except Exception as e:
        return jsonify({"ok": False, "error": str(e)}), 500


@app.route("/api/flows/recent")
def api_recent_flows():
    """Return the most recent flows. Optional filters:
    - exporter, ifindex (existing)
    - talker_a + talker_b: bidirectional pair (matches either direction)
    - port: matches src_port OR dst_port
    - protocol: matches protocol number
    """
    limit = min(int(request.args.get("limit", 200)), RING_SIZE)
    exporter_filter = request.args.get("exporter")
    ifindex_filter  = request.args.get("ifindex")
    if ifindex_filter is not None:
        ifindex_filter = int(ifindex_filter)
    talker_a = request.args.get("talker_a") or None
    talker_b = request.args.get("talker_b") or None
    port_filter = request.args.get("port")
    if port_filter is not None and port_filter != "":
        try: port_filter = int(port_filter)
        except ValueError: port_filter = None
    else:
        port_filter = None
    protocol_filter = request.args.get("protocol")
    if protocol_filter is not None and protocol_filter != "":
        try: protocol_filter = int(protocol_filter)
        except ValueError: protocol_filter = None
    else:
        protocol_filter = None

    with state_lock:
        flows = list(recent_flows)
    with endpoint_dns_lock:
        dns = dict(endpoint_dns)
    flows.reverse()
    out = []
    for f in flows:
        if exporter_filter and f.get("exporter") != exporter_filter:
            continue
        if ifindex_filter is not None and ifindex_filter not in (
                f.get("input_if"), f.get("output_if")):
            continue
        if talker_a and talker_b:
            src, dst = f.get("src_ip"), f.get("dst_ip")
            if not ((src == talker_a and dst == talker_b) or
                    (src == talker_b and dst == talker_a)):
                continue
        if port_filter is not None and port_filter not in (
                f.get("src_port"), f.get("dst_port")):
            continue
        if protocol_filter is not None and f.get("protocol") != protocol_filter:
            continue
        out.append({
            **f,
            "protocol_name": proto_name(f.get("protocol", 0)),
            "src_host": dns.get(f.get("src_ip")) or "",
            "dst_host": dns.get(f.get("dst_ip")) or "",
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
    with endpoint_dns_lock:
        dns = dict(endpoint_dns)
    rows = [{
        "src_ip":   k[0],
        "dst_ip":   k[1],
        "src_host": dns.get(k[0]) or "",
        "dst_host": dns.get(k[1]) or "",
        **v,
    } for k, v in agg.items()]
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
    # Aggregate by protocol number so the frontend can drill into Live Flows
    # with an unambiguous numeric filter; keep the human-readable name too.
    agg = defaultdict(lambda: {"bytes": 0, "flows": 0})
    with state_lock:
        flows = list(recent_flows)
    for f in flows:
        if exporter_filter and f.get("exporter") != exporter_filter:
            continue
        num = f.get("protocol", 0) or 0
        agg[num]["bytes"] += f.get("bytes", 0) or 0
        agg[num]["flows"] += 1
    rows = [{"protocol": proto_name(k), "proto_num": k, **v}
            for k, v in agg.items()]
    rows.sort(key=lambda r: r["bytes"], reverse=True)
    return jsonify(rows)


# ---------------------------------------------------------------------------
# SNMP profiles, bindings, resolution, polling
# ---------------------------------------------------------------------------
# A profile is a named credential set (v2c community OR v3 user/auth/priv).
# A binding attaches a profile to a scope (global / folder / device) and may
# override poll interval and enabled flag. Resolution for a given exporter
# walks device -> folder -> global, with later tiers overriding earlier ones
# field-by-field. Poll telemetry is in snmp_poll_status, persisted to SQLite.
#
# v3 passphrases live encrypted-at-rest (snmp_crypto). Decrypted plaintext
# only exists in memory for the running process; API responses never return
# cleartext — they return "***set***" / "***unset***" markers instead.

V3_AUTH_PROTOS = {"MD5", "SHA", "SHA224", "SHA256", "SHA384", "SHA512"}
V3_PRIV_PROTOS = {"DES", "3DES", "AES128", "AES192", "AES256"}
V3_SEC_LEVELS  = {"noAuthNoPriv", "authNoPriv", "authPriv"}


def _redact_profile(p):
    """Return the profile dict with v3 passphrases redacted for API output."""
    if p is None:
        return None
    out = {
        "name":              p["name"],
        "version":           p["version"],
        "community":         p.get("community"),
        "v3_username":       p.get("v3_username"),
        "v3_security_level": p.get("v3_security_level"),
        "v3_auth_proto":     p.get("v3_auth_proto"),
        "v3_priv_proto":     p.get("v3_priv_proto"),
        "v3_context":        p.get("v3_context"),
        "v3_auth_pass":      "***set***" if p.get("_has_auth_pass") else "***unset***",
        "v3_priv_pass":      "***set***" if p.get("_has_priv_pass") else "***unset***",
        "port":              p.get("port", SNMP_DEFAULT_PORT),
        "timeout_s":         p.get("timeout_s"),
        "retries":           p.get("retries"),
        "created_ts":        p.get("created_ts"),
        "updated_ts":        p.get("updated_ts"),
    }
    return out


def _validate_profile_payload(payload, partial=False):
    """Validate fields in a POST or PATCH body. Returns (cleaned_dict, error_or_None)."""
    out = {}
    if "version" in payload:
        v = str(payload["version"]).strip()
        if v not in ("2c", "3"):
            return None, "version must be '2c' or '3'"
        out["version"] = v
    elif not partial:
        return None, "version is required"

    # Optional / version-specific fields
    if "community" in payload:
        c = payload["community"]
        out["community"] = (str(c).strip() or None) if c is not None else None
    for k in ("v3_username", "v3_context"):
        if k in payload:
            v = payload[k]
            out[k] = (str(v).strip() or None) if v is not None else None
    if "v3_security_level" in payload:
        v = payload["v3_security_level"]
        if v is not None and v not in V3_SEC_LEVELS:
            return None, f"v3_security_level must be one of {sorted(V3_SEC_LEVELS)}"
        out["v3_security_level"] = v
    if "v3_auth_proto" in payload:
        v = payload["v3_auth_proto"]
        if v is not None and v not in V3_AUTH_PROTOS:
            return None, f"v3_auth_proto must be one of {sorted(V3_AUTH_PROTOS)}"
        out["v3_auth_proto"] = v
    if "v3_priv_proto" in payload:
        v = payload["v3_priv_proto"]
        if v is not None and v not in V3_PRIV_PROTOS:
            return None, f"v3_priv_proto must be one of {sorted(V3_PRIV_PROTOS)}"
        out["v3_priv_proto"] = v
    # Passphrases: empty string clears, missing key leaves alone, None clears.
    for k in ("v3_auth_pass", "v3_priv_pass"):
        if k in payload:
            v = payload[k]
            out[k] = None if (v is None or v == "") else str(v)
    if "port" in payload:
        try:
            p = int(payload["port"])
        except (TypeError, ValueError):
            return None, "port must be an integer"
        if not (1 <= p <= 65535):
            return None, "port out of range"
        out["port"] = p
    if "timeout_s" in payload:
        try:
            t = float(payload["timeout_s"])
        except (TypeError, ValueError):
            return None, "timeout_s must be a number"
        if t <= 0 or t > 60:
            return None, "timeout_s out of range (0, 60]"
        out["timeout_s"] = t
    if "retries" in payload:
        try:
            r = int(payload["retries"])
        except (TypeError, ValueError):
            return None, "retries must be an integer"
        if r < 0 or r > 10:
            return None, "retries out of range [0, 10]"
        out["retries"] = r
    return out, None


def _persist_profile(name, p, is_create):
    """Write the in-memory profile back to SQLite. Caller holds db_lock + state_lock."""
    auth_enc = snmp_crypto.encrypt(p.get("v3_auth_pass")) if p.get("v3_auth_pass") else None
    priv_enc = snmp_crypto.encrypt(p.get("v3_priv_pass")) if p.get("v3_priv_pass") else None
    if is_create:
        db_conn.execute("""
            INSERT INTO snmp_profiles
            (name, version, community, v3_username, v3_security_level,
             v3_auth_proto, v3_auth_pass_enc, v3_priv_proto, v3_priv_pass_enc,
             v3_context, port, timeout_s, retries, created_ts, updated_ts)
            VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
        """, (
            name, p["version"], p.get("community"),
            p.get("v3_username"), p.get("v3_security_level"),
            p.get("v3_auth_proto"), auth_enc,
            p.get("v3_priv_proto"), priv_enc,
            p.get("v3_context"),
            p.get("port", SNMP_DEFAULT_PORT),
            p.get("timeout_s", 2.0),
            p.get("retries", 1),
            p["created_ts"], p["updated_ts"],
        ))
    else:
        db_conn.execute("""
            UPDATE snmp_profiles SET
                version=?, community=?, v3_username=?, v3_security_level=?,
                v3_auth_proto=?, v3_auth_pass_enc=?, v3_priv_proto=?,
                v3_priv_pass_enc=?, v3_context=?, port=?, timeout_s=?,
                retries=?, updated_ts=?
            WHERE name=?
        """, (
            p["version"], p.get("community"), p.get("v3_username"),
            p.get("v3_security_level"), p.get("v3_auth_proto"), auth_enc,
            p.get("v3_priv_proto"), priv_enc, p.get("v3_context"),
            p.get("port", SNMP_DEFAULT_PORT),
            p.get("timeout_s", 2.0), p.get("retries", 1),
            p["updated_ts"], name,
        ))
    db_conn.commit()
    p["_has_auth_pass"] = bool(auth_enc)
    p["_has_priv_pass"] = bool(priv_enc)


@app.route("/api/snmp/profiles", methods=["GET", "POST"])
def api_snmp_profiles():
    if request.method == "GET":
        with state_lock:
            out = [_redact_profile(p) for p in snmp_profiles_mem.values()]
        out.sort(key=lambda x: x["name"].lower())
        return jsonify(out)

    # POST: create. Body must include "name" + at minimum "version".
    payload = request.get_json(silent=True) or {}
    name = (payload.get("name") or "").strip()
    if not name:
        return jsonify({"ok": False, "error": "name required"}), 400
    cleaned, err = _validate_profile_payload(payload, partial=False)
    if err:
        return jsonify({"ok": False, "error": err}), 400
    # If a v3 passphrase was supplied but crypto isn't configured, fail fast.
    if (cleaned.get("v3_auth_pass") or cleaned.get("v3_priv_pass")) \
            and not snmp_crypto.is_configured():
        return jsonify({"ok": False,
                        "error": f"{snmp_crypto.ENV_KEY} must be set to store v3 passphrases"}), 400
    try:
        with db_lock, state_lock:
            if name in snmp_profiles_mem:
                return jsonify({"ok": False, "error": "profile name already exists"}), 409
            now = time.time()
            p = {
                "name":              name,
                "version":           cleaned["version"],
                "community":         cleaned.get("community"),
                "v3_username":       cleaned.get("v3_username"),
                "v3_security_level": cleaned.get("v3_security_level"),
                "v3_auth_proto":     cleaned.get("v3_auth_proto"),
                "v3_auth_pass":      cleaned.get("v3_auth_pass"),
                "v3_priv_proto":     cleaned.get("v3_priv_proto"),
                "v3_priv_pass":      cleaned.get("v3_priv_pass"),
                "v3_context":        cleaned.get("v3_context"),
                "port":              cleaned.get("port", SNMP_DEFAULT_PORT),
                "timeout_s":         cleaned.get("timeout_s", 2.0),
                "retries":           cleaned.get("retries", 1),
                "created_ts":        now,
                "updated_ts":        now,
                "_has_auth_pass":    False,
                "_has_priv_pass":    False,
            }
            _persist_profile(name, p, is_create=True)
            snmp_profiles_mem[name] = p
            return jsonify(_redact_profile(p)), 201
    except Exception as e:
        return jsonify({"ok": False, "error": str(e)}), 500


@app.route("/api/snmp/profiles/<name>", methods=["GET", "PATCH", "DELETE"])
def api_snmp_profile(name):
    if request.method == "GET":
        with state_lock:
            p = snmp_profiles_mem.get(name)
            if not p:
                return jsonify({"ok": False, "error": "not found"}), 404
            return jsonify(_redact_profile(p))

    if request.method == "DELETE":
        try:
            with db_lock, state_lock:
                if name not in snmp_profiles_mem:
                    return jsonify({"ok": False, "error": "not found"}), 404
                # Bindings that referenced this profile have ON DELETE SET NULL,
                # so they remain but lose their profile pointer. Mirror that in
                # memory so resolve_snmp sees the change immediately.
                db_conn.execute("DELETE FROM snmp_profiles WHERE name = ?", (name,))
                db_conn.commit()
                snmp_profiles_mem.pop(name, None)
                for b in snmp_bindings_mem.values():
                    if b.get("profile") == name:
                        b["profile"] = None
            return ("", 204)
        except Exception as e:
            return jsonify({"ok": False, "error": str(e)}), 500

    # PATCH: partial update. Cannot change name (it's the PK).
    payload = request.get_json(silent=True) or {}
    cleaned, err = _validate_profile_payload(payload, partial=True)
    if err:
        return jsonify({"ok": False, "error": err}), 400
    if (cleaned.get("v3_auth_pass") or cleaned.get("v3_priv_pass")) \
            and not snmp_crypto.is_configured():
        return jsonify({"ok": False,
                        "error": f"{snmp_crypto.ENV_KEY} must be set to store v3 passphrases"}), 400
    try:
        with db_lock, state_lock:
            p = snmp_profiles_mem.get(name)
            if not p:
                return jsonify({"ok": False, "error": "not found"}), 404
            for k, v in cleaned.items():
                p[k] = v
            p["updated_ts"] = time.time()
            _persist_profile(name, p, is_create=False)
            return jsonify(_redact_profile(p))
    except Exception as e:
        return jsonify({"ok": False, "error": str(e)}), 500


@app.route("/api/snmp/profiles/<name>/test", methods=["POST"])
def api_snmp_profile_test(name):
    """Trial-walk a profile against a target. In Phase 2 with the mock
    client this always returns success with synthetic data; Phase 3 will
    perform a real SNMP walk."""
    payload = request.get_json(silent=True) or {}
    exporter = (payload.get("exporter") or "").strip()
    if not exporter:
        return jsonify({"ok": False, "error": "exporter required"}), 400
    with state_lock:
        p = snmp_profiles_mem.get(name)
        if not p:
            return jsonify({"ok": False, "error": "profile not found"}), 404
        # Ifindexes the test should ask about: anything FlowScope already saw
        # for this exporter. Falls back to a few synthetic ones if unknown.
        ifs = sorted({i for (e, i) in interface_stats.keys() if e == exporter})
    if not ifs:
        ifs = [1, 2, 3]
    try:
        result = walk_iftable_dispatch(p, exporter, ifs)
        return jsonify({"ok": True, "profile": name, "exporter": exporter,
                        "ifaces": [{"ifindex": i, **info} for i, info in result.items()]})
    except Exception as e:
        return jsonify({"ok": False, "error": str(e)}), 500


# ----- Bindings -----

def _binding_to_dict(scope, scope_id, b):
    return {
        "scope":           scope,
        "scope_id":        scope_id,
        "profile":         b.get("profile"),
        "poll_interval_s": b.get("poll_interval_s"),
        "enabled":         b.get("enabled"),
    }


def _validate_binding_payload(payload):
    out = {}
    if "profile" in payload:
        v = payload["profile"]
        out["profile"] = (str(v).strip() or None) if v is not None else None
    if "poll_interval_s" in payload:
        v = payload["poll_interval_s"]
        if v is None:
            out["poll_interval_s"] = None
        else:
            try:
                iv = int(v)
            except (TypeError, ValueError):
                return None, "poll_interval_s must be an integer or null"
            if iv < 5 or iv > 86400:
                return None, "poll_interval_s out of range [5, 86400]"
            out["poll_interval_s"] = iv
    if "enabled" in payload:
        v = payload["enabled"]
        if v is None:
            out["enabled"] = None
        else:
            out["enabled"] = 1 if bool(v) else 0
    return out, None


def _upsert_binding(scope, scope_id, fields):
    """Caller holds db_lock + state_lock."""
    existing = snmp_bindings_mem.get((scope, scope_id))
    if existing is None:
        db_conn.execute("""
            INSERT INTO snmp_bindings(scope, scope_id, profile, poll_interval_s, enabled)
            VALUES(?,?,?,?,?)
        """, (scope, scope_id, fields.get("profile"),
              fields.get("poll_interval_s"), fields.get("enabled")))
        snmp_bindings_mem[(scope, scope_id)] = {
            "profile":         fields.get("profile"),
            "poll_interval_s": fields.get("poll_interval_s"),
            "enabled":         fields.get("enabled"),
        }
    else:
        for k in ("profile", "poll_interval_s", "enabled"):
            if k in fields:
                existing[k] = fields[k]
        db_conn.execute("""
            UPDATE snmp_bindings SET profile=?, poll_interval_s=?, enabled=?
            WHERE scope=? AND scope_id=?
        """, (existing["profile"], existing["poll_interval_s"],
              existing["enabled"], scope, scope_id))
    db_conn.commit()


def _delete_binding(scope, scope_id):
    db_conn.execute("DELETE FROM snmp_bindings WHERE scope=? AND scope_id=?",
                    (scope, scope_id))
    db_conn.commit()
    snmp_bindings_mem.pop((scope, scope_id), None)


@app.route("/api/snmp/bindings", methods=["GET"])
def api_snmp_bindings():
    with state_lock:
        out = [_binding_to_dict(s, sid, b) for (s, sid), b in snmp_bindings_mem.items()]
    return jsonify(out)


def _binding_endpoint(scope, scope_id):
    """Shared GET/PUT/DELETE for the three binding routes."""
    if request.method == "GET":
        with state_lock:
            b = snmp_bindings_mem.get((scope, scope_id))
            if not b:
                return jsonify({"ok": False, "error": "not found"}), 404
            return jsonify(_binding_to_dict(scope, scope_id, b))
    if request.method == "DELETE":
        try:
            with db_lock, state_lock:
                if (scope, scope_id) not in snmp_bindings_mem:
                    return ("", 204)   # idempotent
                _delete_binding(scope, scope_id)
            return ("", 204)
        except Exception as e:
            return jsonify({"ok": False, "error": str(e)}), 500
    # PUT
    payload = request.get_json(silent=True) or {}
    cleaned, err = _validate_binding_payload(payload)
    if err:
        return jsonify({"ok": False, "error": err}), 400
    # Validate referenced profile (if any) exists.
    if cleaned.get("profile") is not None:
        with state_lock:
            if cleaned["profile"] not in snmp_profiles_mem:
                return jsonify({"ok": False, "error": "referenced profile does not exist"}), 400
    # For folder scope, validate the folder id.
    if scope == "folder":
        try:
            fid = int(scope_id)
        except (TypeError, ValueError):
            return jsonify({"ok": False, "error": "folder id must be an integer"}), 400
        with state_lock:
            if fid not in folders:
                return jsonify({"ok": False, "error": "folder not found"}), 404
    try:
        with db_lock, state_lock:
            _upsert_binding(scope, scope_id, cleaned)
            return jsonify(_binding_to_dict(scope, scope_id,
                                            snmp_bindings_mem[(scope, scope_id)]))
    except Exception as e:
        return jsonify({"ok": False, "error": str(e)}), 500


@app.route("/api/snmp/bindings/global", methods=["GET", "PUT", "DELETE"])
def api_snmp_binding_global():
    return _binding_endpoint("global", "")


@app.route("/api/snmp/bindings/folder/<int:fid>", methods=["GET", "PUT", "DELETE"])
def api_snmp_binding_folder(fid):
    return _binding_endpoint("folder", str(fid))


@app.route("/api/snmp/bindings/device/<exporter>", methods=["GET", "PUT", "DELETE"])
def api_snmp_binding_device(exporter):
    return _binding_endpoint("device", exporter)


# ----- Resolution -----

def resolve_snmp(exporter):
    """Walk device -> folder -> global, merging fields from each tier.
    Each tier overrides the previous tier's value for any field it sets to
    non-None. Returns a dict with effective profile + interval + enabled,
    and a per-field source map. Caller may hold state_lock or not — we
    take it locally since this is read-only."""
    with state_lock:
        eff = {"profile": None, "poll_interval_s": None, "enabled": None}
        sources = {"profile": None, "poll_interval_s": None, "enabled": None}
        chain = []
        # Tier 1: global.
        b = snmp_bindings_mem.get(("global", ""))
        if b is not None:
            chain.append("global")
            for k in eff:
                if b.get(k) is not None:
                    eff[k] = b[k]
                    sources[k] = "global"
        # Tier 2: folder.
        fid = folder_of.get(exporter)
        if fid is not None:
            b = snmp_bindings_mem.get(("folder", str(fid)))
            if b is not None:
                tag = f"folder:{fid}"
                chain.append(tag)
                for k in eff:
                    if b.get(k) is not None:
                        eff[k] = b[k]
                        sources[k] = tag
        # Tier 3: device.
        b = snmp_bindings_mem.get(("device", exporter))
        if b is not None:
            chain.append("device")
            for k in eff:
                if b.get(k) is not None:
                    eff[k] = b[k]
                    sources[k] = "device"
        return {
            "exporter":        exporter,
            "profile":         eff["profile"],
            "poll_interval_s": eff["poll_interval_s"],
            "enabled":         eff["enabled"],
            "sources":         sources,
            "chain":           chain,
        }


@app.route("/api/snmp/effective/<exporter>")
def api_snmp_effective(exporter):
    return jsonify(resolve_snmp(exporter))


# ----- Status & manual poll -----

@app.route("/api/snmp/status")
def api_snmp_status():
    """Aggregate poll telemetry plus a tiny config snapshot."""
    exporter_filter = request.args.get("exporter")
    now = time.time()
    with state_lock:
        rows = []
        ok = stale = err = 0
        for ip, s in snmp_poll_status.items():
            if exporter_filter and ip != exporter_filter:
                continue
            eff = None
            # Lightweight inline resolution to avoid double-locking; reuses
            # the same dicts under the lock.
            d = snmp_bindings_mem.get(("device", ip))
            f = snmp_bindings_mem.get(("folder", str(folder_of.get(ip)))) \
                if folder_of.get(ip) is not None else None
            g = snmp_bindings_mem.get(("global", ""))
            interval = None
            for src in (g, f, d):
                if src and src.get("poll_interval_s") is not None:
                    interval = src["poll_interval_s"]
            interval = interval or DEFAULT_POLL_INTERVAL_S
            row = {
                "exporter":             ip,
                "last_poll_ts":         s.get("last_poll_ts"),
                "last_ok_ts":           s.get("last_ok_ts"),
                "last_error":           s.get("last_error"),
                "iface_count":          s.get("iface_count"),
                "consecutive_failures": s.get("consecutive_failures", 0),
            }
            # Bucket: ok / stale / err
            if s.get("consecutive_failures", 0) >= 3:
                err += 1; row["health"] = "error"
            elif s.get("last_ok_ts") and (now - s["last_ok_ts"]) <= 2 * interval:
                ok += 1; row["health"] = "ok"
            else:
                stale += 1; row["health"] = "stale"
            rows.append(row)
        rows.sort(key=lambda r: (r["health"] != "error", r["health"] != "stale",
                                  r["exporter"]))
        return jsonify({
            "summary": {
                "ok":    ok,
                "stale": stale,
                "error": err,
                "total": ok + stale + err,
            },
            "config": {
                "mock":          SNMP_USE_MOCK,
                "key_configured": snmp_crypto.is_configured(),
                "default_interval_s": DEFAULT_POLL_INTERVAL_S,
            },
            "devices": rows,
        })


@app.route("/api/snmp/poll/<exporter>", methods=["POST"])
def api_snmp_poll(exporter):
    """Trigger an immediate poll for one exporter, ignoring its scheduled
    cadence. Returns the same shape as the scheduler would record."""
    eff = resolve_snmp(exporter)
    if not eff.get("profile"):
        return jsonify({"ok": False, "error": "no profile bound for this device"}), 400
    with state_lock:
        p = snmp_profiles_mem.get(eff["profile"])
        if not p:
            return jsonify({"ok": False, "error": "bound profile no longer exists"}), 400
        ifs = sorted({i for (e, i) in interface_stats.keys() if e == exporter})
    if not ifs:
        return jsonify({"ok": False, "error": "no known interfaces for this exporter"}), 400
    err = _run_poll(exporter, p, ifs)
    return jsonify({"ok": err is None, "error": err, "exporter": exporter})


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

    db_hydrate_recent()

    threading.Thread(target=netflow_listener,   daemon=True).start()
    threading.Thread(target=sflow_listener,     daemon=True).start()
    threading.Thread(target=rdns_loop,          daemon=True).start()
    threading.Thread(target=endpoint_rdns_loop, daemon=True).start()

    # APScheduler manages the SNMP poll dispatcher (1Hz) and the SQLite prune
    # job (every 5 minutes). Each tick is short — the SNMP tick only enqueues
    # work onto snmp_pool, the prune tick is one DELETE.
    scheduler = BackgroundScheduler(daemon=True, timezone="UTC")
    scheduler.add_job(snmp_scheduler_tick, "interval", seconds=1,
                      id="snmp-sched", max_instances=1, coalesce=True)
    scheduler.add_job(db_prune_tick, "interval", minutes=5,
                      id="db-prune", max_instances=1, coalesce=True)
    scheduler.start()

    mode = "mock" if SNMP_USE_MOCK else "real"
    crypto_state = "configured" if snmp_crypto.is_configured() else "no key set"
    print(f"[snmp]   scheduler running ({mode}; FLOWSCOPE_SNMP_KEY: {crypto_state}; "
          f"workers={SNMP_POLL_WORKERS})")
    web_threads = int(os.environ.get("FLOWSCOPE_WEB_THREADS", 32))
    print(f"[web]    http://{WEB_HOST}:{WEB_PORT} (waitress, threads={web_threads})")
    waitress_serve(app, host=WEB_HOST, port=WEB_PORT, threads=web_threads)


if __name__ == "__main__":
    main()
