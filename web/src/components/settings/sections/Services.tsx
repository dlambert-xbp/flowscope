import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, type CustomService, type LibraryRow, type ServiceLookup } from '../../../api'
import { useAppConfirm } from '../../ui/appConfirm'
import { SectionHeader } from '../Shell'
import { Btn, Empty, Field, Section, StyleScope, Tag } from '../shared'

// Services panel: layered view of the port → service-name registry.
//
//   1. Lookup widget — instant resolve of one (proto, port) tuple,
//      hits /api/services/lookup which is the same path every other
//      part of the app uses for chip labels.
//
//   2. Library — paginated browse of the built-in dataset (nmap +
//      IANA, ~22k entries). The "*" marker on a row means the same
//      port has more than one well-known meaning.
//
//   3. Custom — operator overrides (with port ranges + groups). A
//      custom always wins; narrowest range wins among customs.

type SubTab = 'library' | 'custom'

export function Services() {
  const [tab, setTab] = useState<SubTab>('library')

  return (
    <div>
      <SectionHeader
        eyebrow="Settings · Services"
        title="Service registry"
        subtitle={
          <>
            FlowScope ships the IANA Service Names registry plus the nmap-services
            dataset (~22,000 well-known ports) so flows render with meaningful
            labels out of the box. Operators can override or extend with custom
            services — single ports or ranges, optionally tagged with a logical
            group the alert engine can reference. Custom names always win;
            narrower matches win over wider ones.
          </>
        }
      />
      <StyleScope />
      <Lookup />

      <div className="px-6 pt-4 flex items-center gap-2">
        <SubTabBtn active={tab === 'library'} onClick={() => setTab('library')}>
          Library
        </SubTabBtn>
        <SubTabBtn active={tab === 'custom'} onClick={() => setTab('custom')}>
          Custom services
        </SubTabBtn>
      </div>

      {tab === 'library' ? <Library /> : <Custom />}
    </div>
  )
}

function SubTabBtn({ active, onClick, children }: { active: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`px-3 py-1.5 text-[12.5px] border-b-2 transition-colors ${
        active ? 'border-accent text-text' : 'border-transparent text-dim hover:text-text'
      }`}
    >
      {children}
    </button>
  )
}

/* ----------------------------- Lookup ----------------------------- */

function Lookup() {
  const [proto, setProto] = useState('udp')
  const [port, setPort] = useState('4789')
  const lookup = useQuery({
    queryKey: ['service-lookup', proto, port],
    queryFn: () => api.serviceLookup(proto, Number(port)),
    enabled: Number(port) > 0 && Number(port) <= 65535,
    retry: false,
  })

  return (
    <Section eyebrow="Quick lookup" hint="resolve a (proto, port) the same way the app does">
      <div className="flex flex-wrap items-end gap-3">
        <Field label="proto">
          <select
            value={proto}
            onChange={(e) => setProto(e.target.value)}
            className="s-input s-input--narrow"
          >
            <option value="tcp">tcp</option>
            <option value="udp">udp</option>
            <option value="sctp">sctp</option>
            <option value="dccp">dccp</option>
          </select>
        </Field>
        <Field label="port">
          <input
            type="number"
            min={1}
            max={65535}
            value={port}
            onChange={(e) => setPort(e.target.value)}
            className="s-input s-input--narrow"
          />
        </Field>
        <div className="flex-1 min-w-[300px]">
          <LookupResult result={lookup.data} loading={lookup.isFetching} />
        </div>
      </div>
    </Section>
  )
}

function LookupResult({ result, loading }: { result?: ServiceLookup; loading: boolean }) {
  if (loading) return <div className="text-faint text-[12px] font-mono">resolving…</div>
  if (!result || !result.found) {
    return (
      <div className="text-dim text-[12.5px] font-mono">
        no well-known mapping · the UI shows the raw port
      </div>
    )
  }
  return (
    <div className="font-mono text-[12.5px] leading-[1.6]">
      <div className="flex items-center gap-2">
        <span className="text-text font-semibold">{result.primary.name}</span>
        <Tag tone={result.primary.source === 'custom' ? 'accent' : undefined}>
          {result.primary.source}
        </Tag>
        {result.multi && <Tag tone="warn" title="more than one known meaning">*</Tag>}
        {result.primary.group && <Tag tone="accent">{result.primary.group}</Tag>}
      </div>
      {result.primary.description && (
        <div className="text-faint mt-0.5 max-w-[60ch] truncate">{result.primary.description}</div>
      )}
      {result.alternatives && result.alternatives.length > 0 && (
        <div className="mt-1 text-faint">
          also known as:{' '}
          {result.alternatives
            .slice(0, 5)
            .map((a) => a.name)
            .join(' · ')}
          {result.alternatives.length > 5 && ` · +${result.alternatives.length - 5} more`}
        </div>
      )}
    </div>
  )
}

/* ----------------------------- Library ----------------------------- */

function Library() {
  const [q, setQ] = useState('')
  const [proto, setProto] = useState('')
  const limit = 200
  const [offset, setOffset] = useState(0)

  const lib = useQuery({
    queryKey: ['service-library', q, proto, offset],
    queryFn: () => api.serviceLibrary(q, proto, limit, offset),
    placeholderData: (prev) => prev,
  })

  const onSearch = (next: string) => {
    setOffset(0)
    setQ(next)
  }

  return (
    <Section
      eyebrow={`Library · ${lib.data?.counts.built_in ? lib.data.counts.built_in.toLocaleString() + ' entries' : 'loading…'}`}
      hint={lib.data ? `showing ${offset + 1}–${Math.min(offset + (lib.data.rows.length ?? 0), lib.data.total)} of ${lib.data.total.toLocaleString()}` : ''}
      actions={
        <>
          <select
            value={proto}
            onChange={(e) => {
              setOffset(0)
              setProto(e.target.value)
            }}
            className="s-input s-input--narrow"
          >
            <option value="">any proto</option>
            <option value="tcp">tcp</option>
            <option value="udp">udp</option>
            <option value="sctp">sctp</option>
            <option value="dccp">dccp</option>
          </select>
          <input
            placeholder="search name (e.g. vxlan, https)"
            value={q}
            onChange={(e) => onSearch(e.target.value)}
            className="s-input"
            style={{ width: 240 }}
          />
        </>
      }
    >
      <div className="border border-line">
        <table className="w-full">
          <thead>
            <tr className="border-b border-line bg-raise">
              <Th>name</Th>
              <Th>proto</Th>
              <Th className="r">port</Th>
              <Th>source</Th>
              <Th>description</Th>
            </tr>
          </thead>
          <tbody>
            {lib.data?.rows.map((r) => (
              <LibraryRow_ key={`${r.proto}-${r.port}-${r.name}`} r={r} />
            ))}
            {lib.data && lib.data.rows.length === 0 && (
              <tr>
                <td colSpan={5} className="px-3 py-4 text-center text-faint text-[12px] font-mono">
                  no matches
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>

      <div className="mt-3 flex items-center gap-2 font-mono text-[11px]">
        <Btn onClick={() => setOffset(Math.max(0, offset - limit))} disabled={offset === 0}>
          prev
        </Btn>
        <Btn
          onClick={() => setOffset(offset + limit)}
          disabled={!lib.data || offset + limit >= lib.data.total}
        >
          next
        </Btn>
        <span className="ml-auto text-faint">
          page size {limit}
        </span>
      </div>
    </Section>
  )
}

function Th({ children, className }: { children?: React.ReactNode; className?: string }) {
  return (
    <th
      className={`text-left text-[10.5px] uppercase tracking-[0.1em] text-faint font-mono font-semibold px-3 py-2 ${className === 'r' ? 'text-right' : ''}`}
    >
      {children}
    </th>
  )
}

function LibraryRow_({ r }: { r: LibraryRow }) {
  return (
    <tr className="border-b border-line/60 hover:bg-surface">
      <td className="px-3 py-1.5 text-[12.5px] text-text">{r.name}</td>
      <td className="px-3 py-1.5 text-[12.5px] font-mono text-dim uppercase">{r.proto}</td>
      <td className="px-3 py-1.5 text-[12.5px] font-mono text-text text-right tabular-nums">{r.port}</td>
      <td className="px-3 py-1.5">
        <Tag>{r.source}</Tag>
        {r.multi && (
          <span className="ml-1.5">
            <Tag tone="warn" title="more than one known meaning">*</Tag>
          </span>
        )}
      </td>
      <td className="px-3 py-1.5 text-[12px] text-faint truncate max-w-[40ch]">
        {r.description ?? '—'}
      </td>
    </tr>
  )
}

/* ----------------------------- Custom ----------------------------- */

function Custom() {
  const list = useQuery({
    queryKey: ['custom-services'],
    queryFn: () => api.listCustomServices(),
  })
  const [editing, setEditing] = useState<CustomService | 'new' | null>(null)

  return (
    <Section
      eyebrow={`Custom services · ${list.data?.count ?? 0}`}
      hint="overlay on top of the built-in registry · narrowest match wins"
      actions={
        <Btn tone="accent" onClick={() => setEditing('new')}>
          + new
        </Btn>
      }
    >
      {editing && (
        <CustomForm
          initial={editing === 'new' ? null : editing}
          onClose={() => setEditing(null)}
        />
      )}

      {list.isLoading && (
        <div className="text-dim font-mono text-[12px]">loading…</div>
      )}

      {list.data && list.data.rows.length === 0 && (
        <Empty>
          no custom services yet · click "new" to add one
        </Empty>
      )}

      {list.data && list.data.rows.length > 0 && (
        <div className="border border-line">
          <table className="w-full">
            <thead>
              <tr className="border-b border-line bg-raise">
                <Th>name</Th>
                <Th>proto</Th>
                <Th className="r">port(s)</Th>
                <Th>group</Th>
                <Th>updated</Th>
                <Th />
              </tr>
            </thead>
            <tbody>
              {list.data.rows.map((c) => (
                <CustomRow key={c.id} c={c} onEdit={() => setEditing(c)} />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Section>
  )
}

function CustomRow({ c, onEdit }: { c: CustomService; onEdit: () => void }) {
  const qc = useQueryClient()
  const confirm = useAppConfirm()
  const del = useMutation({
    mutationFn: () => api.deleteCustomService(c.id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['custom-services'] }),
  })

  const portText =
    c.port_lo === c.port_hi ? String(c.port_lo) : `${c.port_lo}–${c.port_hi}`

  return (
    <tr className="border-b border-line/60 hover:bg-surface">
      <td className="px-3 py-1.5 text-[12.5px] text-text">{c.name}</td>
      <td className="px-3 py-1.5 text-[12.5px] font-mono text-dim uppercase">{c.proto}</td>
      <td className="px-3 py-1.5 text-[12.5px] font-mono text-text text-right tabular-nums">
        {portText}
      </td>
      <td className="px-3 py-1.5 text-[12px]">
        {c.group ? <Tag tone="accent">{c.group}</Tag> : <span className="text-faint">—</span>}
      </td>
      <td className="px-3 py-1.5 text-[11px] font-mono text-faint">
        {c.updated_at ? c.updated_at.slice(0, 19).replace('T', ' ') : '—'}
        {c.updated_by ? ` · ${c.updated_by}` : ''}
      </td>
      <td className="px-3 py-1.5">
        <div className="flex justify-end gap-2">
          <Btn onClick={onEdit}>edit</Btn>
          <Btn
            tone="crit"
            onClick={async () => {
              const ok = await confirm({
                title: `Delete ${c.name}?`,
                body: 'The override is removed and the built-in name (if any) returns.',
                confirmLabel: 'Delete',
                tone: 'crit',
              })
              if (ok) del.mutate()
            }}
          >
            delete
          </Btn>
        </div>
      </td>
    </tr>
  )
}

function CustomForm({ initial, onClose }: { initial: CustomService | null; onClose: () => void }) {
  const qc = useQueryClient()
  const isEdit = !!initial
  const [c, setC] = useState<Partial<CustomService>>(
    initial ?? {
      proto: 'udp',
      port_lo: 4789,
      port_hi: 4789,
      name: '',
      description: '',
      group: '',
    },
  )
  const [error, setError] = useState<string | null>(null)
  const save = useMutation({
    mutationFn: () => api.putCustomService(c),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['custom-services'] })
      onClose()
    },
    onError: (e: Error) => setError(e.message),
  })

  return (
    <div className="border border-accent/40 bg-accent-wash px-4 py-4 mb-4">
      <div className="flex items-baseline gap-3 mb-3">
        <span className="text-[11px] uppercase tracking-[0.1em] text-accent font-semibold">
          {isEdit ? 'Edit custom service' : 'New custom service'}
        </span>
        <button onClick={onClose} className="ml-auto font-mono text-[11px] text-dim hover:text-text">
          cancel
        </button>
      </div>
      <div className="grid grid-cols-2 md:grid-cols-6 gap-3">
        <Field label="proto">
          <select
            value={c.proto}
            onChange={(e) => setC({ ...c, proto: e.target.value })}
            className="s-input"
          >
            <option value="tcp">tcp</option>
            <option value="udp">udp</option>
            <option value="sctp">sctp</option>
            <option value="dccp">dccp</option>
          </select>
        </Field>
        <Field label="port (lo)">
          <input
            type="number"
            value={c.port_lo ?? ''}
            min={1}
            max={65535}
            onChange={(e) => setC({ ...c, port_lo: Number(e.target.value) || 0 })}
            className="s-input"
          />
        </Field>
        <Field label="port (hi)" hint="same as lo for a single port">
          <input
            type="number"
            value={c.port_hi ?? ''}
            min={1}
            max={65535}
            onChange={(e) => setC({ ...c, port_hi: Number(e.target.value) || 0 })}
            className="s-input"
          />
        </Field>
        <Field label="name">
          <input
            value={c.name ?? ''}
            onChange={(e) => setC({ ...c, name: e.target.value })}
            placeholder="vxlan"
            className="s-input"
          />
        </Field>
        <Field label="group" hint="optional · alert rules can match by group">
          <input
            value={c.group ?? ''}
            onChange={(e) => setC({ ...c, group: e.target.value })}
            placeholder="DC-internal"
            className="s-input"
          />
        </Field>
        <Field label="owner" hint="who owns this entry">
          <input
            value={c.owner ?? ''}
            onChange={(e) => setC({ ...c, owner: e.target.value })}
            placeholder="netops"
            className="s-input"
          />
        </Field>
        <div className="col-span-2 md:col-span-6">
          <Field label="description">
            <input
              value={c.description ?? ''}
              onChange={(e) => setC({ ...c, description: e.target.value })}
              placeholder="Virtual eXtensible LAN — overlay encap"
              className="s-input"
            />
          </Field>
        </div>
      </div>

      {error && <div className="mt-3 text-crit font-mono text-[12px]">{error}</div>}

      <div className="flex items-center gap-3 mt-4">
        <Btn
          tone="accent"
          size="md"
          disabled={save.isPending || !c.name || !c.port_lo}
          onClick={() => {
            setError(null)
            save.mutate()
          }}
        >
          {save.isPending ? 'saving…' : 'save'}
        </Btn>
        <Btn size="md" onClick={onClose}>
          cancel
        </Btn>
        <span className="ml-auto font-mono text-[10.5px] text-faint">
          custom always wins · narrowest range first
        </span>
      </div>
    </div>
  )
}
