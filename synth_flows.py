#!/usr/bin/env python3
"""
Synthetic flow generator for FlowScope.

Simulates a small enterprise network:
  - 2 Arista switches sending sFlow v5 (an "edge" and a "core")
  - 1 router sending NetFlow v9 / IPFIX

Each "switch" has 8 interfaces with realistic-looking traffic mixes:
  - HTTPS to common destinations (Google, Cloudflare, AWS)
  - DNS lookups
  - SSH and SNMP management traffic
  - Internal east-west traffic between RFC1918 ranges
  - One noisy "top talker" doing a big file transfer

Usage:
    python synth_flows.py --target 127.0.0.1 \
        --sflow-port 6343 --netflow-port 2055 \
        --duration 600 --rate 30
"""

import argparse
import random
import socket
import struct
import time
from ipaddress import ip_address


# ---------------------------------------------------------------------------
# Synthetic network model
# ---------------------------------------------------------------------------

class SyntheticDevice:
    """A simulated switch or router with interfaces and an interface counter
    state that increases monotonically over time (so sFlow counter samples
    look real)."""

    def __init__(self, agent_ip, kind, interfaces):
        self.agent_ip = agent_ip
        self.kind     = kind          # 'sflow' or 'netflow'
        self.interfaces = interfaces  # list of dicts with name, ifindex, speed, role
        self.seq      = 0
        # Per-interface running counters
        self.counters = {
            i["ifindex"]: {
                "in_oct": 0, "in_pkt": 0,
                "out_oct": 0, "out_pkt": 0,
            } for i in interfaces
        }
        # NetFlow v9 template state
        self.nf9_template_sent = False

    def bump_counters(self, ifindex, in_b, in_p, out_b, out_p):
        c = self.counters[ifindex]
        c["in_oct"]  += in_b
        c["in_pkt"]  += in_p
        c["out_oct"] += out_b
        c["out_pkt"] += out_p


# Three devices in the synthetic network
DEVICES = [
    SyntheticDevice(
        agent_ip="10.10.1.1",
        kind="sflow",
        interfaces=[
            {"name": "Ethernet1",  "ifindex": 1,  "speed": 10_000_000_000, "role": "uplink"},
            {"name": "Ethernet2",  "ifindex": 2,  "speed": 10_000_000_000, "role": "uplink"},
            {"name": "Ethernet3",  "ifindex": 3,  "speed":  1_000_000_000, "role": "access"},
            {"name": "Ethernet4",  "ifindex": 4,  "speed":  1_000_000_000, "role": "access"},
            {"name": "Ethernet5",  "ifindex": 5,  "speed":  1_000_000_000, "role": "access"},
            {"name": "Ethernet6",  "ifindex": 6,  "speed":  1_000_000_000, "role": "access"},
            {"name": "Management1","ifindex": 999,"speed":  1_000_000_000, "role": "mgmt"},
        ],
    ),
    SyntheticDevice(
        agent_ip="10.10.1.2",
        kind="sflow",
        interfaces=[
            {"name": "Ethernet1",  "ifindex": 1,  "speed": 40_000_000_000, "role": "uplink"},
            {"name": "Ethernet2",  "ifindex": 2,  "speed": 40_000_000_000, "role": "uplink"},
            {"name": "Ethernet3",  "ifindex": 3,  "speed": 10_000_000_000, "role": "spine"},
            {"name": "Ethernet4",  "ifindex": 4,  "speed": 10_000_000_000, "role": "spine"},
            {"name": "Management1","ifindex": 999,"speed":  1_000_000_000, "role": "mgmt"},
        ],
    ),
    SyntheticDevice(
        agent_ip="10.10.2.1",
        kind="netflow",
        interfaces=[
            {"name": "GigabitEthernet0/0", "ifindex": 1, "speed": 1_000_000_000, "role": "wan"},
            {"name": "GigabitEthernet0/1", "ifindex": 2, "speed": 1_000_000_000, "role": "lan"},
            {"name": "GigabitEthernet0/2", "ifindex": 3, "speed": 1_000_000_000, "role": "lan"},
            {"name": "Loopback0",          "ifindex": 100,"speed":      0,        "role": "lo"},
        ],
    ),
]


# Realistic destination IP pools
EXTERNAL_DESTS = [
    "8.8.8.8", "8.8.4.4",                 # Google DNS
    "1.1.1.1", "1.0.0.1",                 # Cloudflare DNS
    "142.250.80.46",                      # google.com
    "151.101.1.140",                      # fastly
    "104.16.132.229", "104.16.133.229",   # Cloudflare
    "52.85.132.10",                       # AWS CloudFront
    "13.107.42.14",                       # Microsoft
    "140.82.121.4",                       # GitHub
    "208.67.222.222",                     # OpenDNS
    "199.232.45.140",                     # Reddit
]

INTERNAL_RANGES = [
    "10.20.30.", "10.20.31.", "10.20.32.",
    "192.168.10.", "192.168.20.",
    "172.16.5.", "172.16.6.",
]

# Traffic profiles (relative weights)
PROFILES = [
    # (description, src_pool, dst_pool, dst_port, proto, bytes_range, pkt_range, weight)
    ("HTTPS browsing",   "internal", "external", 443,   6, (500, 250_000),    (5, 200), 50),
    ("HTTP",             "internal", "external", 80,    6, (500, 80_000),     (5, 80),  6),
    ("DNS",              "internal", "external", 53,   17, (80, 250),         (1, 2),   25),
    ("SSH",              "internal", "internal", 22,    6, (200, 4_000),      (3, 30),  4),
    ("SMB",              "internal", "internal", 445,   6, (1_500, 90_000),   (10, 80), 5),
    ("RDP",              "internal", "internal", 3389,  6, (800, 25_000),     (8, 60),  3),
    ("SNMP poll",        "internal", "internal", 161,  17, (100, 400),        (1, 3),   8),
    ("NTP",              "internal", "external", 123,  17, (90, 90),          (1, 1),   3),
    ("BGP",              "internal", "internal", 179,   6, (200, 800),        (1, 5),   1),
    ("Internal HTTPS",   "internal", "internal", 443,   6, (500, 50_000),     (5, 50),  10),
    ("ICMP ping",        "internal", "external", 0,     1, (84, 84),          (1, 1),   2),
]


def random_internal_ip():
    return random.choice(INTERNAL_RANGES) + str(random.randint(2, 250))


def pick_endpoints(profile):
    desc, src_pool, dst_pool, dport, proto, byte_r, pkt_r, _w = profile
    src = random_internal_ip() if src_pool == "internal" else random.choice(EXTERNAL_DESTS)
    if dst_pool == "internal":
        dst = random_internal_ip()
        while dst == src:
            dst = random_internal_ip()
    else:
        dst = random.choice(EXTERNAL_DESTS)
    sport = random.randint(32768, 60999) if proto in (6, 17) else 0
    nbytes = random.randint(*byte_r)
    npkts  = random.randint(*pkt_r)
    return src, dst, sport, dport, proto, nbytes, npkts, desc


def weighted_pick():
    weights = [p[-1] for p in PROFILES]
    total = sum(weights)
    r = random.random() * total
    acc = 0
    for p in PROFILES:
        acc += p[-1]
        if r <= acc:
            return p
    return PROFILES[-1]


# ---------------------------------------------------------------------------
# sFlow v5 packet builder
# ---------------------------------------------------------------------------

def build_eth_ipv4_pkt(src_ip, dst_ip, sport, dport, proto, frame_len):
    """Build a synthetic Ethernet + IPv4 (+ TCP/UDP/ICMP) header that fits
    inside an sFlow raw_packet_header sample."""
    # We only emit the first ~54 bytes (header), which is what real switches do.
    eth = b"\xaa\xbb\xcc\xdd\xee\x01" + b"\xaa\xbb\xcc\xdd\xee\x02" + b"\x08\x00"
    # IPv4 header (no options, 20 bytes)
    total_len = max(20, frame_len - 14)
    if total_len > 0xFFFF:
        total_len = 0xFFFF
    ip_hdr = struct.pack("!BBHHHBBH",
        0x45, 0,
        total_len,
        random.randint(0, 65535),
        0,
        64, proto, 0,
    ) + socket.inet_aton(src_ip) + socket.inet_aton(dst_ip)
    if proto in (6, 17):
        l4 = struct.pack("!HH", sport, dport) + b"\x00" * 16
    elif proto == 1:
        l4 = b"\x08\x00\x00\x00\x00\x00\x00\x00"  # ICMP echo request
    else:
        l4 = b""
    return eth + ip_hdr + l4


def build_sflow_flow_sample(device, in_if, out_if, src_ip, dst_ip, sport, dport,
                             proto, nbytes, npkts):
    """Build one sFlow flow_sample (format=1) with a sampled raw header."""
    sampled_pkt = build_eth_ipv4_pkt(src_ip, dst_ip, sport, dport, proto, nbytes)

    raw_hdr  = struct.pack("!IIII", 1, nbytes, 0, len(sampled_pkt))
    raw_hdr += sampled_pkt
    while len(raw_hdr) % 4:
        raw_hdr += b"\x00"

    flow_record = struct.pack("!II", 1, len(raw_hdr)) + raw_hdr

    body  = struct.pack("!I", random.randint(1, 1_000_000))   # sample sequence
    body += struct.pack("!I", (0 << 24) | in_if)              # source_id
    body += struct.pack("!I", 1000)                           # sampling rate
    body += struct.pack("!I", random.randint(1, 1_000_000))   # sample pool
    body += struct.pack("!I", 0)                              # drops
    body += struct.pack("!I", in_if)
    body += struct.pack("!I", out_if)
    body += struct.pack("!I", 1)                              # num_records
    body += flow_record

    return struct.pack("!II", 1, len(body)) + body


def build_sflow_counter_sample(device, iface):
    """Build one sFlow counters_sample with generic interface counters
    (record tag 1, 88 bytes)."""
    c = device.counters[iface["ifindex"]]
    if_counters = struct.pack("!I", iface["ifindex"])           # ifIndex
    if_counters += struct.pack("!I", 6)                          # ifType ethernetCsmacd
    if_counters += struct.pack("!Q", iface["speed"])             # ifSpeed
    if_counters += struct.pack("!I", 1)                          # ifDirection full-duplex
    if_counters += struct.pack("!I", 3)                          # ifStatus admin+oper up
    if_counters += struct.pack("!Q", c["in_oct"])                # ifInOctets
    if_counters += struct.pack("!I", c["in_pkt"])                # ifInUcastPkts
    if_counters += struct.pack("!I", 0)                          # ifInMulticastPkts
    if_counters += struct.pack("!I", 0)                          # ifInBroadcastPkts
    if_counters += struct.pack("!I", 0)                          # ifInDiscards
    if_counters += struct.pack("!I", 0)                          # ifInErrors
    if_counters += struct.pack("!I", 0)                          # ifInUnknownProtos
    if_counters += struct.pack("!Q", c["out_oct"])               # ifOutOctets
    if_counters += struct.pack("!I", c["out_pkt"])               # ifOutUcastPkts
    if_counters += struct.pack("!I", 0)                          # ifOutMulticastPkts
    if_counters += struct.pack("!I", 0)                          # ifOutBroadcastPkts
    if_counters += struct.pack("!I", 0)                          # ifOutDiscards
    if_counters += struct.pack("!I", 0)                          # ifOutErrors
    if_counters += struct.pack("!I", 0)                          # ifPromiscuousMode

    record = struct.pack("!II", 1, len(if_counters)) + if_counters

    body  = struct.pack("!I", random.randint(1, 1_000_000))      # counter sample seq
    body += struct.pack("!I", (0 << 24) | iface["ifindex"])      # source_id
    body += struct.pack("!I", 1)                                 # num records
    body += record

    # Tag 2 = counters_sample
    return struct.pack("!II", 2, len(body)) + body


def send_sflow_datagram(sock, target_addr, device, samples):
    """Wrap a list of sample bytes into an sFlow v5 datagram and send it."""
    device.seq += 1
    hdr  = struct.pack("!I", 5)
    hdr += struct.pack("!I", 1)                              # ip version v4
    hdr += socket.inet_aton(device.agent_ip)
    hdr += struct.pack("!I", 0)                              # sub agent id
    hdr += struct.pack("!I", device.seq)                     # datagram seq
    hdr += struct.pack("!I", int(time.time() * 1000) & 0xFFFFFFFF)
    hdr += struct.pack("!I", len(samples))                   # num samples
    pkt = hdr + b"".join(samples)
    sock.sendto(pkt, target_addr)


# ---------------------------------------------------------------------------
# NetFlow v9 packet builder
# ---------------------------------------------------------------------------

# Template id 256 with these fields:
#   8=src_ip(4), 12=dst_ip(4), 7=src_port(2), 11=dst_port(2),
#   4=proto(1), 1=bytes(4), 2=packets(4), 10=in_if(2), 14=out_if(2),
#   6=tcp_flags(1)
NF9_FIELDS = [(8,4),(12,4),(7,2),(11,2),(4,1),(1,4),(2,4),(10,2),(14,2),(6,1)]
NF9_REC_SIZE = sum(l for _, l in NF9_FIELDS)
NF9_TEMPLATE_ID = 256
NF9_SOURCE_ID = 0xCAFE0001


def nf9_template_flowset():
    body = struct.pack("!HH", NF9_TEMPLATE_ID, len(NF9_FIELDS))
    for ftype, flen in NF9_FIELDS:
        body += struct.pack("!HH", ftype, flen)
    # Pad to 4-byte boundary
    while (len(body) + 4) % 4: body += b"\x00"
    return struct.pack("!HH", 0, 4 + len(body)) + body


def nf9_data_record(src, dst, sport, dport, proto, nbytes, npkts, in_if, out_if):
    return (
        socket.inet_aton(src) + socket.inet_aton(dst)
        + struct.pack("!HH", sport, dport)
        + struct.pack("!B", proto)
        + struct.pack("!I", nbytes)
        + struct.pack("!I", npkts)
        + struct.pack("!HH", in_if, out_if)
        + struct.pack("!B", 0x18)   # TCP flags ACK+PSH
    )


def send_netflow_v9(sock, target_addr, device, records, send_template=False):
    """records is a list of pre-built data record bytes."""
    device.seq += 1
    flowsets = b""
    flowset_count = 0
    if send_template:
        flowsets += nf9_template_flowset()
        flowset_count += 1
    if records:
        data_body = b"".join(records)
        # pad to 4-byte boundary
        while (len(data_body) + 4) % 4:
            data_body += b"\x00"
        flowsets += struct.pack("!HH", NF9_TEMPLATE_ID, 4 + len(data_body)) + data_body
        flowset_count += 1
    if flowset_count == 0:
        return

    # NetFlow v9 'count' is total records (template + flow)
    rec_count = (1 if send_template else 0) + len(records)
    hdr = struct.pack("!HHIIII",
        9, rec_count,
        int(time.time() * 1000) & 0xFFFFFFFF,   # uptime
        int(time.time()),                        # unix_secs
        device.seq,
        NF9_SOURCE_ID,
    )
    sock.sendto(hdr + flowsets, target_addr)


# ---------------------------------------------------------------------------
# Main loop
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--target",        default="127.0.0.1")
    parser.add_argument("--sflow-port",    type=int, default=6343)
    parser.add_argument("--netflow-port",  type=int, default=2055)
    parser.add_argument("--duration",      type=int, default=300, help="seconds to run, 0 = forever")
    parser.add_argument("--rate",          type=float, default=20, help="flows per second")
    args = parser.parse_args()

    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sf_addr = (args.target, args.sflow_port)
    nf_addr = (args.target, args.netflow_port)

    # Pre-send NetFlow v9 templates so the collector can decode data flowsets
    # right away. Real switches re-send templates periodically; we'll do every
    # 30 seconds.
    for d in DEVICES:
        if d.kind == "netflow":
            send_netflow_v9(sock, nf_addr, d, [], send_template=True)
    last_template = time.time()

    print(f"Sending synthetic flows to "
          f"sflow={sf_addr}, netflow={nf_addr} at ~{args.rate}/s "
          f"for {args.duration or 'forever'}s")

    start = time.time()
    next_flow = start
    next_counter_dump = start + 5     # send sFlow counter samples every 5s
    flow_interval = 1.0 / args.rate
    flow_count = 0

    # Designate one persistent "noisy talker" so the top-talker view is
    # interesting (a server constantly hammering an external destination).
    noisy_src = "10.20.30.42"
    noisy_dst = "52.85.132.10"

    while True:
        now = time.time()
        if args.duration and now - start > args.duration:
            break

        # Periodic re-send of NetFlow v9 templates
        if now - last_template > 30:
            for d in DEVICES:
                if d.kind == "netflow":
                    send_netflow_v9(sock, nf_addr, d, [], send_template=True)
            last_template = now

        # Send a flow if it's time
        if now >= next_flow:
            next_flow += flow_interval

            device = random.choices(DEVICES, weights=[5, 3, 2])[0]
            # pick two interfaces (in != out)
            if_in, if_out = random.sample(device.interfaces, 2)

            # 8% of the time, generate the noisy big-transfer flow
            if random.random() < 0.08:
                src, dst = noisy_src, noisy_dst
                sport, dport, proto = random.randint(40000, 60000), 443, 6
                nbytes = random.randint(800_000, 4_000_000)
                npkts  = random.randint(600, 3000)
                desc   = "Bulk upload (noisy talker)"
            else:
                profile = weighted_pick()
                src, dst, sport, dport, proto, nbytes, npkts, desc = pick_endpoints(profile)

            # Update interface counters: in on if_in, out on if_out
            device.bump_counters(if_in["ifindex"],  nbytes, npkts, 0, 0)
            device.bump_counters(if_out["ifindex"], 0, 0, nbytes, npkts)

            if device.kind == "sflow":
                sample = build_sflow_flow_sample(
                    device, if_in["ifindex"], if_out["ifindex"],
                    src, dst, sport, dport, proto, nbytes, npkts)
                send_sflow_datagram(sock, sf_addr, device, [sample])
            else:
                rec = nf9_data_record(src, dst, sport, dport, proto,
                                       nbytes, npkts,
                                       if_in["ifindex"], if_out["ifindex"])
                send_netflow_v9(sock, nf_addr, device, [rec])

            flow_count += 1
            if flow_count % 50 == 0:
                print(f"  [{int(now-start):4d}s] {flow_count:5d} flows sent "
                      f"(latest: {desc[:30]:30s} {src} → {dst}:{dport})")

        # Send sFlow counter samples for all interfaces every 5 seconds
        if now >= next_counter_dump:
            next_counter_dump = now + 5
            for d in DEVICES:
                if d.kind != "sflow":
                    continue
                # Fan out one datagram per device with all interfaces' counters
                samples = [build_sflow_counter_sample(d, i) for i in d.interfaces]
                # Split into chunks of 5 to keep datagrams small
                for i in range(0, len(samples), 5):
                    send_sflow_datagram(sock, sf_addr, d, samples[i:i+5])

        # Sleep just enough to keep the loop responsive
        time.sleep(max(0, min(0.05, next_flow - time.time())))

    print(f"\nDone. Sent {flow_count} flow records.")


if __name__ == "__main__":
    main()
