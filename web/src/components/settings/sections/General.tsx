import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { api } from '../../../api'
import { SectionHeader } from '../Shell'
import { Banner, Btn, Field, Section, StyleScope } from '../shared'

// General: small set of curated app-wide settings stored in
// app_settings (KV). The closed key set lives on the api side
// (validGeneralKey); this UI maps them to typed inputs.

type Form = {
  display_name: string
  default_time_range: '5m' | '15m' | '1h' | '6h' | '24h'
  default_theme: 'system' | 'dark' | 'light'
  timezone: string
  flow_retention_days: number
  counter_retention_days: number
}

const DEFAULTS: Form = {
  display_name: 'FlowScope',
  default_time_range: '5m',
  default_theme: 'system',
  timezone: 'UTC',
  flow_retention_days: 30,
  counter_retention_days: 30,
}

export function General() {
  const list = useQuery({
    queryKey: ['app-settings'],
    queryFn: () => api.listGeneralSettings(),
  })

  const [form, setForm] = useState<Form>(DEFAULTS)
  const [dirty, setDirty] = useState<Set<keyof Form>>(new Set())

  // Hydrate from server once on first load. Subsequent server updates
  // do not clobber local edits — we'd re-fetch only after saving.
  useEffect(() => {
    if (!list.data) return
    const byName: Record<string, unknown> = {}
    for (const r of list.data.rows) byName[r.name] = r.value
    setForm({
      ...DEFAULTS,
      ...(byName as Partial<Form>),
    } as Form)
    setDirty(new Set())
  }, [list.dataUpdatedAt])

  const qc = useQueryClient()
  const save = useMutation({
    mutationFn: async () => {
      // Only PUT keys the operator actually changed. Avoids rewriting
      // every key (and audit-logging it) on every save.
      for (const key of dirty) {
        await api.putGeneralSetting(key, form[key])
      }
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ['app-settings'] }),
  })

  const set = <K extends keyof Form>(k: K, v: Form[K]) => {
    setForm({ ...form, [k]: v })
    setDirty(new Set([...dirty, k]))
  }

  return (
    <div>
      <SectionHeader
        eyebrow="Settings · General"
        title="Display & defaults"
        subtitle="App-wide defaults that travel with new sessions and operator-visible chrome."
      />
      <StyleScope />

      <Section
        eyebrow="Display"
        actions={
          <Btn
            tone="accent"
            size="md"
            disabled={dirty.size === 0 || save.isPending}
            onClick={() => save.mutate()}
          >
            {save.isPending ? 'saving…' : `save${dirty.size > 0 ? ` (${dirty.size})` : ''}`}
          </Btn>
        }
      >
        <div className="grid grid-cols-1 md:grid-cols-2 gap-3 max-w-[780px]">
          <Field label="display name" hint="rendered in the brand bar and emails">
            <input
              value={form.display_name}
              onChange={(e) => set('display_name', e.target.value)}
              className="s-input"
            />
          </Field>
          <Field label="default theme">
            <select
              value={form.default_theme}
              onChange={(e) => set('default_theme', e.target.value as Form['default_theme'])}
              className="s-input"
            >
              <option value="system">system</option>
              <option value="dark">dark</option>
              <option value="light">light</option>
            </select>
          </Field>
          <Field label="default time range">
            <select
              value={form.default_time_range}
              onChange={(e) => set('default_time_range', e.target.value as Form['default_time_range'])}
              className="s-input"
            >
              <option value="5m">5 minutes</option>
              <option value="15m">15 minutes</option>
              <option value="1h">1 hour</option>
              <option value="6h">6 hours</option>
              <option value="24h">24 hours</option>
            </select>
          </Field>
          <Field label="timezone" hint="display only · queries always use UTC">
            <select
              value={form.timezone}
              onChange={(e) => set('timezone', e.target.value)}
              className="s-input"
            >
              <option value="UTC">UTC</option>
              <option value="America/New_York">America/New_York</option>
              <option value="America/Chicago">America/Chicago</option>
              <option value="America/Denver">America/Denver</option>
              <option value="America/Los_Angeles">America/Los_Angeles</option>
              <option value="Europe/London">Europe/London</option>
              <option value="Europe/Berlin">Europe/Berlin</option>
              <option value="Asia/Tokyo">Asia/Tokyo</option>
              <option value="Australia/Sydney">Australia/Sydney</option>
            </select>
          </Field>
        </div>
      </Section>

      <Section
        eyebrow="Retention"
        hint="changing these requires a service restart"
      >
        <Banner tone="warn">
          The numbers below describe what new schema migrations would target. Live
          retention is set on the ClickHouse table TTL — restart the api / store
          init container after editing so the migration reapplies.
        </Banner>
        <div className="grid grid-cols-1 md:grid-cols-2 gap-3 max-w-[600px]">
          <Field label="flow retention (days)" hint="how long flow rows live">
            <input
              type="number"
              min={1}
              max={3650}
              value={form.flow_retention_days}
              onChange={(e) => set('flow_retention_days', Number(e.target.value) || 30)}
              className="s-input"
            />
          </Field>
          <Field label="counter retention (days)" hint="iface_counter_samples">
            <input
              type="number"
              min={1}
              max={3650}
              value={form.counter_retention_days}
              onChange={(e) => set('counter_retention_days', Number(e.target.value) || 30)}
              className="s-input"
            />
          </Field>
        </div>
      </Section>
    </div>
  )
}
