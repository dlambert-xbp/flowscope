import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo, useState } from 'react'
import {
  api,
  fmt,
  type SNMPCredential,
  type SNMPProfile,
  type SNMPScan,
  type SNMPTestResult,
} from '../../../api'
import { useAppConfirm } from '../../ui/appConfirm'
import { SectionHeader } from '../Shell'
import { Banner, Btn, Empty, Field, Section, StyleScope, Tag } from '../shared'

// SNMP settings — named credential library + per-exporter bindings +
// bulk discovery scans + auto-bind for flow-discovered exporters.
//
// This page is intentionally three stacked sections (profiles →
// auto-bind order → bulk discovery → bindings) rather than tabs.
// Operators need to see all of it at once when troubleshooting why a
// device isn't being walked.

const AUTH_PROTOS = ['', 'MD5', 'SHA', 'SHA-224', 'SHA-256', 'SHA-384', 'SHA-512']
const PRIV_PROTOS = ['', 'DES', 'AES', 'AES-192', 'AES-256']

export function SNMP() {
  const profilesQ = useQuery({
    queryKey: ['snmp-profiles'],
    queryFn: () => api.listProfiles().catch((e: Error) => Promise.reject(e)),
    refetchInterval: 15_000,
    retry: false,
  })
  const credsQ = useQuery({
    queryKey: ['snmp-credentials'],
    queryFn: () => api.listCredentials().catch((e: Error) => Promise.reject(e)),
    refetchInterval: 10_000,
    retry: false,
  })

  const isUnavailable =
    profilesQ.error?.message?.includes('503') ||
    profilesQ.error?.message?.includes('disabled')

  const [editingProfile, setEditingProfile] = useState<SNMPProfile | 'new' | null>(null)
  const [editingBinding, setEditingBinding] = useState<SNMPCredential | 'new' | null>(null)

  const profiles = profilesQ.data?.profiles ?? []
  const bindings = credsQ.data?.credentials ?? []
  const profilesById = useMemo(() => {
    const m: Record<string, SNMPProfile> = {}
    for (const p of profiles) m[p.id] = p
    return m
  }, [profiles])

  // Count bound devices per profile.
  const bindingsPerProfile = useMemo(() => {
    const m: Record<string, number> = {}
    for (const c of bindings) {
      if (c.profile_id) m[c.profile_id] = (m[c.profile_id] || 0) + 1
    }
    return m
  }, [bindings])

  const discoveryProfiles = useMemo(
    () =>
      profiles
        .filter((p) => p.use_for_discovery)
        .sort(
          (a, b) =>
            a.discovery_priority - b.discovery_priority || a.name.localeCompare(b.name),
        ),
    [profiles],
  )

  return (
    <div>
      <SectionHeader
        eyebrow="SNMP"
        title="Credentials, profiles & discovery"
        subtitle="Named credential library, per-exporter bindings, and bulk discovery. Secrets stored in ClickHouse, AES-256-GCM-sealed under FLOWSCOPE_SNMP_KEY."
        actions={
          !isUnavailable && (
            <div className="flex gap-2">
              <Btn tone="accent" size="md" onClick={() => setEditingProfile('new')}>
                + profile
              </Btn>
              <Btn size="md" onClick={() => setEditingBinding('new')}>
                + binding
              </Btn>
            </div>
          )
        }
      />
      <StyleScope />

      {isUnavailable && (
        <div className="px-6 pt-4">
          <Banner tone="crit">
            <strong className="text-crit">Disabled.</strong> The api service is
            running without <code className="bg-raise px-1 font-mono">FLOWSCOPE_SNMP_KEY</code>.
            Set the master-key env var on both the api and snmp services (same
            value) and restart to enable credential management.
          </Banner>
        </div>
      )}

      {!isUnavailable && (
        <>
          <Section
            eyebrow={`Profile library · ${profiles.length}`}
            hint="named v2c / v3 profiles · bindings reference one · use-for-discovery profiles auto-bind flow-discovered exporters"
          >
            {editingProfile && (
              <ProfileForm
                initial={editingProfile === 'new' ? null : editingProfile}
                onClose={() => setEditingProfile(null)}
              />
            )}
            {profilesQ.isLoading && <div className="text-dim text-[12px] font-mono">loading…</div>}
            {profilesQ.data && profiles.length === 0 && !editingProfile && (
              <Empty>
                no profiles configured · create one to credential your devices
              </Empty>
            )}
            {profiles.length > 0 && (
              <ProfileList
                rows={profiles}
                bindingsPerProfile={bindingsPerProfile}
                onEdit={setEditingProfile}
              />
            )}
          </Section>

          <Section
            eyebrow={`Auto-bind order · ${discoveryProfiles.length}`}
            hint="profiles tried when a new exporter appears in flow ingest with no binding · 1s tight timeout on first attempt, full timeout on subsequent"
          >
            <AutoBindOrder profiles={discoveryProfiles} />
          </Section>

          <Section
            eyebrow="Bulk discovery"
            hint="scan a CIDR (/24 to /32) with a single profile · matched IPs are bound on confirm"
          >
            <BulkDiscovery profiles={profiles} />
          </Section>

          <Section
            eyebrow={`Bindings · ${bindings.length}`}
            hint="per-exporter walk credentials · profile-referenced rows defer to the library"
          >
            {editingBinding && (
              <CredentialForm
                initial={editingBinding === 'new' ? null : editingBinding}
                profiles={profiles}
                onClose={() => setEditingBinding(null)}
              />
            )}
            {credsQ.isLoading && <div className="text-dim text-[12px] font-mono">loading…</div>}
            {credsQ.data && bindings.length === 0 && !editingBinding && (
              <Empty>
                no per-exporter bindings configured · the snmp service falls back to
                the cluster-wide v2c community / mock unless an auto-bind profile matches
              </Empty>
            )}
            {bindings.length > 0 && (
              <CredentialList
                rows={bindings}
                profilesById={profilesById}
                onEdit={setEditingBinding}
              />
            )}
          </Section>
        </>
      )}
    </div>
  )
}

/* --------------------------- Profile library --------------------------- */

function ProfileList({
  rows,
  bindingsPerProfile,
  onEdit,
}: {
  rows: SNMPProfile[]
  bindingsPerProfile: Record<string, number>
  onEdit: (p: SNMPProfile) => void
}) {
  return (
    <div className="border border-line">
      <table className="w-full">
        <thead>
          <tr className="border-b border-line bg-raise">
            <Th>name</Th>
            <Th>version</Th>
            <Th>identity</Th>
            <Th>discovery</Th>
            <Th align="r">bound</Th>
            <Th>updated</Th>
            <Th />
          </tr>
        </thead>
        <tbody>
          {rows.map((p) => (
            <ProfileRow
              key={p.id}
              p={p}
              boundCount={bindingsPerProfile[p.id] || 0}
              onEdit={() => onEdit(p)}
            />
          ))}
        </tbody>
      </table>
    </div>
  )
}

function ProfileRow({
  p,
  boundCount,
  onEdit,
}: {
  p: SNMPProfile
  boundCount: number
  onEdit: () => void
}) {
  const qc = useQueryClient()
  const confirm = useAppConfirm()
  const del = useMutation({
    mutationFn: () => api.deleteProfile(p.id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['snmp-profiles'] }),
  })

  const identity =
    p.version === 'v3' ? (
      <span className="text-dim">
        {p.v3_username || <span className="text-faint">—</span>}{' '}
        <span className="text-faint">·</span>{' '}
        <span className={p.has_auth_pass ? 'text-ok' : 'text-faint'}>
          {p.v3_auth_proto || 'noAuth'}
        </span>{' '}
        <span className="text-faint">·</span>{' '}
        <span className={p.has_priv_pass ? 'text-ok' : 'text-faint'}>
          {p.v3_priv_proto || 'noPriv'}
        </span>
      </span>
    ) : (
      <span className={p.has_community ? 'text-ok' : 'text-faint'}>
        {p.has_community ? '✓ community set' : 'community not set'}
      </span>
    )

  return (
    <tr className="border-b border-line/60 hover:bg-surface">
      <td className="px-3 py-1.5 text-[12.5px] font-mono text-text">{p.name}</td>
      <td className="px-3 py-1.5">
        <Tag tone={p.version === 'v3' ? 'accent' : undefined}>{p.version}</Tag>
      </td>
      <td className="px-3 py-1.5 text-[12px] font-mono">{identity}</td>
      <td className="px-3 py-1.5">
        {p.use_for_discovery ? (
          <span className="font-mono text-[11px] text-accent">
            #{p.discovery_priority || 0} disc
          </span>
        ) : (
          <span className="font-mono text-[11px] text-faint">—</span>
        )}
      </td>
      <td className="px-3 py-1.5 text-right text-[12.5px] font-mono tabular">
        {boundCount}
      </td>
      <td className="px-3 py-1.5 text-[11px] font-mono text-faint">
        {p.updated_at ? fmt.time(p.updated_at).slice(0, 19) : '—'}
        {p.updated_by ? ` · ${p.updated_by}` : ''}
      </td>
      <td className="px-3 py-1.5">
        <div className="flex justify-end gap-2">
          <Btn onClick={onEdit}>edit</Btn>
          <Btn
            tone="crit"
            onClick={async () => {
              const ok = await confirm({
                title: `Delete profile ${p.name}?`,
                body:
                  boundCount > 0
                    ? `${boundCount} binding(s) reference this profile. The delete will be refused until you unbind them first.`
                    : 'No bindings reference this profile.',
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

function ProfileForm({
  initial,
  onClose,
}: {
  initial: SNMPProfile | null
  onClose: () => void
}) {
  const qc = useQueryClient()
  const isEdit = !!initial
  const [p, setP] = useState<SNMPProfile>(
    initial ?? {
      id: '',
      name: '',
      version: 'v2c',
      port: 161,
      interval_sec: 60,
      use_for_discovery: false,
      discovery_priority: 0,
      has_community: false,
      has_auth_pass: false,
      has_priv_pass: false,
    },
  )
  const [error, setError] = useState<string | null>(null)
  const save = useMutation({
    mutationFn: () => (isEdit ? api.updateProfile(p) : api.createProfile(p)),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['snmp-profiles'] })
      onClose()
    },
    onError: (e: Error) => setError(e.message),
  })

  const set = <K extends keyof SNMPProfile>(k: K, v: SNMPProfile[K]) =>
    setP({ ...p, [k]: v })

  return (
    <div className="border border-accent/40 bg-accent-wash px-4 py-4 mb-4">
      <div className="flex items-baseline gap-3 mb-3">
        <span className="text-[11px] uppercase tracking-[0.1em] text-accent font-semibold">
          {isEdit ? `Edit profile · ${initial?.name}` : 'New profile'}
        </span>
        <button onClick={onClose} className="ml-auto font-mono text-[11px] text-dim hover:text-text">
          cancel
        </button>
      </div>

      <div className="grid grid-cols-2 md:grid-cols-4 gap-3 mb-3">
        <Field label="name">
          <input
            value={p.name}
            onChange={(e) => set('name', e.target.value)}
            placeholder="e.g. hq-readonly-v3"
            className="s-input"
          />
        </Field>
        <Field label="version">
          <select
            value={p.version}
            onChange={(e) => set('version', e.target.value as 'v2c' | 'v3')}
            className="s-input"
          >
            <option value="v2c">v2c</option>
            <option value="v3">v3</option>
          </select>
        </Field>
        <Field label="port">
          <input
            type="number"
            value={p.port}
            onChange={(e) => set('port', Number(e.target.value) || 161)}
            className="s-input"
          />
        </Field>
        <Field label="interval (sec)">
          <input
            type="number"
            value={p.interval_sec}
            onChange={(e) => set('interval_sec', Number(e.target.value) || 60)}
            className="s-input"
          />
        </Field>
      </div>

      {p.version === 'v2c' && (
        <Field label={`community ${p.has_community ? '· (already set; leave blank to keep)' : ''}`}>
          <input
            type="password"
            value={p.community || ''}
            onChange={(e) => set('community', e.target.value)}
            placeholder={p.has_community ? '••••••••' : 'public'}
            className="s-input"
          />
        </Field>
      )}

      {p.version === 'v3' && (
        <div className="grid grid-cols-2 md:grid-cols-3 gap-3">
          <Field label="username">
            <input
              value={p.v3_username || ''}
              onChange={(e) => set('v3_username', e.target.value)}
              placeholder="noc-readonly"
              className="s-input"
            />
          </Field>
          <Field label="auth protocol">
            <select
              value={p.v3_auth_proto || ''}
              onChange={(e) => set('v3_auth_proto', e.target.value)}
              className="s-input"
            >
              {AUTH_PROTOS.map((x) => (
                <option key={x || 'none'} value={x}>
                  {x || 'none'}
                </option>
              ))}
            </select>
          </Field>
          <Field label={`auth passphrase ${p.has_auth_pass ? '· (set)' : ''}`}>
            <input
              type="password"
              value={p.v3_auth_pass || ''}
              onChange={(e) => set('v3_auth_pass', e.target.value)}
              placeholder={p.has_auth_pass ? '••••••••' : ''}
              className="s-input"
            />
          </Field>
          <Field label="priv protocol">
            <select
              value={p.v3_priv_proto || ''}
              onChange={(e) => set('v3_priv_proto', e.target.value)}
              className="s-input"
            >
              {PRIV_PROTOS.map((x) => (
                <option key={x || 'none'} value={x}>
                  {x || 'none'}
                </option>
              ))}
            </select>
          </Field>
          <Field label={`priv passphrase ${p.has_priv_pass ? '· (set)' : ''}`}>
            <input
              type="password"
              value={p.v3_priv_pass || ''}
              onChange={(e) => set('v3_priv_pass', e.target.value)}
              placeholder={p.has_priv_pass ? '••••••••' : ''}
              className="s-input"
            />
          </Field>
          <Field label="context (optional)">
            <input
              value={p.v3_context || ''}
              onChange={(e) => set('v3_context', e.target.value)}
              className="s-input"
            />
          </Field>
        </div>
      )}

      <div className="mt-4 pt-3 border-t border-line">
        <label className="flex items-center gap-2 font-mono text-[12px] text-dim cursor-pointer select-none">
          <input
            type="checkbox"
            checked={p.use_for_discovery}
            onChange={(e) => set('use_for_discovery', e.target.checked)}
          />
          <span>
            use for auto-bind of flow-discovered exporters
            <span className="text-faint">
              {' '}· profiles tried in priority order until one authenticates
            </span>
          </span>
        </label>
        {p.use_for_discovery && (
          <div className="mt-2 ml-6">
            <Field label="priority (lower = tried first)">
              <input
                type="number"
                value={p.discovery_priority}
                onChange={(e) => set('discovery_priority', Number(e.target.value) || 0)}
                className="s-input w-32"
              />
            </Field>
          </div>
        )}
      </div>

      {error && <div className="mt-3 text-crit font-mono text-[12px]">{error}</div>}

      <div className="flex items-center gap-3 mt-4">
        <Btn
          tone="accent"
          size="md"
          disabled={save.isPending || !p.name}
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
          empty passphrase fields preserve the existing secret
        </span>
      </div>
    </div>
  )
}

/* --------------------------- Auto-bind order panel --------------------------- */

function AutoBindOrder({ profiles }: { profiles: SNMPProfile[] }) {
  if (profiles.length === 0) {
    return (
      <Empty>
        no profiles flagged for auto-bind · flow-discovered exporters will fall through to
        the env-var fallback community / mock
      </Empty>
    )
  }
  return (
    <div className="border border-line">
      {profiles.map((p, i) => (
        <div
          key={p.id}
          className={`px-3 py-2 flex items-center gap-3 ${
            i < profiles.length - 1 ? 'border-b border-line/60' : ''
          }`}
        >
          <span className="font-mono text-[11px] text-accent w-8">#{p.discovery_priority || 0}</span>
          <span className="font-mono text-[12.5px] text-text">{p.name}</span>
          <Tag tone={p.version === 'v3' ? 'accent' : undefined}>{p.version}</Tag>
          <span className="font-mono text-[11.5px] text-dim ml-auto">
            port {p.port} · interval {p.interval_sec}s
          </span>
        </div>
      ))}
    </div>
  )
}

/* --------------------------- Bulk discovery --------------------------- */

function BulkDiscovery({ profiles }: { profiles: SNMPProfile[] }) {
  const qc = useQueryClient()
  const [range, setRange] = useState('')
  const [profileId, setProfileId] = useState(profiles[0]?.id || '')
  const [scanId, setScanId] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [committedSummary, setCommittedSummary] = useState<{ bound: number; skipped: number } | null>(
    null,
  )

  // Pick a default profile when the list arrives or changes and the
  // current selection is no longer valid.
  useEffect(() => {
    if (!profileId && profiles[0]) setProfileId(profiles[0].id)
    if (profileId && !profiles.find((p) => p.id === profileId)) {
      setProfileId(profiles[0]?.id || '')
    }
  }, [profiles, profileId])

  // Poll the scan job while it's running.
  const scanQ = useQuery({
    queryKey: ['snmp-scan', scanId],
    queryFn: () => (scanId ? api.getScan(scanId) : Promise.resolve(null)),
    enabled: !!scanId,
    refetchInterval: (q) => {
      const d = q.state.data as SNMPScan | null
      if (!d) return 2000
      return d.state === 'running' ? 2000 : false
    },
  })

  const start = useMutation({
    mutationFn: () => api.createScan(profileId, range.trim()),
    onSuccess: (job) => {
      setScanId(job.id)
      setSelected(new Set())
      setCommittedSummary(null)
    },
    onError: (e: Error) => setError(e.message),
  })

  const commit = useMutation({
    mutationFn: () =>
      api.commitScan(scanId!, Array.from(selected)),
    onSuccess: (res) => {
      setCommittedSummary({ bound: res.bound.length, skipped: res.skipped.length })
      setSelected(new Set())
      qc.invalidateQueries({ queryKey: ['snmp-credentials'] })
    },
    onError: (e: Error) => setError(e.message),
  })

  const scan = scanQ.data
  const matched = scan?.results.filter((r) => r.matched) || []
  const rejected = scan?.results.filter((r) => !r.matched && !r.silent) || []
  const selectedProfile = profiles.find((p) => p.id === profileId)

  const allMatchedSelected = matched.length > 0 && matched.every((r) => selected.has(r.ip))
  const toggleAll = () => {
    if (allMatchedSelected) setSelected(new Set())
    else setSelected(new Set(matched.map((r) => r.ip)))
  }
  const toggle = (ip: string) => {
    const next = new Set(selected)
    if (next.has(ip)) next.delete(ip)
    else next.add(ip)
    setSelected(next)
  }

  return (
    <div>
      <div className="grid grid-cols-1 md:grid-cols-[1.4fr_1fr] gap-3">
        <div className="border border-line bg-raise px-3 py-3">
          <div className="text-[10.5px] uppercase tracking-[0.16em] font-mono text-faint mb-1">
            range
          </div>
          <input
            value={range}
            onChange={(e) => setRange(e.target.value)}
            placeholder="10.110.0.0/24 · 10.110.0.0/28 · 10.110.0.5/32 · 10.110.0.1-10.110.0.50"
            className="s-input w-full"
            disabled={start.isPending || scan?.state === 'running'}
          />
          <div className="text-[10.5px] font-mono text-faint mt-1">
            CIDR · /24 to /32 · max 256 addresses per scan · /32 for a single IP
          </div>
        </div>
        <div className="border border-line bg-raise px-3 py-3">
          <div className="text-[10.5px] uppercase tracking-[0.16em] font-mono text-faint mb-1">
            profile
          </div>
          <select
            value={profileId}
            onChange={(e) => setProfileId(e.target.value)}
            className="s-input w-full"
            disabled={start.isPending || scan?.state === 'running'}
          >
            {profiles.length === 0 && <option value="">no profiles configured</option>}
            {profiles.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name} · {p.version}
              </option>
            ))}
          </select>
          {selectedProfile && (
            <div className="mt-2 font-mono text-[11px] text-dim border border-dashed border-line px-2 py-1.5">
              {selectedProfile.version}
              {selectedProfile.version === 'v3' && (
                <>
                  {' · user '}
                  <span className="text-text">{selectedProfile.v3_username || '—'}</span>
                  {' · auth '}
                  <span className={selectedProfile.has_auth_pass ? 'text-ok' : 'text-faint'}>
                    {selectedProfile.v3_auth_proto || 'none'}
                  </span>
                  {' · priv '}
                  <span className={selectedProfile.has_priv_pass ? 'text-ok' : 'text-faint'}>
                    {selectedProfile.v3_priv_proto || 'none'}
                  </span>
                </>
              )}
              {' · port '}
              {selectedProfile.port}
            </div>
          )}
          <div className="text-[10.5px] font-mono text-faint mt-1">
            devices that don't respond to this profile are reported in the results — re-run
            with a different profile to credential them
          </div>
        </div>
      </div>

      <div className="flex items-center gap-3 mt-3">
        <Btn
          tone="accent"
          size="md"
          disabled={
            !range.trim() ||
            !profileId ||
            start.isPending ||
            scan?.state === 'running'
          }
          onClick={() => {
            setError(null)
            start.mutate()
          }}
        >
          {start.isPending
            ? 'starting…'
            : scan?.state === 'running'
              ? 'scanning…'
              : 'start scan'}
        </Btn>
        {scanId && (
          <Btn
            size="md"
            onClick={() => {
              setScanId(null)
              setSelected(new Set())
              setCommittedSummary(null)
              if (scanId) api.cancelScan(scanId).catch(() => {})
            }}
          >
            clear results
          </Btn>
        )}
        {scan && (
          <span className="font-mono text-[11.5px] text-dim ml-auto">
            {scan.probed} / {scan.total} probed ·{' '}
            <span className="text-ok">{scan.matched} matched</span>
            {scan.rejected > 0 && (
              <>
                {' · '}
                <span className="text-crit">{scan.rejected} auth-rejected</span>
              </>
            )}
            {' · '}
            <span className="text-faint">{scan.silent} silent</span>
          </span>
        )}
      </div>

      {error && <div className="mt-3 text-crit font-mono text-[12px]">{error}</div>}
      {committedSummary && (
        <div className="mt-3 font-mono text-[12px] text-ok">
          ✓ bound {committedSummary.bound} device(s)
          {committedSummary.skipped > 0 && (
            <span className="text-warn"> · skipped {committedSummary.skipped}</span>
          )}
        </div>
      )}

      {scan && (
        <div className="mt-4 border border-line">
          <div className="px-3 py-2 border-b border-line bg-raise flex items-center gap-3">
            <span className="font-mono text-[11.5px] text-text">
              scan results · {scan.range} · {selectedProfile?.name || scan.profile_id}
            </span>
            <span className="font-mono text-[11px] text-faint ml-auto">
              {scan.state === 'running'
                ? 'running…'
                : scan.state === 'cancelled'
                  ? 'cancelled'
                  : `done in ${
                      scan.finished_at && scan.started_at
                        ? Math.max(
                            0,
                            Math.round(
                              (new Date(scan.finished_at).getTime() -
                                new Date(scan.started_at).getTime()) /
                                1000,
                            ),
                          )
                        : '?'
                    }s`}
            </span>
          </div>

          {matched.length === 0 && scan.state !== 'running' && (
            <div className="px-3 py-3 text-[12px] text-dim font-mono">
              no devices matched this profile
            </div>
          )}

          {matched.length > 0 && (
            <table className="w-full">
              <thead>
                <tr className="border-b border-line bg-surface">
                  <Th>
                    <input
                      type="checkbox"
                      checked={allMatchedSelected}
                      onChange={toggleAll}
                    />
                  </Th>
                  <Th>ip</Th>
                  <Th>sys_name</Th>
                  <Th>sys_descr</Th>
                </tr>
              </thead>
              <tbody>
                {matched.map((r) => (
                  <tr key={r.ip} className="border-b border-line/60 hover:bg-surface">
                    <td className="px-3 py-1.5">
                      <input
                        type="checkbox"
                        checked={selected.has(r.ip)}
                        onChange={() => toggle(r.ip)}
                      />
                    </td>
                    <td className="px-3 py-1.5 text-[12.5px] font-mono text-text">
                      {r.ip}
                    </td>
                    <td className="px-3 py-1.5 text-[12.5px] font-mono text-dim">
                      {r.sys_name || <span className="text-faint">—</span>}
                    </td>
                    <td className="px-3 py-1.5 text-[12px] font-mono text-faint truncate max-w-[420px]">
                      {r.sys_descr || ''}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}

          {rejected.length > 0 && (
            <div className="border-t border-line bg-[rgba(226,107,107,0.04)] px-3 py-2.5">
              <div className="font-mono text-[11px] text-crit mb-1">
                {rejected.length} alive, profile didn't authenticate
              </div>
              <div className="font-mono text-[11.5px] text-dim space-y-0.5">
                {rejected.map((r) => (
                  <div key={r.ip}>
                    {r.ip}{' '}
                    <span className="text-faint">
                      · {r.error ? r.error.slice(0, 80) : 'authenticationFailure'}
                    </span>
                  </div>
                ))}
              </div>
              <div className="font-mono text-[10.5px] text-faint mt-1">
                re-run this scan with a different profile, or leave them unbound
              </div>
            </div>
          )}

          {matched.length > 0 && (
            <div className="border-t border-line px-3 py-2.5 flex items-center gap-3">
              <Btn
                tone="accent"
                size="md"
                disabled={selected.size === 0 || commit.isPending || scan.state === 'running'}
                onClick={() => commit.mutate()}
              >
                {commit.isPending
                  ? 'binding…'
                  : `bind ${selected.size} device${selected.size === 1 ? '' : 's'}`}
              </Btn>
              <span className="font-mono text-[10.5px] text-faint">
                selected devices will be bound to {selectedProfile?.name || 'this profile'}
              </span>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

/* --------------------------- Bindings table --------------------------- */

function CredentialList({
  rows,
  profilesById,
  onEdit,
}: {
  rows: SNMPCredential[]
  profilesById: Record<string, SNMPProfile>
  onEdit: (c: SNMPCredential) => void
}) {
  const sorted = useMemo(
    () => [...rows].sort((a, b) => a.exporter.localeCompare(b.exporter)),
    [rows],
  )
  return (
    <div className="border border-line">
      <table className="w-full">
        <thead>
          <tr className="border-b border-line bg-raise">
            <Th>exporter</Th>
            <Th>profile</Th>
            <Th>version</Th>
            <Th align="r">port</Th>
            <Th align="r">interval</Th>
            <Th>updated</Th>
            <Th />
          </tr>
        </thead>
        <tbody>
          {sorted.map((c) => (
            <CredentialRow
              key={c.exporter}
              c={c}
              profile={c.profile_id ? profilesById[c.profile_id] : undefined}
              onEdit={() => onEdit(c)}
            />
          ))}
        </tbody>
      </table>
    </div>
  )
}

function CredentialRow({
  c,
  profile,
  onEdit,
}: {
  c: SNMPCredential
  profile: SNMPProfile | undefined
  onEdit: () => void
}) {
  const qc = useQueryClient()
  const confirm = useAppConfirm()
  const del = useMutation({
    mutationFn: () => api.deleteCredential(c.exporter),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['snmp-credentials'] }),
  })
  const [test, setTest] = useState<SNMPTestResult | null>(null)
  const [testing, setTesting] = useState(false)
  const [walkState, setWalkState] = useState<'idle' | 'queued' | 'error'>('idle')
  const runTest = async () => {
    setTesting(true)
    try {
      const r = await api.testCredential(c.exporter)
      setTest(r)
    } catch (e) {
      setTest({ ok: false, error: (e as Error).message })
    } finally {
      setTesting(false)
    }
  }
  const runWalk = async () => {
    setWalkState('queued')
    try {
      await api.requestSnmpWalk(c.exporter)
    } catch {
      setWalkState('error')
      return
    }
    setTimeout(() => setWalkState('idle'), 4000)
  }

  const usesProfile = !!c.profile_id
  const version = usesProfile ? profile?.version || c.version : c.version

  return (
    <>
      <tr className="border-b border-line/60 hover:bg-surface">
        <td className="px-3 py-1.5 text-[12.5px] font-mono text-text">{c.exporter}</td>
        <td className="px-3 py-1.5 text-[12.5px] font-mono">
          {usesProfile ? (
            profile ? (
              <span className="text-accent">→ {profile.name}</span>
            ) : (
              <span className="text-crit">→ missing profile</span>
            )
          ) : (
            <span className="text-faint">(custom inline)</span>
          )}
        </td>
        <td className="px-3 py-1.5">
          <Tag tone={version === 'v3' ? 'accent' : undefined}>{version || '—'}</Tag>
        </td>
        <td className="px-3 py-1.5 text-right text-[12.5px] font-mono">{c.port}</td>
        <td className="px-3 py-1.5 text-right text-[12.5px] font-mono">
          {Math.round(c.interval_sec / 60)}m
        </td>
        <td className="px-3 py-1.5 text-[11px] font-mono text-faint">
          {c.updated_at ? fmt.time(c.updated_at).slice(0, 19) : '—'}
          {c.updated_by ? ` · ${c.updated_by}` : ''}
        </td>
        <td className="px-3 py-1.5">
          <div className="flex justify-end gap-2">
            <Btn onClick={runTest} disabled={testing}>
              {testing ? 'testing…' : 'test'}
            </Btn>
            <Btn onClick={runWalk} disabled={walkState === 'queued'}>
              {walkState === 'queued'
                ? 'queued · walks ≤30s'
                : walkState === 'error'
                  ? 'walk failed'
                  : 'walk now'}
            </Btn>
            <Btn onClick={onEdit}>edit</Btn>
            <Btn
              tone="crit"
              onClick={async () => {
                const ok = await confirm({
                  title: `Delete SNMP binding for ${c.exporter}?`,
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
      {test && (
        <tr>
          <td colSpan={7} className="bg-raise px-4 py-2.5 font-mono text-[11.5px]">
            {test.ok ? (
              <span className="text-ok">
                ✓ ok · sys_name={test.sys_name || '—'} · interfaces={test.interfaces} ·{' '}
                {test.poll_duration_ms}ms
              </span>
            ) : (
              <span className="text-crit">✗ {test.error || 'failed'}</span>
            )}
            <button onClick={() => setTest(null)} className="ml-3 text-faint hover:text-text">
              dismiss
            </button>
          </td>
        </tr>
      )}
    </>
  )
}

function CredentialForm({
  initial,
  profiles,
  onClose,
}: {
  initial: SNMPCredential | null
  profiles: SNMPProfile[]
  onClose: () => void
}) {
  const qc = useQueryClient()
  const isEdit = !!initial
  const [c, setC] = useState<SNMPCredential>(
    initial ?? {
      exporter: '',
      version: 'v2c',
      port: 161,
      interval_sec: 900,
      profile_id: profiles[0]?.id || '',
      has_community: false,
      has_auth_pass: false,
      has_priv_pass: false,
    },
  )
  const usesProfile = !!c.profile_id
  const [error, setError] = useState<string | null>(null)
  const save = useMutation({
    mutationFn: () => api.putCredential(c),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['snmp-credentials'] })
      onClose()
    },
    onError: (e: Error) => setError(e.message),
  })

  const set = <K extends keyof SNMPCredential>(k: K, v: SNMPCredential[K]) =>
    setC({ ...c, [k]: v })

  const setMode = (mode: 'profile' | 'custom') => {
    if (mode === 'profile') {
      setC((prev) => ({
        ...prev,
        profile_id: prev.profile_id || profiles[0]?.id || '',
      }))
    } else {
      setC((prev) => ({ ...prev, profile_id: '' }))
    }
  }

  return (
    <div className="border border-accent/40 bg-accent-wash px-4 py-4 mb-4">
      <div className="flex items-baseline gap-3 mb-3">
        <span className="text-[11px] uppercase tracking-[0.1em] text-accent font-semibold">
          {isEdit ? 'Edit binding' : 'New binding'}
        </span>
        <button onClick={onClose} className="ml-auto font-mono text-[11px] text-dim hover:text-text">
          cancel
        </button>
      </div>

      <Field label="type">
        <div className="flex gap-1 text-[12px]">
          <ModeRadio mode="profile" active={usesProfile ? 'profile' : 'custom'} onChange={setMode}>
            Use library profile
          </ModeRadio>
          <ModeRadio mode="custom" active={usesProfile ? 'profile' : 'custom'} onChange={setMode}>
            Custom inline
          </ModeRadio>
        </div>
      </Field>

      <div className="grid grid-cols-2 md:grid-cols-4 gap-3 mb-3 mt-3">
        <Field label="exporter">
          <input
            disabled={isEdit}
            value={c.exporter}
            onChange={(e) => set('exporter', e.target.value)}
            placeholder="10.2.0.11"
            className="s-input"
          />
        </Field>
        {!usesProfile && (
          <Field label="version">
            <select
              value={c.version}
              onChange={(e) => set('version', e.target.value as 'v2c' | 'v3')}
              className="s-input"
            >
              <option value="v2c">v2c</option>
              <option value="v3">v3</option>
            </select>
          </Field>
        )}
        <Field label="port">
          <input
            type="number"
            value={c.port}
            onChange={(e) => set('port', Number(e.target.value) || 161)}
            className="s-input"
          />
        </Field>
        <Field label="interval (sec)">
          <input
            type="number"
            value={c.interval_sec}
            onChange={(e) => set('interval_sec', Number(e.target.value) || 900)}
            className="s-input"
          />
        </Field>
      </div>

      {usesProfile && (
        <Field label="profile">
          <select
            value={c.profile_id || ''}
            onChange={(e) => set('profile_id', e.target.value)}
            className="s-input"
          >
            {profiles.length === 0 && <option value="">no profiles configured</option>}
            {profiles.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name} · {p.version}
              </option>
            ))}
          </select>
        </Field>
      )}

      {!usesProfile && c.version === 'v2c' && (
        <Field label={`community ${c.has_community ? '· (already set; leave blank to keep)' : ''}`}>
          <input
            type="password"
            value={c.community || ''}
            onChange={(e) => set('community', e.target.value)}
            placeholder={c.has_community ? '••••••••' : 'public'}
            className="s-input"
          />
        </Field>
      )}

      {!usesProfile && c.version === 'v3' && (
        <div className="grid grid-cols-2 md:grid-cols-3 gap-3">
          <Field label="username">
            <input
              value={c.v3_username || ''}
              onChange={(e) => set('v3_username', e.target.value)}
              placeholder="noc-readonly"
              className="s-input"
            />
          </Field>
          <Field label="auth protocol">
            <select
              value={c.v3_auth_proto || ''}
              onChange={(e) => set('v3_auth_proto', e.target.value)}
              className="s-input"
            >
              {AUTH_PROTOS.map((p) => (
                <option key={p || 'none'} value={p}>
                  {p || 'none'}
                </option>
              ))}
            </select>
          </Field>
          <Field label={`auth passphrase ${c.has_auth_pass ? '· (set)' : ''}`}>
            <input
              type="password"
              value={c.v3_auth_pass || ''}
              onChange={(e) => set('v3_auth_pass', e.target.value)}
              placeholder={c.has_auth_pass ? '••••••••' : ''}
              className="s-input"
            />
          </Field>
          <Field label="priv protocol">
            <select
              value={c.v3_priv_proto || ''}
              onChange={(e) => set('v3_priv_proto', e.target.value)}
              className="s-input"
            >
              {PRIV_PROTOS.map((p) => (
                <option key={p || 'none'} value={p}>
                  {p || 'none'}
                </option>
              ))}
            </select>
          </Field>
          <Field label={`priv passphrase ${c.has_priv_pass ? '· (set)' : ''}`}>
            <input
              type="password"
              value={c.v3_priv_pass || ''}
              onChange={(e) => set('v3_priv_pass', e.target.value)}
              placeholder={c.has_priv_pass ? '••••••••' : ''}
              className="s-input"
            />
          </Field>
          <Field label="context (optional)">
            <input
              value={c.v3_context || ''}
              onChange={(e) => set('v3_context', e.target.value)}
              className="s-input"
            />
          </Field>
        </div>
      )}

      {error && <div className="mt-3 text-crit font-mono text-[12px]">{error}</div>}

      <div className="flex items-center gap-3 mt-4">
        <Btn
          tone="accent"
          size="md"
          disabled={save.isPending || !c.exporter || (usesProfile && !c.profile_id)}
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
      </div>
    </div>
  )
}

/* --------------------------- Shared bits --------------------------- */

function Th({
  children,
  align,
}: {
  children?: React.ReactNode
  align?: 'r'
}) {
  return (
    <th
      className={`text-left text-[10.5px] uppercase tracking-[0.1em] text-faint font-mono font-semibold px-3 py-2 ${
        align === 'r' ? 'text-right' : ''
      }`}
    >
      {children}
    </th>
  )
}

function ModeRadio({
  mode,
  active,
  onChange,
  children,
}: {
  mode: 'profile' | 'custom'
  active: 'profile' | 'custom'
  onChange: (m: 'profile' | 'custom') => void
  children: React.ReactNode
}) {
  const on = mode === active
  return (
    <button
      type="button"
      onClick={() => onChange(mode)}
      className={`px-2.5 py-1 border font-mono text-[11.5px] uppercase tracking-[0.06em] ${
        on
          ? 'border-accent bg-accent-wash text-accent'
          : 'border-line text-dim hover:border-accent hover:text-text'
      }`}
    >
      {children}
    </button>
  )
}
