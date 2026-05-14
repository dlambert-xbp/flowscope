import { useMemo, useState, type ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import {
  api,
  type AlertRuleInstance,
  type AlertRuleParamSpec,
  type AlertScopeKind,
  type AlertScopeSelector,
  type AlertTemplate,
} from '../../../api'
import { SectionHeader } from '../Shell'
import { Banner, Btn, EditForm, Empty, Field, Section, StyleScope, Tag } from '../shared'

// AlertRules — phase 1 per-device alerting UI. One section per
// built-in template; each section lists the seed instance plus any
// operator-created instances. The "+ New" button on each section
// opens an inline editor for that template.
//
// Companion to AlertsTuning ("Defaults" tab), which still edits the
// legacy alert_rule_settings table — the api dual-writes seed
// instances so both surfaces stay coherent until the legacy table is
// dropped (phase 7 in the design doc).

export function AlertRules() {
  const templates = useQuery({
    queryKey: ['alert-templates'],
    queryFn: () => api.listAlertTemplates(),
  })
  const instances = useQuery({
    queryKey: ['alert-instances'],
    queryFn: () => api.listAlertInstances(),
  })

  const byTemplate = useMemo(() => {
    const m = new Map<string, AlertRuleInstance[]>()
    for (const inst of instances.data?.rows ?? []) {
      const arr = m.get(inst.template_id) ?? []
      arr.push(inst)
      m.set(inst.template_id, arr)
    }
    return m
  }, [instances.data])

  return (
    <div>
      <SectionHeader
        eyebrow="Per-device rules"
        title="Bind detectors to specific devices and interfaces"
        subtitle={
          <>
            Each built-in detector is a <em>template</em>. Create one or more <em>instances</em> per
            template — each instance has its own scope (which devices / interfaces it watches),
            its own threshold parameters, severity, and runbook. Instances fire as independent
            alerts, so two instances of the same template (e.g. "warn at 80%" and "page at 95%")
            are perfectly fine. The "Default · …" instance every template ships with watches all
            devices; edit its parameters in the <strong>Defaults</strong> tab or here directly.
          </>
        }
      />
      <StyleScope />

      {templates.isLoading && (
        <div className="px-6 pt-4 text-dim text-[12px] font-mono">loading templates…</div>
      )}

      {templates.data?.templates.map((tpl) => (
        <TemplateSection
          key={tpl.template_id}
          template={tpl}
          instances={byTemplate.get(tpl.template_id) ?? []}
        />
      ))}
    </div>
  )
}

function TemplateSection({
  template,
  instances,
}: {
  template: AlertTemplate
  instances: AlertRuleInstance[]
}) {
  const [creating, setCreating] = useState(false)
  const seed = instances.find((i) => i.is_seed)
  const custom = instances.filter((i) => !i.is_seed)

  return (
    <Section
      eyebrow={template.label}
      hint={template.template_id}
      actions={
        <Btn tone="accent" size="md" onClick={() => setCreating(true)}>
          + new instance
        </Btn>
      }
    >
      <div className="text-[13px] text-dim mb-3 max-w-[80ch]">{template.description}</div>

      {creating && (
        <InstanceForm
          mode="create"
          template={template}
          initial={blankInstance(template)}
          onClose={() => setCreating(false)}
        />
      )}

      {seed && (
        <InstanceCard template={template} instance={seed} />
      )}
      {custom.length === 0 && !creating && (
        <Empty>no per-device instances yet — click "+ new instance" to scope this rule.</Empty>
      )}
      {custom.map((inst) => (
        <InstanceCard key={inst.instance_id} template={template} instance={inst} />
      ))}
    </Section>
  )
}

function InstanceCard({
  template,
  instance,
}: {
  template: AlertTemplate
  instance: AlertRuleInstance
}) {
  const [editing, setEditing] = useState(false)
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: () => api.deleteAlertInstance(instance.instance_id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['alert-instances'] }),
  })

  if (editing) {
    return (
      <InstanceForm
        mode="edit"
        template={template}
        initial={instance}
        onClose={() => setEditing(false)}
      />
    )
  }

  const sev = (instance.severity || template.default_severity) as 'critical' | 'warning' | 'info'
  return (
    <div className="border border-line bg-raise px-4 py-3 mb-3 flex items-start gap-4">
      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-[13.5px] font-semibold text-text">{instance.name}</span>
          {instance.is_seed && <Tag>seed</Tag>}
          <Tag tone={sev === 'critical' ? 'crit' : sev === 'warning' ? 'warn' : 'accent'}>
            {sev}
          </Tag>
          {!instance.enabled && <Tag tone="warn">disabled</Tag>}
        </div>
        <div className="text-[11.5px] font-mono text-faint mt-1">
          {scopeSummary(instance.scope, template.scope_kinds)}
        </div>
        {instance.params && Object.keys(instance.params).length > 0 && (
          <div className="text-[11.5px] font-mono text-dim mt-1">
            {Object.entries(instance.params)
              .map(([k, v]) => `${k}=${formatVal(v)}`)
              .join('  ·  ')}
          </div>
        )}
        {instance.runbook && (
          <div className="text-[11px] font-mono text-faint mt-1 truncate">↳ {instance.runbook}</div>
        )}
      </div>
      <div className="flex items-center gap-1">
        <Btn onClick={() => setEditing(true)}>edit</Btn>
        {!instance.is_seed && (
          <Btn tone="crit" disabled={del.isPending} onClick={() => del.mutate()}>
            {del.isPending ? '…' : 'delete'}
          </Btn>
        )}
      </div>
    </div>
  )
}

function InstanceForm({
  mode,
  template,
  initial,
  onClose,
}: {
  mode: 'create' | 'edit'
  template: AlertTemplate
  initial: AlertRuleInstance
  onClose: () => void
}) {
  const [draft, setDraft] = useState<AlertRuleInstance>(initial)
  const [exportersText, setExportersText] = useState((initial.scope.exporters ?? []).join(', '))
  const [ifindexText, setIfindexText] = useState((initial.scope.ifindex ?? []).join(', '))
  const [bgpPeersText, setBgpPeersText] = useState((initial.scope.bgp_peers ?? []).join(', '))
  const [asnRemoteText, setAsnRemoteText] = useState((initial.scope.asn_remote ?? []).join(', '))
  const [error, setError] = useState<string | null>(null)
  const qc = useQueryClient()

  const supportsExporter = template.scope_kinds.includes('exporter')
  const supportsInterface = template.scope_kinds.includes('interface')
  const supportsBGPPeer = template.scope_kinds.includes('bgp_peer')

  const save = useMutation({
    mutationFn: async () => {
      const scope: AlertScopeSelector = {}
      if (supportsExporter) {
        const exps = parseList(exportersText)
        if (exps.length > 0) scope.exporters = exps
      }
      if (supportsInterface) {
        const idx = parseList(ifindexText).map(Number).filter((n) => Number.isFinite(n))
        if (idx.length > 0) scope.ifindex = idx
      }
      if (supportsBGPPeer) {
        const peers = parseList(bgpPeersText)
        if (peers.length > 0) scope.bgp_peers = peers
        const asns = parseList(asnRemoteText).map(Number).filter((n) => Number.isFinite(n))
        if (asns.length > 0) scope.asn_remote = asns
      }
      const body: Partial<AlertRuleInstance> & { instance_id?: string } = {
        ...draft,
        template_id: template.template_id,
        scope,
      }
      if (mode === 'create') {
        return api.createAlertInstance(body)
      }
      return api.updateAlertInstance({ ...body, instance_id: initial.instance_id })
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['alert-instances'] })
      onClose()
    },
    onError: (e: Error) => setError(e.message),
  })

  const seedLocked = draft.is_seed
  return (
    <EditForm
      title={mode === 'create' ? `new instance · ${template.label}` : `edit · ${initial.name}`}
      onCancel={onClose}
    >
      {error && <Banner tone="crit">{error}</Banner>}
      {seedLocked && (
        <Banner tone="accent">
          This is the <strong>default instance</strong> for this template — it always matches every
          device. You can edit parameters / severity / runbook here, or in the Defaults tab.
        </Banner>
      )}

      <div className="grid grid-cols-1 md:grid-cols-3 gap-3 mb-3">
        <Field label="name" hint="appears on the alert card">
          <input
            value={draft.name}
            onChange={(e) => setDraft({ ...draft, name: e.target.value })}
            disabled={seedLocked}
            className="s-input"
          />
        </Field>
        <Field label="state">
          <select
            value={draft.enabled ? 'on' : 'off'}
            onChange={(e) => setDraft({ ...draft, enabled: e.target.value === 'on' })}
            className="s-input"
          >
            <option value="on">enabled</option>
            <option value="off">disabled</option>
          </select>
        </Field>
        <Field label="severity">
          <select
            value={draft.severity || ''}
            onChange={(e) => setDraft({ ...draft, severity: e.target.value })}
            className="s-input"
          >
            <option value="">use default ({template.default_severity})</option>
            <option value="critical">critical</option>
            <option value="warning">warning</option>
            <option value="info">info</option>
          </select>
        </Field>
      </div>

      {!seedLocked && (supportsExporter || supportsInterface || supportsBGPPeer) && (
        <div className="border border-line bg-ink px-3 py-3 mb-3">
          <div className="text-[10.5px] uppercase tracking-[0.1em] text-faint font-mono mb-2">
            scope
          </div>
          <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
            {supportsExporter && (
              <Field label="exporters" hint="comma-separated IPs (empty = all devices)">
                <textarea
                  value={exportersText}
                  onChange={(e) => setExportersText(e.target.value)}
                  rows={2}
                  placeholder="10.0.0.1, 10.0.0.2"
                  className="s-input"
                />
              </Field>
            )}
            {supportsInterface && (
              <Field label="ifindex" hint="comma-separated SNMP ifindex values">
                <input
                  value={ifindexText}
                  onChange={(e) => setIfindexText(e.target.value)}
                  placeholder="1, 2, 3"
                  className="s-input"
                />
              </Field>
            )}
            {supportsBGPPeer && (
              <Field label="bgp peers" hint="comma-separated peer IPs (empty = all peers)">
                <input
                  value={bgpPeersText}
                  onChange={(e) => setBgpPeersText(e.target.value)}
                  placeholder="192.0.2.1, 198.51.100.5"
                  className="s-input"
                />
              </Field>
            )}
            {supportsBGPPeer && (
              <Field label="remote ASN" hint="comma-separated peer ASNs (empty = all ASNs)">
                <input
                  value={asnRemoteText}
                  onChange={(e) => setAsnRemoteText(e.target.value)}
                  placeholder="65001, 65002"
                  className="s-input"
                />
              </Field>
            )}
          </div>
        </div>
      )}

      {template.params.length > 0 && (
        <div className="border border-line bg-ink px-3 py-3 mb-3">
          <div className="text-[10.5px] uppercase tracking-[0.1em] text-faint font-mono mb-2">
            parameters
          </div>
          <div className="grid grid-cols-1 md:grid-cols-3 gap-3">
            {template.params.map((p) => (
              <ParamField
                key={p.name}
                spec={p}
                value={draft.params?.[p.name] ?? template.default_params[p.name] ?? p.default}
                onChange={(v) =>
                  setDraft({
                    ...draft,
                    params: { ...(draft.params ?? {}), [p.name]: v },
                  })
                }
              />
            ))}
          </div>
        </div>
      )}

      <Field label="runbook URL" hint="rendered on the alert card">
        <input
          value={draft.runbook ?? ''}
          onChange={(e) => setDraft({ ...draft, runbook: e.target.value })}
          placeholder="https://wiki.internal/runbooks/…"
          className="s-input"
        />
      </Field>

      <div className="mt-4 flex items-center gap-2">
        <Btn tone="accent" size="md" disabled={save.isPending} onClick={() => save.mutate()}>
          {save.isPending ? 'saving…' : mode === 'create' ? 'create instance' : 'save'}
        </Btn>
        <Btn onClick={onClose}>cancel</Btn>
      </div>
    </EditForm>
  )
}

function ParamField({
  spec,
  value,
  onChange,
}: {
  spec: AlertRuleParamSpec
  value: unknown
  onChange: (v: unknown) => void
}) {
  if (spec.kind === 'int' || spec.kind === 'float') {
    return (
      <Field label={spec.name} hint={`default ${String(spec.default)}`}>
        <input
          type="number"
          min={spec.min}
          max={spec.max}
          value={String(value ?? spec.default)}
          onChange={(e) => onChange(Number(e.target.value))}
          className="s-input"
        />
      </Field>
    )
  }
  if (spec.kind === 'bool') {
    return (
      <Field label={spec.name} hint={`default ${String(spec.default)}`}>
        <select
          value={value ? 'true' : 'false'}
          onChange={(e) => onChange(e.target.value === 'true')}
          className="s-input"
        >
          <option value="true">true</option>
          <option value="false">false</option>
        </select>
      </Field>
    )
  }
  return (
    <Field label={spec.name} hint={`default ${String(spec.default)}`}>
      <input
        value={String(value ?? spec.default)}
        onChange={(e) => onChange(e.target.value)}
        className="s-input"
      />
    </Field>
  )
}

function blankInstance(template: AlertTemplate): AlertRuleInstance {
  return {
    instance_id: '',
    template_id: template.template_id,
    name: '',
    enabled: true,
    severity: '',
    scope: {},
    params: { ...template.default_params },
    runbook: '',
    channels: [],
    is_seed: false,
  }
}

function scopeSummary(scope: AlertScopeSelector, kinds: AlertScopeKind[]): ReactNode {
  if (!scope || (Object.keys(scope).length === 0)) return 'matches all'
  const parts: string[] = []
  if (scope.exporters?.length) parts.push(`${scope.exporters.length} exporter${scope.exporters.length === 1 ? '' : 's'}`)
  if (scope.ifindex?.length) parts.push(`${scope.ifindex.length} ifindex`)
  if (scope.bgp_peers?.length) parts.push(`${scope.bgp_peers.length} bgp peer${scope.bgp_peers.length === 1 ? '' : 's'}`)
  if (scope.asn_remote?.length) parts.push(`${scope.asn_remote.length} asn`)
  if (scope.exporter_labels && Object.keys(scope.exporter_labels).length > 0) {
    parts.push('label-matched')
  }
  if (parts.length === 0) {
    if (kinds.length === 0) return 'all flow-pairs'
    return 'matches all'
  }
  return parts.join(' · ')
}

function parseList(s: string): string[] {
  return s
    .split(/[,\s]+/)
    .map((x) => x.trim())
    .filter(Boolean)
}

function formatVal(v: unknown): string {
  if (typeof v === 'number') {
    if (v >= 1_000_000_000) return `${(v / 1_000_000_000).toFixed(1)}G`
    if (v >= 1_000_000) return `${(v / 1_000_000).toFixed(1)}M`
    if (v >= 1_000) return `${(v / 1_000).toFixed(1)}k`
  }
  return String(v)
}
