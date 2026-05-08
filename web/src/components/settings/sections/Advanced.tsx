import { useQuery } from '@tanstack/react-query'
import { api, type AdvancedField } from '../../../api'
import { SectionHeader } from '../Shell'
import { Banner, Section, StyleScope } from '../shared'

// Advanced is a metadata-only view of every operationally relevant
// tunable. v1 is honest: nothing reloads at runtime today, so the
// reload column would be uniform [restart] noise — replaced with
// the banner up top. Adding a future [live] field reintroduces the
// column conditionally.

export function Advanced() {
  const data = useQuery({
    queryKey: ['advanced-fields'],
    queryFn: () => api.listAdvanced(),
  })

  const fields = data.data?.fields ?? []

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
        eyebrow="Advanced"
        title="Tuning & runtime knobs"
        subtitle="Every operationally relevant configuration the FlowScope services accept. Edit through env vars on the deployment; restart the service for changes to take effect."
      />
      <StyleScope />

      <div className="px-6 pt-4">
        <Banner tone="warn">
          <strong className="text-warn">All fields are restart-required in v1.</strong>{' '}
          Nothing in the FlowScope services re-reads configuration at runtime today.
          Adding a live-reloadable field requires a real reload tick in the owning
          service, not just a UI change.
        </Banner>
      </div>

      <Section eyebrow={`Fields · ${fields.length}`}>
        {data.isLoading && <div className="text-dim text-[12px] font-mono">loading…</div>}

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
                      <td
                        className="px-3 py-1.5 text-[12px] text-faint max-w-[60ch]"
                        title={f.description}
                      >
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

function Th({ children }: { children?: React.ReactNode }) {
  return (
    <th className="text-left text-[10.5px] uppercase tracking-[0.1em] text-faint font-mono font-semibold px-3 py-2">
      {children}
    </th>
  )
}
