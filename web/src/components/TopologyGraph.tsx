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

import {
  api,
  type Neighbor,
  type TopologyEdge as ApiTopologyEdge,
  type TopologyNode as ApiTopologyNode,
} from '../api'
import { useTheme } from '../theme'

// Style constants — keep dagre-layout dimensions in lockstep with
// what the custom node renders, otherwise edges hit the wrong side.
const NODE_WIDTH = 200
const NODE_HEIGHT = 64

// Scope selector — narrows the rendered subgraph. Default is
// 'device' when the operator has selected an exporter on the left
// rail (the Devices → Neighbors call site), otherwise 'fleet' (the
// standalone Topology view). The choice is persisted in localStorage
// so a return visit lands on the last-used scope; selection-derived
// defaults only apply when no saved value exists.
export type TopologyScope = 'device' | 'site' | 'fleet'
const SCOPE_STORAGE_KEY = 'flowscope.topology.scope'

function loadScope(): TopologyScope | null {
  try {
    const raw = localStorage.getItem(SCOPE_STORAGE_KEY)
    if (raw === 'device' || raw === 'site' || raw === 'fleet') return raw
    return null
  } catch {
    return null
  }
}

function saveScope(s: TopologyScope) {
  try {
    localStorage.setItem(SCOPE_STORAGE_KEY, s)
  } catch {
    /* ignore — private mode etc. */
  }
}

// ScopeResult is what applyScope returns. `effectiveScope` differs
// from the requested scope only when 'site' falls back to 'device'
// because the selected node has no sys_location. The UI surfaces an
// inline notice when that happens.
export type ScopeResult = {
  nodes: ApiTopologyNode[]
  edges: ApiTopologyEdge[]
  effectiveScope: TopologyScope
  siteFallback: boolean
}

// applyScope is the pure filter used before the dagre layout pass.
// Layout costs scale with node + edge count, so filtering first means
// dagre doesn't waste effort on hidden nodes and the resulting bounds
// match what the user actually sees.
//
// 'fleet'  → pass-through.
// 'device' → ego graph: {selected} ∪ {every neighbor sharing an edge
//            with selected}; edges = edges touching selected.
// 'site'   → derive siteLocation from the selected node's
//            sys_location. If empty, fall back to 'device' so the
//            Uncategorized bucket doesn't get rendered as one giant
//            site. Otherwise keep nodes with matching sys_location
//            and edges whose endpoints both survive the filter.
//
// Exported for unit tests.
export function applyScope(
  allNodes: ApiTopologyNode[],
  allEdges: ApiTopologyEdge[],
  selectedExporter: string | null,
  scope: TopologyScope,
): ScopeResult {
  if (scope === 'fleet' || selectedExporter == null) {
    return { nodes: allNodes, edges: allEdges, effectiveScope: 'fleet', siteFallback: false }
  }

  if (scope === 'device') {
    return egoGraph(allNodes, allEdges, selectedExporter)
  }

  // scope === 'site'
  const selected = allNodes.find((n) => n.id === selectedExporter)
  const siteLocation = selected?.sys_location ?? ''
  if (siteLocation === '') {
    // No site to filter by — fall back to ego graph and let the UI
    // surface a small notice explaining why.
    const ego = egoGraph(allNodes, allEdges, selectedExporter)
    return { ...ego, effectiveScope: 'device', siteFallback: true }
  }

  const keep = new Set(
    allNodes.filter((n) => n.sys_location === siteLocation).map((n) => n.id),
  )
  const nodes = allNodes.filter((n) => keep.has(n.id))
  const edges = allEdges.filter((e) => keep.has(e.source) && keep.has(e.target))
  return { nodes, edges, effectiveScope: 'site', siteFallback: false }
}

function egoGraph(
  allNodes: ApiTopologyNode[],
  allEdges: ApiTopologyEdge[],
  selectedExporter: string,
): ScopeResult {
  const keep = new Set<string>([selectedExporter])
  for (const e of allEdges) {
    if (e.source === selectedExporter) keep.add(e.target)
    else if (e.target === selectedExporter) keep.add(e.source)
  }
  const nodes = allNodes.filter((n) => keep.has(n.id))
  const edges = allEdges.filter(
    (e) => e.source === selectedExporter || e.target === selectedExporter,
  )
  return { nodes, edges, effectiveScope: 'device', siteFallback: false }
}

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

  // Scope state. Default = saved-in-localStorage OR a
  // selection-derived guess. Render-on-state-change: setScope below
  // updates synchronously; the next render computes the filtered
  // subset and re-runs dagre against that smaller graph.
  const [scope, setScope] = useState<TopologyScope>(() => {
    const saved = loadScope()
    if (saved) return saved
    return selectedExporter ? 'device' : 'fleet'
  })

  // When the operator clears the selection (selectedExporter → null),
  // the 'device' and 'site' scopes are no longer meaningful — drop
  // back to 'fleet' so the canvas isn't filtered against a stale ID.
  // When they pick a new device the prior scope sticks (per the
  // PR brief: device/site stays, fleet stays).
  useEffect(() => {
    if (selectedExporter == null && (scope === 'device' || scope === 'site')) {
      setScope('fleet')
    }
  }, [selectedExporter, scope])

  // Persist scope on every change. Cheap; the localStorage write is
  // wrapped to tolerate private-mode failures.
  useEffect(() => {
    saveScope(scope)
  }, [scope])

  // Filter the API response down to the rendered subset BEFORE
  // dagre. Layout only sees nodes that will actually paint, so its
  // bounds match the visible graph and an ego-graph render is cheap.
  const scoped = useMemo<ScopeResult>(() => {
    if (!topo.data) {
      return { nodes: [], edges: [], effectiveScope: scope, siteFallback: false }
    }
    return applyScope(topo.data.nodes, topo.data.edges, selectedExporter, scope)
  }, [topo.data, selectedExporter, scope])

  // dagreLayout is pure-of-inputs, so memoise on the scoped result +
  // selected node. Re-running on every render would re-layout on
  // every prop change and snap manually-nudged nodes back to the
  // dagre coords.
  const layouted = useMemo(() => {
    if (!topo.data) return { nodes: [] as Node[], edges: [] as Edge[] }
    const nodes: Node[] = scoped.nodes.map((n) => ({
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
    const edges: Edge[] = scoped.edges.map((e) => ({
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
  }, [topo.data, scoped.nodes, scoped.edges, selectedExporter])

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
  // Camera placement:
  //   device / site: centre on the selected node so it's visually the
  //                  focus (matches the Devices → Neighbors behaviour
  //                  PR #52 added). If the selected node was filtered
  //                  out of the visible set, noop instead of crashing
  //                  setCenter on a missing position.
  //   fleet:         fitView the whole graph (the standalone view).
  useEffect(() => {
    if (layouted.nodes.length === 0) return
    const id = window.setTimeout(() => {
      if (
        selectedExporter &&
        (scoped.effectiveScope === 'device' || scoped.effectiveScope === 'site')
      ) {
        const focus = layouted.nodes.find((n) => n.id === selectedExporter)
        if (focus) {
          const cx = focus.position.x + NODE_WIDTH / 2
          const cy = focus.position.y + NODE_HEIGHT / 2
          flow.setCenter(cx, cy, { zoom: 0.85, duration: 400 })
          return
        }
        // Selected device isn't on the canvas (e.g. the scope filter
        // ate it). Don't crash setCenter on undefined — fall through
        // to fitView and log once for diagnosis.
        // eslint-disable-next-line no-console
        console.warn(
          `[topology] selected device ${selectedExporter} not in scoped subgraph; fitView fallback`,
        )
      }
      flow.fitView({ padding: 0.2 })
    }, 0)
    return () => window.clearTimeout(id)
  }, [flow, layouted.nodes, selectedExporter, scoped.effectiveScope])

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
    return (
      <div data-testid="topology-canvas">
        <EmptyState />
      </div>
    )
  }

  // After scope filtering the visible set may be empty (e.g. an
  // isolated device with no neighbors picked in 'device' scope). Show
  // the same empty state the no-data path uses — the user still has
  // the scope toggle above it so they can climb back out to fleet.
  const filteredEmpty = layouted.nodes.length === 0

  return (
    <div className="relative" data-testid="topology-canvas">
      <ScopeToggle
        scope={scope}
        onChange={setScope}
        selectionEnabled={selectedExporter != null}
      />
      {scoped.siteFallback && (
        <div
          className="font-mono text-[10.5px] text-faint border-t border-x border-line bg-surface px-3 py-1.5"
          data-testid="topology-scope-site-fallback"
        >
          no sys_location set on selected device; showing connected devices only
        </div>
      )}
      {filteredEmpty ? (
        // PR brief: reuse the existing empty-state copy when the
        // scope filter eats all the nodes; the toggle above this
        // notice is still operable so the operator can climb back
        // out to fleet.
        <EmptyState />
      ) : (
        <div className="h-[640px] border border-line">
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
      )}
    </div>
  )
}

// ScopeToggle is the three-segment selector above the canvas. Matches
// the surrounding minor-control style: font-mono uppercase 10–11px,
// border-line outline, accent fill on the active segment. The
// device/site segments are disabled until a device is selected on
// the left rail (the standalone Topology view has no selection and
// only the fleet segment is meaningful).
function ScopeToggle({
  scope,
  onChange,
  selectionEnabled,
}: {
  scope: TopologyScope
  onChange: (s: TopologyScope) => void
  selectionEnabled: boolean
}) {
  return (
    <div
      data-testid="topology-scope-toggle"
      className="flex items-center gap-px border border-line bg-surface mb-2 w-fit font-mono uppercase text-[10.5px] tracking-[0.1em]"
    >
      <ScopeSegment
        scope="device"
        current={scope}
        disabled={!selectionEnabled}
        onChange={onChange}
        disabledTitle="select a device first"
      />
      <ScopeSegment
        scope="site"
        current={scope}
        disabled={!selectionEnabled}
        onChange={onChange}
        disabledTitle="select a device first"
      />
      <ScopeSegment scope="fleet" current={scope} disabled={false} onChange={onChange} />
    </div>
  )
}

function ScopeSegment({
  scope,
  current,
  disabled,
  onChange,
  disabledTitle,
}: {
  scope: TopologyScope
  current: TopologyScope
  disabled: boolean
  onChange: (s: TopologyScope) => void
  disabledTitle?: string
}) {
  const active = current === scope
  // Active = accent border ring + accent text. Disabled = faint text,
  // no hover. Idle = dim text on bg-surface, hover bumps to accent.
  const cls = active
    ? 'px-2.5 py-1 border-accent text-accent bg-surface'
    : disabled
      ? 'px-2.5 py-1 text-faint cursor-not-allowed'
      : 'px-2.5 py-1 text-dim hover:text-accent hover:bg-hover'
  return (
    <button
      type="button"
      data-testid={`topology-scope-${scope}`}
      aria-pressed={active}
      aria-disabled={disabled || undefined}
      title={disabled ? disabledTitle : undefined}
      disabled={disabled}
      onClick={() => {
        if (disabled || active) return
        // Render-on-state-change: scope state flips synchronously
        // here, the next render re-runs applyScope + dagre against
        // the new scope. No fetch involved.
        onChange(scope)
      }}
      className={cls}
    >
      {scope}
    </button>
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
  // EmptyState renders without its own data-testid because callers
  // wrap it inside the outer 'topology-canvas' container. Two
  // elements with the same testid trips Playwright's strict mode.
  return (
    <div className="p-8 border border-dashed border-line">
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
