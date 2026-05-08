import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { api, type AdvancedField } from '../../../api'
import { SectionHeader } from '../Shell'
import { Banner, Empty, Section, StyleScope, Tag } from '../shared'

// Advanced is a metadata-only view of every operationally relevant
// tunable. v1 is honest: nothing reloads at runtime today, so almost
// every field is labeled [restart]. Adding a [live] field is a one-
// line change in cmd/api/settings.go (advancedFields()) plus the
// reload wiring in the owning service.

export function Advanced() {
  const data = useQuery({
    queryKey: ['advanced-fields'],
    queryFn: () => api.listAdvanced(),
  })
  const [showRestart, setShowRestart] = useState(true)
  const [showLive, setShowLive] = useState(true)

  const fields = (data.data?.fields ?? []).filter((f) =>
    (f.reload === 'live' && showLive) || (f.reload === 'restart' && showRestart),
  )

  // Group by service for visual cohesion.
  const groups = new Map<string, AdvancedField[]>()
  for (const f of fields) {
    const key = f.service
    if (!groups.has(key)) groups.set(key, [])
    groups.get(key)!.push(f)
  }

  return (
    <div>
      <SectionHeader
        eyebrow="Settings · Advanced"
        title="Tuning & runtime knobs"
        subtitle="Every operationally relevant configuration the FlowScope services accept. Each field is tagged [live] (re-read on a refresh tick) or [restart] (read once at boot)."
      />
      <StyleScope />

      <Section
        eyebrow={`Fields · ${fields.length}`}
        actions={
          <>
            <Toggle on={showLive} onClick={() => setShowLive(!showLive)} tone="ok">
              live
            </Toggle>
            <Toggle on={showRestart} onClick={() => setShowRestart(!showRestart)} tone="warn">
              restart
            </Toggle>
          </>
        }
      >
        <Banner tone="warn">
          v1 is honest about reloads — nothing in the FlowScope services re-reads
          configuration at runtime today. Every field below is{' '}
          <strong>[restart]</strong>. Adding a <strong>[live]</strong> field
          requires a real reload tick in the owning service, not just a label
          change.
        </Banner>

        {data.isLoading && <div className="text-dim text-[12px] font-mono">loading…</div>}

        {fields.length === 0 && !data.isLoading && (
          <Empty>no fields match · re-enable the toggles above</Empty>
        )}

        {Array.from(groups.entries()).map(([svc, items]) => (
          <div key={svc} className="mb-5">
            <div className="text-[11px] uppercase tracking-[0.1em] text-faint font-mono font-semibold mb-2">
              {svc}
            </div>
            <div className="border border-line">
              <table className="w-full">
                <thead>
                  <tr className="border-b border-line bg-raise">
                    <Th>name</Th>
                    <Th>env var</Th>
                    <Th>default</Th>
                    <Th>reload</Th>
                    <Th>description</Th>
                  </tr>
                </thead>
                <tbody>
                  {items.map((f) => (
                    <tr key={`${svc}-${f.name}`} className="border-b border-line/60">
                      <td className="px-3 py-1.5 text-[12.5px] text-text">{f.name}</td>
                      <td className="px-3 py-1.5 text-[12px] font-mono text-faint">
                        {f.env_var ?? '—'}
                      </td>
                      <td className="px-3 py-1.5 text-[12px] font-mono text-dim">
                        {f.default_text ?? '—'}
                      </td>
                      <td className="px-3 py-1.5">
                        <Tag tone={f.reload === 'live' ? 'ok' : 'warn'}>
                          [{f.reload}]
                        </Tag>
                      </td>
                      <td className="px-3 py-1.5 text-[12px] text-faint max-w-[60ch]">
                        {f.description}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        ))}
      </Section>
    </div>
  )
}

function Th({ children }: { children: React.ReactNode }) {
  return (
    <th className="text-left text-[10.5px] uppercase tracking-[0.1em] text-faint font-mono font-semibold px-3 py-2">
      {children}
    </th>
  )
}

function Toggle({
  on,
  onClick,
  tone,
  children,
}: {
  on: boolean
  onClick: () => void
  tone?: 'ok' | 'warn'
  children: React.ReactNode
}) {
  const cls = on
    ? tone === 'ok'
      ? 's-tag s-tag--ok'
      : tone === 'warn'
        ? 's-tag s-tag--warn'
        : 's-tag s-tag--accent'
    : 's-tag opacity-50'
  return (
    <button type="button" onClick={onClick} className={cls}>
      {children}
    </button>
  )
}
