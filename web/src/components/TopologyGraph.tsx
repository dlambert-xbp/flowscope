// TopologyGraph renders the fleet-wide LLDP/CDP adjacency map. The
// API returns react-flow-ready nodes and edges; this component wraps
// them in @xyflow/react with a dagre auto-layout pass so the operator
// sees a sensible top-down tree on first paint and can drag-nudge
// from there.
//
// VISION.md §3.1 — SNMP is reserved for inventory + topology; the
// 5-minute walker cadence in cmd/snmp keeps load light. This view
// reads from /api/topology, which is server-cached for 30s.
import { useQuery } from '@tanstack/react-query'
import { useEffect, useMemo, useState } from 'react'
import {
  Background,
  Controls,
  Handle,
  Position,
  ReactFlow,
  ReactFlowProvider,
  useReactFlow,
  type Edge,
  type Node,
  type NodeMouseHandler,
} from '@xyflow/react'
import Dagre from '@dagrejs/dagre'
import '@xyflow/react/dist/style.css'

import { api, type Neighbor } from '../api'
import { useTheme } from '../theme'

// Style constants — keep dagre-layout dimensions in lockstep with
// what the custom node renders, otherwise edges hit the wrong side.
const NODE_WIDTH = 200
const NODE_HEIGHT = 64

// Capability → icon glyph. Tiny svgs would be nicer; for V1 a single
// emoji glyph per role keeps the bundle small and the column easy to
// scan. Order matters: first match wins so a multi-role device
// (bridge+router) still shows a single primary icon.
function primaryGlyph(caps: string[]): string {
  if (caps.includes('router')) return '⌬'
  if (caps.includes('wlan-ap')) return '⌒'
  if (caps.includes('bridge')) return '⊞'
  if (caps.includes('telephone')) return '☎'
  if (caps.includes('host')) return '◦'
  return '⬡'
}

// Capability → tooltip helper text.
function capLabel(caps: string[]): string {
  if (caps.length === 0) return 'unknown role'
  return caps.join(' · ')
}

// DeviceNode is the custom react-flow node renderer. Shows the
// device hostname (or IP fallback), a capability glyph on the left,
// and a small last-seen timestamp footer. Greyed-out tone marks
// devices we haven't seen flows from in 5+ minutes.
function DeviceNode({
  data,
}: {
  data: {
    label: string
    address: string
    caps: string[]
    discovered: boolean
    reachable: boolean
    selected: boolean
  }
}) {
  // Reachability + provenance drive the border + tone:
  //   selected: accent border, full text
  //   discovered (no inventory): dashed border + dim text — "saw this
  //                              chassis in someone's TLV but didn't
  //                              walk it"
  //   unreachable: dim text — "we walked it, but flows have gone silent"
  //   reachable: solid border, normal text
  const borderClass = data.selected
    ? 'border-accent'
    : data.discovered
      ? 'border-line border-dashed'
      : data.reachable
        ? 'border-line'
        : 'border-line opacity-60'
  return (
    <div
      className={`bg-surface border ${borderClass} px-3 py-2 font-mono text-[12px] min-w-0 max-w-[200px]`}
      style={{ width: NODE_WIDTH }}
    >
      {/* react-flow needs source + target handles for edges to land. */}
      <Handle type="target" position={Position.Top} className="opacity-0" />
      <Handle type="source" position={Position.Bottom} className="opacity-0" />
      <div className="flex items-center gap-2">
        <span
          aria-label={capLabel(data.caps)}
          title={capLabel(data.caps)}
          className="text-[14px] text-accent"
        >
          {primaryGlyph(data.caps)}
        </span>
        <span className="truncate" title={data.label}>
          {data.label}
        </span>
      </div>
      {data.address && data.address !== data.label && (
        <div className="text-[10.5px] text-faint truncate mt-0.5" title={data.address}>
          {data.address}
        </div>
      )}
      {data.discovered && (
        <div className="text-[9.5px] uppercase tracking-[0.1em] text-faint mt-0.5">
          discovered only
        </div>
      )}
    </div>
  )
}

const nodeTypes = { device: DeviceNode }

// dagreLayout runs the dagre top-down layout. Returns the same nodes
// + edges with x/y populated. Dagre is the right call for L2
// topology graphs: most adjacencies form clean hierarchical trees
// (core → distribution → access), and the operator can still drag
// individual nodes to tweak after the layout pass.
function dagreLayout(nodes: Node[], edges: Edge[]): Node[] {
  const g = new Dagre.graphlib.Graph()
  g.setDefaultEdgeLabel(() => ({}))
  g.setGraph({ rankdir: 'TB', nodesep: 40, ranksep: 80, marginx: 24, marginy: 24 })
  for (const n of nodes) {
    g.setNode(n.id, { width: NODE_WIDTH, height: NODE_HEIGHT })
  }
  for (const e of edges) {
    g.setEdge(e.source, e.target)
  }
  Dagre.layout(g)
  return nodes.map((n) => {
    const pos = g.node(n.id)
    return {
      ...n,
      position: { x: pos.x - NODE_WIDTH / 2, y: pos.y - NODE_HEIGHT / 2 },
      // Hand the layout a stable size so react-flow's bounds match.
      width: NODE_WIDTH,
      height: NODE_HEIGHT,
    }
  })
}

export type TopologyGraphProps = {
  // selectedExporter is highlighted on the graph when set. Click on
  // any node fires onSelectExporter so the host shell can switch to
  // that device's Summary tab (the existing per-device URL state).
  selectedExporter: string | null
  onSelectExporter: (exporter: string) => void
}

export function TopologyGraph(props: TopologyGraphProps) {
  return (
    <ReactFlowProvider>
      <TopologyGraphInner {...props} />
    </ReactFlowProvider>
  )
}

function TopologyGraphInner({ selectedExporter, onSelectExporter }: TopologyGraphProps) {
  const topo = useQuery({
    queryKey: ['topology'],
    queryFn: () => api.topology(),
    // Server caches for 30s so a longer client refetch is fine; this
    // mostly bumps the dashboard back to live data after a focused
    // walk completes.
    refetchInterval: 60_000,
  })

  // dagreLayout is pure-of-inputs, so memoise on the topology +
  // selected node. Re-running on every render would re-layout on
  // every prop change and snap manually-nudged nodes back to the
  // dagre coords.
  const layouted = useMemo(() => {
    if (!topo.data) return { nodes: [] as Node[], edges: [] as Edge[] }
    const nodes: Node[] = topo.data.nodes.map((n) => ({
      id: n.id,
      type: 'device',
      data: {
        label: n.label,
        address: n.address,
        caps: n.capabilities ?? [],
        discovered: n.discovered,
        reachable: n.reachable,
        selected: !!selectedExporter && n.id === selectedExporter,
      },
      position: { x: 0, y: 0 },
    }))
    const edges: Edge[] = topo.data.edges.map((e) => ({
      id: e.id,
      source: e.source,
      target: e.target,
      // Edge label shows the local interface name; the remote port
      // is in the tooltip via aria so the visual stays clean at zoom.
      label: e.source_port && e.target_port
        ? `${e.source_port} ↔ ${e.target_port}`
        : e.source_port || e.target_port,
      data: { proto: e.discovery_proto },
      style: {
        stroke: e.discovery_proto === 'cdp' ? 'var(--color-warn, #d97706)' : 'var(--color-accent, #6366f1)',
        strokeWidth: 1.5,
      },
      labelStyle: { fontSize: 10, fontFamily: 'monospace' },
      labelBgStyle: { fill: 'var(--color-surface, #fff)' },
    }))
    return { nodes: dagreLayout(nodes, edges), edges }
  }, [topo.data, selectedExporter])

  // Local nodes state lets the operator drag-nudge after the dagre
  // pass without losing their layout on the next refetch (we only
  // re-layout when the underlying topology shape changes, not when
  // the user moves a node).
  const [nodes, setNodes] = useState<Node[]>([])
  useEffect(() => {
    setNodes(layouted.nodes)
  }, [layouted.nodes])

  const flow = useReactFlow()
  const { resolved: theme } = useTheme()
  // When a device is selected (Devices → Neighbors), centre the viewport
  // on that node so it's visually the focus. Otherwise fitView the whole
  // graph (the standalone topology view).
  useEffect(() => {
    if (layouted.nodes.length === 0) return
    const id = window.setTimeout(() => {
      if (selectedExporter) {
        const focus = layouted.nodes.find((n) => n.id === selectedExporter)
        if (focus) {
          const cx = focus.position.x + NODE_WIDTH / 2
          const cy = focus.position.y + NODE_HEIGHT / 2
          flow.setCenter(cx, cy, { zoom: 0.85, duration: 400 })
          return
        }
      }
      flow.fitView({ padding: 0.2 })
    }, 0)
    return () => window.clearTimeout(id)
  }, [flow, layouted.nodes, selectedExporter])

  const handleNodeClick: NodeMouseHandler = (_, n) => {
    // Synchronously inform the host (Devices.tsx) BEFORE any async
    // refetch — render-on-state-change. The host then updates the
    // URL device param + selected state; this component re-renders
    // with the new highlight on the next layout pass.
    onSelectExporter(n.id)
  }

  if (topo.isLoading) {
    return (
      <div data-testid="topology-canvas" className="p-8 text-dim font-mono text-[13px]">loading topology…</div>
    )
  }
  if (topo.isError) {
    return (
      <div data-testid="topology-canvas" className="p-8 text-crit font-mono text-[13px]">
        failed to load topology: {(topo.error as Error).message}
      </div>
    )
  }
  if (!topo.data || topo.data.nodes.length === 0) {
    return <EmptyState />
  }

  return (
    <div className="h-[640px] border border-line" data-testid="topology-canvas">
      <ReactFlow
        colorMode={theme}
        nodes={nodes}
        edges={layouted.edges}
        nodeTypes={nodeTypes}
        onNodesChange={(changes) => {
          // Apply position + selection drags. We don't need add /
          // remove handling because the topology data is read-only —
          // adding a device on the wire happens upstream via SNMP.
          setNodes((prev) => {
            const next = [...prev]
            for (const ch of changes) {
              if (ch.type === 'position' && 'position' in ch && ch.position) {
                const i = next.findIndex((n) => n.id === ch.id)
                if (i >= 0) {
                  next[i] = { ...next[i], position: ch.position }
                }
              }
            }
            return next
          })
        }}
        onNodeClick={handleNodeClick}
        fitView
        proOptions={{ hideAttribution: true }}
      >
        <Background gap={20} />
        <Controls showInteractive={false} />
      </ReactFlow>
      <GraphLegend />
    </div>
  )
}

function GraphLegend() {
  return (
    <div className="absolute bottom-2 right-2 flex items-center gap-3 px-2 py-1 bg-surface border border-line font-mono text-[10px] text-dim">
      <span>
        <span className="inline-block w-4 h-px align-middle mr-1" style={{ background: 'var(--color-accent, #6366f1)' }} />
        LLDP
      </span>
      <span>
        <span className="inline-block w-4 h-px align-middle mr-1" style={{ background: 'var(--color-warn, #d97706)' }} />
        CDP
      </span>
      <span>⌬ router · ⊞ bridge · ⌒ AP · ◦ host</span>
    </div>
  )
}

function EmptyState() {
  return (
    <div className="p-8 border border-dashed border-line" data-testid="topology-canvas">
      <div className="font-mono text-[14px] text-text mb-2">No LLDP/CDP neighbors discovered yet.</div>
      <div className="font-mono text-[12px] text-dim leading-[1.5]">
        SNMP credentials must be configured on devices before the snmp
        service can walk LLDP-MIB / CISCO-CDP-MIB. The walker runs
        every 5 minutes once a device is bound.
      </div>
      <div className="font-mono text-[12px] text-dim mt-2">
        <a className="text-accent hover:underline" href="#settings">
          Open Settings → SNMP →
        </a>
      </div>
    </div>
  )
}

// NeighborsTable renders the per-device adjacency list — same source
// of truth as the graph but in a row layout that's easier to scan
// for "what's plugged into Te1/0/24". The Neighbors sub-tab shows
// this directly under the graph when an exporter is selected.
export function NeighborsTable({ exporter }: { exporter: string }) {
  const q = useQuery({
    queryKey: ['neighbors', exporter],
    queryFn: () => api.deviceNeighbors(exporter),
    refetchInterval: 60_000,
  })
  if (q.isLoading) {
    return <div className="font-mono text-[12px] text-dim p-4">loading neighbors…</div>
  }
  if (q.isError) {
    return (
      <div className="font-mono text-[12px] text-crit p-4">
        failed to load neighbors: {(q.error as Error).message}
      </div>
    )
  }
  const rows = q.data?.neighbors ?? []
  if (rows.length === 0) {
    return (
      <div className="font-mono text-[12px] text-dim p-4">
        no neighbors observed on this device · LLDP/CDP may not be
        enabled, or the snmp walker hasn't run yet (5-min cadence)
      </div>
    )
  }
  return (
    <table className="w-full text-[12px] font-mono border-t border-line">
      <thead className="text-[10.5px] uppercase tracking-[0.1em] text-faint">
        <tr className="border-b border-line bg-surface">
          <Th>local port</Th>
          <Th>via</Th>
          <Th>remote chassis</Th>
          <Th>remote sys_name</Th>
          <Th>remote port</Th>
          <Th>capabilities</Th>
          <Th>mgmt addr</Th>
          <Th>last seen</Th>
        </tr>
      </thead>
      <tbody>
        {rows.map((n) => (
          <NeighborRow key={`${n.local_ifindex}-${n.discovery_proto}-${n.remote_chassis_id}-${n.remote_port_id}`} n={n} />
        ))}
      </tbody>
    </table>
  )
}

function Th({ children }: { children: React.ReactNode }) {
  return <th className="text-left px-3 py-2">{children}</th>
}

function NeighborRow({ n }: { n: Neighbor }) {
  return (
    <tr className="border-b border-line-soft hover:bg-hover">
      <td className="px-3 py-1.5 truncate">
        {n.local_port_name || `ifindex ${n.local_ifindex}`}
      </td>
      <td className="px-3 py-1.5">
        <span
          className={`px-1.5 py-0.5 border text-[10px] uppercase tracking-[0.08em] ${
            n.discovery_proto === 'lldp' ? 'border-accent text-accent' : 'border-warn text-warn'
          }`}
        >
          {n.discovery_proto}
        </span>
      </td>
      <td className="px-3 py-1.5 truncate" title={n.remote_chassis_id}>
        {n.remote_chassis_id}
      </td>
      <td className="px-3 py-1.5 truncate" title={n.remote_sys_desc}>
        {n.remote_sys_name || '—'}
      </td>
      <td className="px-3 py-1.5 truncate">{n.remote_port_id || '—'}</td>
      <td className="px-3 py-1.5 truncate text-faint">{n.remote_capabilities || '—'}</td>
      <td className="px-3 py-1.5 truncate text-faint">{n.remote_management_addr || '—'}</td>
      <td className="px-3 py-1.5 text-faint" title={n.first_seen}>
        {n.last_seen.slice(11, 19)}Z
      </td>
    </tr>
  )
}
