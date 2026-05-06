# FlowScope — NetFlow / sFlow collector & dashboard

A self-contained traffic-flow visualization system. It receives NetFlow v5,
NetFlow v9, IPFIX, and sFlow v5 from any standard exporter (Arista, Cisco,
Juniper, MikroTik, Linux `softflowd`, etc.) and shows you:

- All exporting devices that have spoken to it
- Every interface (by ifIndex) seen on each device, with **ingress and egress**
  byte/packet counters
- Live per-flow records (5-tuple, in/out interfaces, bytes, packets)
- Top talkers, top destination ports, protocol breakdown
- A 3-minute live timeseries chart of total throughput

Single file backend, single file frontend. No external dashboard service. No
agents. Runs on Python 3.9+.

---

## Quick start

```bash
cd flowscope
pip install -r requirements.txt
python app.py
```

That's it. Open `http://<host>:8080`.

By default the collector listens on:

| Protocol | UDP port |
|----------|----------|
| NetFlow v5/v9, IPFIX | **2055** |
| sFlow v5 | **6343** |
| Web dashboard | **8080** (TCP) |

Override any of them:

```bash
python app.py --netflow-port 9995 --sflow-port 6343 --web-port 8080
```

If you bind to a port < 1024 (e.g. the standard sFlow 6343 is fine but some
people prefer 162 etc.) you'll need root or `setcap`:

```bash
sudo setcap cap_net_bind_service=+ep $(which python3)
```

---

## Configuring an Arista switch

Replace `<collector-ip>` with the IP of the host running FlowScope.

### sFlow (recommended for Arista — hardware accelerated, samples real packets)

```
configure
sflow source-interface Management1
sflow destination <collector-ip> 6343
sflow polling-interval 10
sflow sample 10000
sflow run
end
write
```

`sflow sample 10000` means roughly 1-in-10000 packets are sampled. Lower the
number to see more flows; raise it to reduce CPU/bandwidth on the switch.

`polling-interval 10` makes the switch send absolute interface byte/packet
counters every 10 seconds, which is what populates the **Interfaces** tab
with accurate per-port totals.

### NetFlow v9 / IPFIX on Arista

```
configure
flow tracking hardware
   tracker FLOWSCOPE
      record export on inactive timeout 15
      record export on interval 60
      exporter FLOWSCOPE_EXP
         collector <collector-ip> port 2055
         format ipfix version 10
         template interval 5000
   no shutdown
!
interface Ethernet1
   flow tracker hardware FLOWSCOPE
interface Ethernet2
   flow tracker hardware FLOWSCOPE
end
write
```

Enable `flow tracker hardware FLOWSCOPE` on every interface you want to
monitor.

### Cisco IOS / IOS-XE — NetFlow v9

```
flow exporter FLOWSCOPE
 destination <collector-ip>
 transport udp 2055
 template data timeout 60
!
flow monitor FM
 exporter FLOWSCOPE
 record netflow ipv4 original-input
!
interface GigabitEthernet0/1
 ip flow monitor FM input
 ip flow monitor FM output
```

### Linux test source (no switch needed)

```bash
sudo apt install softflowd
sudo softflowd -i eth0 -n <collector-ip>:2055 -v 9
```

### Synthetic data generator (no network needed at all)

The repo includes `synth_flows.py`, which simulates two Arista switches and
one router sending realistic-looking flows so you can exercise every view
without any real hardware:

```bash
# In one terminal:
python app.py

# In another:
python synth_flows.py --target 127.0.0.1 --duration 300 --rate 30
```

It will:

- Send sFlow v5 from two simulated switches (`10.10.1.1`, `10.10.1.2`) with
  flow samples and 5-second counter samples for each interface
- Send NetFlow v9 from a simulated router (`127.0.0.1`) with templates
  re-sent every 30 seconds
- Generate a realistic mix of HTTPS/HTTP/DNS/SSH/SMB/RDP/SNMP/NTP/BGP traffic
- Designate one persistent "noisy talker" doing big transfers so the top
  talkers view is interesting
- Maintain monotonically-increasing per-interface byte/packet counters so
  the Interfaces tab shows authoritative absolute totals

Useful for demos, screenshots, and verifying the collector before you wire
it up to real switches.

---

## How the views are built

- **Overview** — aggregates the most recent flows in the in-memory ring buffer
  (5000 records) into top talkers, top ports, protocol mix, and a per-second
  bytes timeseries.
- **Interfaces** — totals are accumulated from every flow's `input_if` /
  `output_if` field. With sFlow, when the switch sends *counter samples* (every
  `polling-interval` seconds), absolute counters from the device replace the
  estimates — these are authoritative.
- **Live Flows** — a streaming view of the most recent 100 flow records, with
  src/dst, ports, protocol, in/out ifIndex, byte count.
- **Filtering** — clicking a device in the sidebar filters everything to that
  exporter. Clicking an interface card jumps to the live flow view filtered
  to that ifIndex.

---

## Persistence

Flow records are also written to a local SQLite file (`flowscope.db`). Records
older than 6 hours are pruned every 5 minutes. The web UI works exclusively
from in-memory state for speed; the SQLite store is there so you can run
ad-hoc analysis from the command line:

```bash
sqlite3 flowscope.db "
  SELECT exporter, src_ip, dst_ip, sum(bytes) AS b
  FROM flows
  WHERE ts > strftime('%s','now') - 3600
  GROUP BY exporter, src_ip, dst_ip
  ORDER BY b DESC
  LIMIT 20;
"
```

---

## REST API

All endpoints return JSON. Optional `?exporter=<ip>` filter on most.

| Path | Description |
|------|-------------|
| `GET /api/summary` | Collector stats (uptime, packet/flow counts, ports). |
| `GET /api/devices` | All exporters seen, with last-seen and flow counts. |
| `GET /api/interfaces` | Per-(exporter, ifIndex) ingress/egress totals. |
| `GET /api/flows/recent?limit=200` | Most recent flow records. |
| `GET /api/top/talkers?limit=20` | Top src→dst pairs by bytes. |
| `GET /api/top/ports?limit=15` | Top destination ports by bytes. |
| `GET /api/protocols` | Bytes/flows broken out by L4 protocol. |
| `GET /api/timeseries?seconds=300` | Per-second ingress/egress byte totals. |

---

## Limitations & honest notes

- **Sampled, not exhaustive.** sFlow is statistical sampling by design; with
  a sample rate of 1/10000, byte totals shown are extrapolations from the
  recent ring buffer, not exact wire totals. The counter-sample-driven
  numbers on the Interfaces tab *are* exact (they come from the switch's
  hardware counters). NetFlow v9 records every flow, so its totals are exact
  per-flow but bucketed by the switch's active/inactive timers.
- **Interface names.** Standard NetFlow only carries `ifIndex`, not the human
  name. The dashboard shows `if<index>`. To get nice names like
  `Ethernet1/1`, an SNMP polling layer would be needed; this is intentionally
  out of scope here to keep the project to a single file.
- **In-memory state.** The 5000-flow ring buffer is enough for live viewing
  but isn't sized for forensic retention — that's what the SQLite store is
  for, and you can lift the 6-hour prune in `db_prune_loop()` if you want.
- **No authentication.** Bind it to a management network or put it behind a
  reverse proxy with auth before exposing it.
- **IPv6 in NetFlow v9.** Field IDs 27/28 are decoded; less-common fields
  (MPLS labels, BGP attributes, etc.) are skipped — they don't affect totals.

---

## Files

```
flowscope/
├── app.py             # Collector + Flask API + SQLite (single file)
├── requirements.txt
├── README.md
└── web/
    └── index.html     # Dashboard (single file)
```
