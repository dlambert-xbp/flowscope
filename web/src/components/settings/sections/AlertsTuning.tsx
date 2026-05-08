import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { api, type AlertRuleAvailable, type AlertRuleSetting } from '../../../api'
import { SectionHeader } from '../Shell'
import { Btn, Field, Section, StyleScope, Tag } from '../shared'

// AlertsTuning: per-rule overrides for the Go-coded rules in
// internal/alerteng. The "available" list comes from the api so a
// new rule shipped on the backend appears here without a UI change.

export function AlertsTuning() {
  const data = useQuery({
    queryKey: ['alert-rules'],
    queryFn: () => api.listAlertRules(),
  })

  const settingsByID = new Map<string, AlertRuleSetting>()
  for (const r of data.data?.rows ?? []) settingsByID.set(r.rule_id, r)

  return (
    <div>
      <SectionHeader
        eyebrow="Settings · Alert rules"
        title="Tune the built-in detectors"
        subtitle={
          <>
            FlowScope ships rules as Go code in <code className="font-mono text-text">internal/alerteng</code>.
            Operators can enable/disable, override severity, point at a runbook, and
            tune per-rule parameters here. New rule definitions arrive in releases —
            this list is built from the api so a fresh detector appears without a
            UI change.
          </>
        }
      />
      <StyleScope />

      {data.isLoading && (
        <div className="px-6 pt-4 text-dim text-[12px] font-mono">loading…</div>
      )}

      {data.data?.available.map((rule) => (
        <RuleSection
          key={rule.rule_id}
          rule={rule}
          current={settingsByID.get(rule.rule_id)}
        />
      ))}
    </div>
  )
}

function RuleSection({
  rule,
  current,
}: {
  rule: AlertRuleAvailable
  current?: AlertRuleSetting
}) {
  const qc = useQueryClient()
  const [setting, setSetting] = useState<AlertRuleSetting>(() => ({
    rule_id: rule.rule_id,
    enabled: current?.enabled ?? true,
    severity: current?.severity ?? '',
    params: { ...(current?.params ?? defaultParams(rule)) },
    runbook: current?.runbook ?? '',
    channels: current?.channels ?? [],
  }))
  const [dirty, setDirty] = useState(false)

  useEffect(() => {
    if (current) {
      setSetting({
        rule_id: rule.rule_id,
        enabled: current.enabled,
        severity: current.severity ?? '',
        params: { ...(current.params ?? defaultParams(rule)) },
        runbook: current.runbook ?? '',
        channels: current.channels ?? [],
      })
      setDirty(false)
    }
  }, [current])

  const save = useMutation({
    mutationFn: () => api.putAlertRule(setting),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['alert-rules'] })
      setDirty(false)
    },
  })

  const update = (patch: Partial<AlertRuleSetting>) => {
    setSetting({ ...setting, ...patch })
    setDirty(true)
  }
  const updateParam = (name: string, val: unknown) => {
    setSetting({ ...setting, params: { ...(setting.params ?? {}), [name]: val } })
    setDirty(true)
  }

  const effectiveSeverity = setting.severity || rule.default_severity

  return (
    <Section
      eyebrow={rule.label}
      hint={rule.rule_id}
      actions={
        <Btn
          tone="accent"
          size="md"
          disabled={!dirty || save.isPending}
          onClick={() => save.mutate()}
        >
          {save.isPending ? 'saving…' : 'save'}
        </Btn>
      }
    >
      <div className="text-[13px] text-dim mb-3 max-w-[80ch]">{rule.description}</div>

      <div className="grid grid-cols-1 md:grid-cols-3 gap-3 mb-3">
        <Field label="state">
          <select
            value={setting.enabled ? 'on' : 'off'}
            onChange={(e) => update({ enabled: e.target.value === 'on' })}
            className="s-input"
          >
            <option value="on">enabled</option>
            <option value="off">disabled</option>
          </select>
        </Field>
        <Field label="severity">
          <select
            value={setting.severity || ''}
            onChange={(e) => update({ severity: e.target.value })}
            className="s-input"
          >
            <option value="">use default ({rule.default_severity})</option>
            <option value="critical">critical</option>
            <option value="warning">warning</option>
            <option value="info">info</option>
          </select>
        </Field>
        <Field label="runbook URL" hint="rendered on the alert card">
          <input
            value={setting.runbook ?? ''}
            onChange={(e) => update({ runbook: e.target.value })}
            placeholder="https://wiki.internal/runbooks/exporter-silent.md"
            className="s-input"
          />
        </Field>
      </div>

      {rule.params.length > 0 && (
        <div className="grid grid-cols-1 md:grid-cols-3 gap-3">
          {rule.params.map((p) => (
            <ParamField
              key={p.name}
              spec={p}
              value={setting.params?.[p.name]}
              onChange={(v) => updateParam(p.name, v)}
            />
          ))}
        </div>
      )}

      <div className="mt-3 text-[11px] font-mono text-faint flex items-center gap-3">
        <span>effective severity:</span>
        <Tag tone={effectiveSeverity === 'critical' ? 'crit' : effectiveSeverity === 'warning' ? 'warn' : 'accent'}>
          {effectiveSeverity}
        </Tag>
        {dirty && <span className="text-warn">· unsaved changes</span>}
      </div>
    </Section>
  )
}

function ParamField({
  spec,
  value,
  onChange,
}: {
  spec: { name: string; kind: string; default: number | string | boolean; min?: number; max?: number }
  value: unknown
  onChange: (v: unknown) => void
}) {
  const v = value === undefined ? spec.default : value
  if (spec.kind === 'int' || spec.kind === 'float') {
    return (
      <Field label={spec.name} hint={`default ${spec.default}`}>
        <input
          type="number"
          min={spec.min}
          max={spec.max}
          value={String(v ?? '')}
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
          value={v ? 'true' : 'false'}
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
      <input value={String(v ?? '')} onChange={(e) => onChange(e.target.value)} className="s-input" />
    </Field>
  )
}

function defaultParams(rule: AlertRuleAvailable): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const p of rule.params) out[p.name] = p.default
  return out
}
