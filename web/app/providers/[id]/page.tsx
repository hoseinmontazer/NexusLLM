'use client'

import { useState } from 'react'
import { useParams, useRouter } from 'next/navigation'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type CatalogEntry, type ExposureRule, type ExposureMode, type ProjectProviderAccess, type ProviderCredential, type ProviderLiveModel } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { toast } from '@/components/ui/toaster'
import {
  ArrowLeft, RefreshCw, Zap, Search, CheckCircle2,
  XCircle, Loader2, Settings, Shield, Eye, PackageCheck, Users, Globe, KeyRound, Trash2, Plus, Ban,
} from 'lucide-react'

// ── Capability badges ──────────────────────────────────────────────────────────

function CapBadges({ entry }: { entry: CatalogEntry }) {
  return (
    <div className="flex flex-wrap gap-0.5">
      {entry.supports_streaming  && <span title="Streaming"  className="text-[9px] px-1 py-0.5 rounded bg-blue-50  text-blue-700  border border-blue-200">SSE</span>}
      {entry.supports_tools      && <span title="Tools"      className="text-[9px] px-1 py-0.5 rounded bg-green-50 text-green-700 border border-green-200">🔧</span>}
      {entry.supports_vision     && <span title="Vision"     className="text-[9px] px-1 py-0.5 rounded bg-pink-50  text-pink-700  border border-pink-200">👁</span>}
      {entry.supports_audio      && <span title="Audio"      className="text-[9px] px-1 py-0.5 rounded bg-teal-50  text-teal-700  border border-teal-200">🎤</span>}
      {entry.supports_embeddings && <span title="Embedding"  className="text-[9px] px-1 py-0.5 rounded bg-purple-50 text-purple-700 border border-purple-200">🧮</span>}
      {entry.supports_reasoning  && <span title="Reasoning"  className="text-[9px] px-1 py-0.5 rounded bg-indigo-50 text-indigo-700 border border-indigo-200">🧠</span>}
    </div>
  )
}

// ── Register as Public Models panel ───────────────────────────────────────────
//
// Shown inside the Catalog tab when the admin has selected models and clicks
// "Register as Public Models". Each selected provider_model_id gets a row
// where the admin types a public name (pre-filled from the model ID).
// Submits to POST /providers/:id/register-models, which creates models rows
// + model_endpoints rows so the models appear in api.models.list and can be
// granted to teams via team_model_permissions — identical to local models.

interface RegisterEntry {
  providerModelId: string
  publicName: string
  serviceType: string
}

function RegisterPublicModelsPanel({
  providerId,
  selectedIds,
  onDone,
  onCancel,
}: {
  providerId: string
  selectedIds: string[]
  onDone: () => void
  onCancel: () => void
}) {
  const qc = useQueryClient()
  const [entries, setEntries] = useState<RegisterEntry[]>(() =>
    selectedIds.map(id => ({
      providerModelId: id,
      // Derive a clean public name: use last path segment, strip version suffixes
      publicName: id.split('/').pop()?.replace(/:.*$/, '') ?? id,
      serviceType: id.toLowerCase().includes('embed') ? 'EMBEDDING'
        : id.toLowerCase().includes('whisper') || id.toLowerCase().includes('transcri') ? 'STT'
        : id.toLowerCase().includes('tts') || id.toLowerCase().includes('speech') ? 'TTS'
        : 'CHAT',
    }))
  )

  const setEntry = (i: number, field: keyof RegisterEntry, val: string) =>
    setEntries(prev => prev.map((e, idx) => idx === i ? { ...e, [field]: val } : e))

  const registerMut = useMutation({
    mutationFn: () => api.providers.registerModels(
      providerId,
      entries.map(e => ({
        public_name:       e.publicName.trim(),
        provider_model_id: e.providerModelId,
        service_type:      e.serviceType,
      })).filter(e => e.public_name !== '')
    ),
    onSuccess: (r) => {
      const ok = r.results.filter(x => !x.error).length
      const fail = r.results.filter(x => !!x.error)
      if (ok > 0) {
        toast({ title: `${ok} model${ok !== 1 ? 's' : ''} registered as Public Models` })
        qc.invalidateQueries({ queryKey: ['models'] })
      }
      if (fail.length > 0) {
        toast({
          title: `${fail.length} model${fail.length !== 1 ? 's' : ''} failed`,
          description: fail.map(f => `${f.public_name}: ${f.error}`).join(' · '),
          variant: 'destructive',
        })
      }
      onDone()
    },
    onError: (e: any) => toast({ title: 'Registration failed', description: e.message, variant: 'destructive' }),
  })

  return (
    <div className="rounded-md border border-blue-200 bg-blue-50 p-3 space-y-3">
      <div className="flex items-center justify-between">
        <p className="text-xs font-semibold text-blue-900 flex items-center gap-1.5">
          <PackageCheck className="w-3.5 h-3.5" />
          Register {selectedIds.length} model{selectedIds.length !== 1 ? 's' : ''} as Public Models
        </p>
        <button onClick={onCancel} className="text-xs text-blue-600 hover:text-blue-900">✕ Cancel</button>
      </div>

      <p className="text-[11px] text-blue-800">
        Each model gets a <strong>Public Name</strong> — what clients send in the <code>model:</code> field.
        After registration the model appears in the unified model registry and can be granted to teams.
      </p>

      <div className="space-y-1.5 max-h-64 overflow-y-auto pr-1">
        <div className="grid grid-cols-[1fr_1fr_auto] gap-2 text-[10px] font-semibold text-blue-700 px-1">
          <span>Provider Model ID</span><span>Public Name</span><span>Type</span>
        </div>
        {entries.map((e, i) => (
          <div key={e.providerModelId} className="grid grid-cols-[1fr_1fr_auto] gap-2 items-center">
            <span className="font-mono text-[10px] text-muted-foreground truncate bg-white/70 border border-blue-100 rounded px-2 py-1"
              title={e.providerModelId}>
              {e.providerModelId}
            </span>
            <input
              value={e.publicName}
              onChange={ev => setEntry(i, 'publicName', ev.target.value)}
              className="text-xs border border-blue-200 rounded px-2 py-1 bg-white focus:outline-none focus:ring-1 focus:ring-blue-400"
              placeholder="public-name"
            />
            <select
              value={e.serviceType}
              onChange={ev => setEntry(i, 'serviceType', ev.target.value)}
              className="text-[10px] border border-blue-200 rounded px-1 py-1 bg-white focus:outline-none"
            >
              <option value="CHAT">CHAT</option>
              <option value="EMBEDDING">EMBED</option>
              <option value="STT">STT</option>
              <option value="TTS">TTS</option>
              <option value="RERANK">RERANK</option>
              <option value="VISION">VISION</option>
            </select>
          </div>
        ))}
      </div>

      <div className="flex items-center gap-3 pt-1">
        <Button
          size="sm"
          className="h-7 bg-blue-600 hover:bg-blue-700 text-white"
          disabled={registerMut.isPending || entries.every(e => !e.publicName.trim())}
          onClick={() => registerMut.mutate()}
        >
          {registerMut.isPending
            ? <><Loader2 className="w-3 h-3 animate-spin mr-1" />Registering…</>
            : <><PackageCheck className="w-3 h-3 mr-1" />Register Models</>}
        </Button>
        <p className="text-[10px] text-blue-700">
          After this, go to <strong>Teams → Model Access</strong> to grant teams permission to use them.
        </p>
      </div>
    </div>
  )
}

// ── Catalog tab ────────────────────────────────────────────────────────────────

function CatalogTab({ providerId }: { providerId: string }) {
  const qc = useQueryClient()
  const [q, setQ] = useState('')
  const [cap, setCap] = useState('')
  const [tag, setTag] = useState('')
  const [page, setPage] = useState(1)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [showRegisterPanel, setShowRegisterPanel] = useState(false)

  // Catalog entries (paginated)
  const { data, isLoading } = useQuery({
    queryKey: ['catalog', providerId, q, cap, tag, page],
    queryFn: () => api.providers.listCatalog(providerId, {
      q: q || undefined, capability: cap || undefined,
      tag: tag || undefined, page, per_page: 50,
    }),
    refetchInterval: false,
  })

  // Which models already have allow_model rules (map: model_id → rule_id)
  const { data: exposedData, refetch: refetchExposed } = useQuery({
    queryKey: ['exposed-models', providerId],
    queryFn: () => api.providers.listExposedModelIDs(providerId),
  })
  const exposed: Record<string, string> = exposedData?.exposed ?? {}
  const exposedCount = exposedData?.count ?? 0

  const entries = data?.data ?? []
  const total = data?.total ?? 0

  // Bulk expose selected models
  const exposeMut = useMutation({
    mutationFn: (ids: string[]) => api.providers.exposeModels(providerId, ids),
    onSuccess: (r) => {
      toast({ title: `${r.created} model${r.created !== 1 ? 's' : ''} exposed` })
      setSelected(new Set())
      refetchExposed()
      qc.invalidateQueries({ queryKey: ['provider', providerId] })
    },
    onError: (e: any) => toast({ title: 'Failed', description: e.message, variant: 'destructive' }),
  })

  // Bulk hide (remove allow_model rules)
  const hideMut = useMutation({
    mutationFn: (ruleIds: string[]) => api.providers.hideModels(providerId, ruleIds),
    onSuccess: (r) => {
      toast({ title: `${r.hidden} model${r.hidden !== 1 ? 's' : ''} hidden` })
      setSelected(new Set())
      refetchExposed()
      qc.invalidateQueries({ queryKey: ['provider', providerId] })
    },
    onError: (e: any) => toast({ title: 'Failed', description: e.message, variant: 'destructive' }),
  })

  // Toggle a single model's exposure inline
  const toggleModel = (modelId: string) => {
    const ruleId = exposed[modelId]
    if (ruleId) {
      hideMut.mutate([ruleId])
    } else {
      exposeMut.mutate([modelId])
    }
  }

  const allSelected = entries.length > 0 && entries.every(e => selected.has(e.provider_model_id))
  const toggleAll = () => {
    if (allSelected) {
      setSelected(prev => { const n = new Set(prev); entries.forEach(e => n.delete(e.provider_model_id)); return n })
    } else {
      setSelected(prev => { const n = new Set(prev); entries.forEach(e => n.add(e.provider_model_id)); return n })
    }
  }

  const selectedArr = Array.from(selected)
  const selectedExposedRuleIds = selectedArr.map(id => exposed[id]).filter(Boolean)
  const selectedUnexposed = selectedArr.filter(id => !exposed[id])

  return (
    <div className="space-y-3">
      {/* Search + filter bar */}
      <div className="flex flex-wrap gap-2 items-center">
        <div className="flex items-center gap-1.5 border rounded-md px-2.5 h-8 bg-white text-xs flex-1 min-w-48">
          <Search className="w-3 h-3 text-muted-foreground shrink-0" />
          <input className="outline-none bg-transparent w-full" placeholder="search models…"
            value={q} onChange={e => { setQ(e.target.value); setPage(1) }} />
        </div>
        <select className="border rounded-md h-8 px-2 text-xs" value={cap}
          onChange={e => { setCap(e.target.value); setPage(1) }}>
          <option value="">All capabilities</option>
          <option value="tools">Function calling</option>
          <option value="vision">Vision</option>
          <option value="audio">Audio</option>
          <option value="embedding">Embeddings</option>
          <option value="reasoning">Reasoning</option>
        </select>
        <input className="border rounded-md h-8 px-2 text-xs w-24" placeholder="tag…"
          value={tag} onChange={e => { setTag(e.target.value); setPage(1) }} />
        <span className="text-xs text-muted-foreground">{total} models</span>
        <span className="text-xs font-medium text-green-700 ml-auto">
          ✓ {exposedCount} exposed
        </span>
      </div>

      {/* Bulk action bar — shown only when rows are selected */}
      {selected.size > 0 && (
        <div className="flex items-center gap-3 bg-blue-50 border border-blue-200 rounded-md px-3 py-2 text-sm">
          <span className="font-medium text-blue-800">{selected.size} selected</span>
          <div className="flex gap-2 ml-auto">
            {selectedUnexposed.length > 0 && (
              <Button size="sm" className="h-7 bg-green-600 hover:bg-green-700 text-white"
                disabled={exposeMut.isPending}
                onClick={() => exposeMut.mutate(selectedUnexposed)}>
                {exposeMut.isPending ? <Loader2 className="w-3 h-3 animate-spin mr-1" /> : <Eye className="w-3 h-3 mr-1" />}
                Expose {selectedUnexposed.length}
              </Button>
            )}
            {selectedExposedRuleIds.length > 0 && (
              <Button size="sm" variant="outline" className="h-7 text-red-600 border-red-300 hover:bg-red-50"
                disabled={hideMut.isPending}
                onClick={() => hideMut.mutate(selectedExposedRuleIds)}>
                {hideMut.isPending ? <Loader2 className="w-3 h-3 animate-spin mr-1" /> : <XCircle className="w-3 h-3 mr-1" />}
                Hide {selectedExposedRuleIds.length}
              </Button>
            )}
            {/* Register as Public Models — promotes selected catalog entries into
                the unified model registry so teams can be granted access to them */}
            <Button size="sm" variant="outline"
              className="h-7 border-blue-400 text-blue-700 hover:bg-blue-50"
              onClick={() => setShowRegisterPanel(v => !v)}>
              <PackageCheck className="w-3 h-3 mr-1" />
              Register as Public Models
            </Button>
            <Button size="sm" variant="ghost" className="h-7 text-xs"
              onClick={() => { setSelected(new Set()); setShowRegisterPanel(false) }}>
              Clear
            </Button>
          </div>
        </div>
      )}

      {/* Register panel — inline form to set public names before submission */}
      {showRegisterPanel && selected.size > 0 && (
        <RegisterPublicModelsPanel
          providerId={providerId}
          selectedIds={Array.from(selected)}
          onDone={() => { setSelected(new Set()); setShowRegisterPanel(false) }}
          onCancel={() => setShowRegisterPanel(false)}
        />
      )}

      {/* Catalog table */}
      {isLoading ? (
        <div className="flex items-center gap-2 text-sm text-muted-foreground py-8 justify-center">
          <Loader2 className="w-4 h-4 animate-spin" />Loading catalog…
        </div>
      ) : (
        <div className="rounded-md border overflow-hidden">
          <table className="w-full text-xs">
            <thead>
              <tr className="bg-gray-50 border-b text-muted-foreground">
                <th className="px-3 py-2 w-8">
                  <input type="checkbox" checked={allSelected} onChange={toggleAll}
                    className="h-3.5 w-3.5 cursor-pointer" />
                </th>
                <th className="text-left px-3 py-2 font-medium">Model ID</th>
                <th className="text-left px-3 py-2 font-medium">Capabilities</th>
                <th className="text-left px-3 py-2 font-medium">Context</th>
                <th className="text-left px-3 py-2 font-medium">$/1M in</th>
                <th className="text-left px-3 py-2 font-medium">Tags</th>
                <th className="text-center px-3 py-2 font-medium w-24">Exposed</th>
              </tr>
            </thead>
            <tbody className="divide-y">
              {entries.map(e => {
                const isExposed = !!exposed[e.provider_model_id]
                const isSelected = selected.has(e.provider_model_id)
                return (
                  <tr key={e.id}
                    className={`transition-colors ${isSelected ? 'bg-blue-50' : isExposed ? 'bg-green-50/40' : 'hover:bg-gray-50'}`}>
                    <td className="px-3 py-2">
                      <input type="checkbox" checked={isSelected}
                        onChange={() => setSelected(prev => {
                          const n = new Set(prev)
                          isSelected ? n.delete(e.provider_model_id) : n.add(e.provider_model_id)
                          return n
                        })}
                        className="h-3.5 w-3.5 cursor-pointer" />
                    </td>
                    <td className="px-3 py-2 font-mono max-w-xs truncate" title={e.provider_model_id}>
                      {e.provider_model_id}
                    </td>
                    <td className="px-3 py-2"><CapBadges entry={e} /></td>
                    <td className="px-3 py-2 tabular-nums whitespace-nowrap">
                      {e.context_length ? `${Math.round(e.context_length / 1000)}K` : '—'}
                    </td>
                    <td className="px-3 py-2 tabular-nums">
                      {e.input_cost_per_1m != null ? `$${e.input_cost_per_1m}` : '—'}
                    </td>
                    <td className="px-3 py-2">
                      <div className="flex flex-wrap gap-0.5">
                        {(e.tags ?? []).slice(0, 3).map(t => (
                          <span key={t} className="text-[9px] px-1 py-0.5 rounded bg-gray-100 text-gray-600">{t}</span>
                        ))}
                      </div>
                    </td>
                    <td className="px-3 py-2 text-center">
                      {/* Toggle switch */}
                      <button
                        onClick={() => toggleModel(e.provider_model_id)}
                        disabled={exposeMut.isPending || hideMut.isPending}
                        className={`relative inline-flex h-5 w-9 items-center rounded-full transition-colors focus:outline-none ${
                          isExposed ? 'bg-green-500' : 'bg-gray-200'
                        }`}
                        title={isExposed ? 'Click to hide' : 'Click to expose'}
                      >
                        <span className={`inline-block h-3.5 w-3.5 transform rounded-full bg-white shadow transition-transform ${
                          isExposed ? 'translate-x-4' : 'translate-x-0.5'
                        }`} />
                      </button>
                    </td>
                  </tr>
                )
              })}
              {entries.length === 0 && (
                <tr>
                  <td colSpan={7} className="px-3 py-10 text-center text-muted-foreground">
                    No catalog entries. Trigger a sync first.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      )}

      {/* Pagination */}
      {total > 50 && (
        <div className="flex items-center gap-2 justify-between">
          <span className="text-xs text-muted-foreground">
            Showing {(page - 1) * 50 + 1}–{Math.min(page * 50, total)} of {total}
          </span>
          <div className="flex gap-2">
            <Button size="sm" variant="outline" disabled={page === 1} onClick={() => setPage(p => p - 1)}>← Prev</Button>
            <span className="text-xs text-muted-foreground self-center">page {page} of {Math.ceil(total / 50)}</span>
            <Button size="sm" variant="outline" disabled={page * 50 >= total} onClick={() => setPage(p => p + 1)}>Next →</Button>
          </div>
        </div>
      )}

      {/* Hint when nothing is exposed yet */}
      {exposedCount === 0 && total > 0 && (
        <div className="rounded-md border border-amber-200 bg-amber-50 px-4 py-3 text-xs text-amber-800">
          <strong>No models exposed yet.</strong> Toggle individual models on, or select multiple and click <em>Expose</em>.
          Only exposed models are routable by clients.
        </div>
      )}
    </div>
  )
}

// ── Rules tab ──────────────────────────────────────────────────────────────────

function RulesTab({ providerId }: { providerId: string }) {
  const qc = useQueryClient()
  const [showCreate, setShowCreate] = useState(false)
  const [ruleType, setRuleType] = useState('allow_pattern')
  const [pattern, setPattern] = useState('')
  const [denyTags, setDenyTags] = useState('')
  const [priority, setPriority] = useState(100)

  const { data } = useQuery({ queryKey: ['rules', providerId], queryFn: () => api.providers.listRules(providerId) })
  const rules = data?.data ?? []

  const preview = useQuery({
    queryKey: ['rules-preview', providerId],
    queryFn: () => api.providers.previewRules(providerId),
    enabled: false,
  })

  const createRule = useMutation({
    mutationFn: () => api.providers.createRule(providerId, {
      rule_type: ruleType,
      pattern: pattern || undefined,
      deny_tags: denyTags ? denyTags.split(',').map(s => s.trim()).filter(Boolean) : undefined,
      priority,
    }),
    onSuccess: () => {
      toast({ title: 'Rule created' })
      qc.invalidateQueries({ queryKey: ['rules', providerId] })
      setShowCreate(false); setPattern(''); setDenyTags('')
    },
    onError: (e: any) => toast({ title: 'Failed', description: e.message, variant: 'destructive' }),
  })

  const deleteRule = useMutation({
    mutationFn: (rid: string) => api.providers.deleteRule(providerId, rid),
    onSuccess: () => { toast({ title: 'Rule deleted' }); qc.invalidateQueries({ queryKey: ['rules', providerId] }) },
    onError: (e: any) => toast({ title: 'Failed', description: e.message, variant: 'destructive' }),
  })

  const ruleColors: Record<string, string> = {
    allow_model: 'bg-green-50 text-green-700 border-green-200',
    allow_pattern: 'bg-blue-50 text-blue-700 border-blue-200',
    deny_pattern: 'bg-red-50 text-red-700 border-red-200',
    capability_filter: 'bg-purple-50 text-purple-700 border-purple-200',
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <p className="text-sm text-muted-foreground">Rules are evaluated in priority order. Deny rules fire before allow rules.</p>
        <div className="flex gap-2">
          <Button size="sm" variant="outline" onClick={() => preview.refetch()}>
            <Eye className="w-3.5 h-3.5 mr-1" />Preview
          </Button>
          <Button size="sm" onClick={() => setShowCreate(v => !v)}>
            {showCreate ? 'Cancel' : '+ Add Rule'}
          </Button>
        </div>
      </div>

      {preview.data && (
        <div className="rounded-md border bg-blue-50 px-4 py-3 text-sm">
          <span className="text-green-700 font-medium">✓ {preview.data.exposed_count} models exposed</span>
          <span className="text-red-500 font-medium ml-4">✗ {preview.data.blocked_count} models blocked</span>
        </div>
      )}

      {showCreate && (
        <div className="rounded-md border p-3 space-y-3 bg-gray-50">
          <p className="text-xs font-semibold">New rule</p>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <Label className="text-xs">Type</Label>
              <select className="w-full border rounded-md h-8 px-2 text-xs mt-1" value={ruleType} onChange={e => setRuleType(e.target.value)}>
                <option value="allow_model">Allow model (exact)</option>
                <option value="allow_pattern">Allow pattern (glob)</option>
                <option value="deny_pattern">Deny pattern (glob)</option>
                <option value="capability_filter">Capability filter</option>
              </select>
            </div>
            <div>
              <Label className="text-xs">Priority <span className="text-muted-foreground font-normal">(lower = higher priority)</span></Label>
              <Input type="number" value={priority} onChange={e => setPriority(Number(e.target.value))} className="mt-1 h-8 text-xs" />
            </div>
          </div>
          {(ruleType === 'allow_pattern' || ruleType === 'deny_pattern' || ruleType === 'allow_model') && (
            <div>
              <Label className="text-xs">Pattern / Model ID <span className="text-muted-foreground font-normal">(glob: openai/*, *:free)</span></Label>
              <Input value={pattern} onChange={e => setPattern(e.target.value)} className="mt-1 h-8 font-mono text-xs" placeholder="openai/*" />
            </div>
          )}
          <div>
            <Label className="text-xs">Deny tags <span className="text-muted-foreground font-normal">(comma-separated: free,preview,beta)</span></Label>
            <Input value={denyTags} onChange={e => setDenyTags(e.target.value)} className="mt-1 h-8 text-xs" placeholder="free,preview" />
          </div>
          <Button size="sm" onClick={() => createRule.mutate()} disabled={createRule.isPending}>
            {createRule.isPending ? <Loader2 className="w-3 h-3 animate-spin mr-1" /> : null}Create rule
          </Button>
        </div>
      )}

      <div className="space-y-2">
        {rules.length === 0 && <p className="text-sm text-muted-foreground text-center py-6">No rules — everything is denied by default. Add allow rules to expose models.</p>}
        {rules.map((r: ExposureRule) => (
          <div key={r.id} className={`flex items-center justify-between rounded-md border px-3 py-2 text-xs ${ruleColors[r.rule_type] ?? 'bg-gray-50 text-gray-700 border-gray-200'}`}>
            <div className="flex items-center gap-3">
              <span className="font-semibold uppercase">{r.rule_type.replace('_', ' ')}</span>
              {r.pattern && <span className="font-mono">{r.pattern}</span>}
              {r.model_id && <span className="font-mono">{r.model_id}</span>}
              {r.deny_tags_raw && <span className="opacity-70">deny: {r.deny_tags_raw}</span>}
              <span className="opacity-60">priority {r.priority}</span>
            </div>
            <Button size="sm" variant="ghost" className="h-6 px-2 text-xs" onClick={() => deleteRule.mutate(r.id)}>✕</Button>
          </div>
        ))}
      </div>
    </div>
  )
}

// ── ExposureMode selector ─────────────────────────────────────────────────────

const EXPOSURE_META: Record<string, { label: string; desc: string; color: string; bg: string; border: string }> = {
  managed: {
    label: 'Managed',
    desc: 'Administrators explicitly register Public Models. Only registered models appear in GET /v1/models. Uses team_model_permissions for authorization.',
    color: 'text-gray-700',
    bg: 'bg-gray-50',
    border: 'border-gray-300',
  },
  catalog: {
    label: 'Catalog',
    desc: 'Provider catalogue exposed directly as virtual models — no registration required. GET /v1/models returns prefix/provider-model-id names. Uses project_provider_access for authorization.',
    color: 'text-violet-700',
    bg: 'bg-violet-50',
    border: 'border-violet-400',
  },
  hybrid: {
    label: 'Hybrid',
    desc: 'Both mechanisms run simultaneously. Registered Public Models AND virtual catalog models are visible. Managed authorization for Public Models, project_provider_access for virtual models.',
    color: 'text-blue-700',
    bg: 'bg-blue-50',
    border: 'border-blue-400',
  },
}

function ExposureModeSelector({
  current,
  onChange,
  disabled,
}: {
  current: string
  onChange: (m: ExposureMode) => void
  disabled?: boolean
}) {
  return (
    <div className="space-y-2">
      <p className="text-xs font-semibold text-muted-foreground uppercase tracking-wide">Exposure Mode</p>
      <div className="grid grid-cols-3 gap-2">
        {(['managed', 'catalog', 'hybrid'] as ExposureMode[]).map(m => {
          const meta = EXPOSURE_META[m]
          const active = current === m
          return (
            <button
              key={m}
              type="button"
              disabled={disabled}
              onClick={() => onChange(m)}
              className={`rounded-lg border-2 px-3 py-2.5 text-left transition-all ${
                active
                  ? `${meta.border} ${meta.bg} ${meta.color}`
                  : 'border-gray-200 hover:border-gray-300 hover:bg-gray-50 text-gray-600'
              } ${disabled ? 'opacity-50 cursor-not-allowed' : 'cursor-pointer'}`}
            >
              <div className="flex items-center gap-1.5 mb-1">
                {active && (
                  <CheckCircle2 className={`w-3 h-3 shrink-0 ${meta.color}`} />
                )}
                <span className="text-xs font-semibold">{meta.label}</span>
              </div>
              <p className="text-[10px] leading-relaxed text-muted-foreground">{meta.desc}</p>
            </button>
          )
        })}
      </div>
      {(current === 'catalog' || current === 'hybrid') && (
        <div className="rounded-md bg-violet-50 border border-violet-200 px-3 py-2 text-xs text-violet-800 space-y-1">
          <p><strong>Next steps for {current} mode:</strong></p>
          <ol className="list-decimal pl-4 space-y-0.5">
            <li>Trigger a catalog sync (Sync Catalog button above)</li>
            <li>Add exposure rules in the <strong>Exposure Rules</strong> tab to control which models are visible</li>
            <li>Go to <strong>Projects → Provider Access</strong> to grant projects access to this provider</li>
          </ol>
        </div>
      )}
    </div>
  )
}

// ── Overview tab ───────────────────────────────────────────────────────────────

function OverviewTab({ providerId }: { providerId: string }) {
  const qc = useQueryClient()
  const { data: p, isLoading } = useQuery({
    queryKey: ['provider', providerId],
    queryFn: () => api.providers.get(providerId),
  })

  const [baseUrl, setBaseUrl] = useState('')
  const [proxyUrl, setProxyUrl] = useState('')
  const [apiKey, setApiKey] = useState('')
  const [exposePrefix, setExposePrefix] = useState('')

  // Sync defaults when provider loads
  const [initialised, setInitialised] = useState(false)
  if (p && !initialised) {
    setBaseUrl(p.base_url)
    setProxyUrl(p.proxy_url ?? '')
    setExposePrefix(p.catalog_expose_prefix ?? '')
    setInitialised(true)
  }

  const updateMut = useMutation({
    mutationFn: (b: Parameters<typeof api.providers.update>[1]) => api.providers.update(providerId, b),
    onSuccess: () => {
      toast({ title: 'Provider updated' })
      qc.invalidateQueries({ queryKey: ['provider', providerId] })
      qc.invalidateQueries({ queryKey: ['providers'] })
    },
    onError: (e: any) => toast({ title: 'Failed', description: e.message, variant: 'destructive' }),
  })

  const exposureMut = useMutation({
    mutationFn: (mode: ExposureMode) => api.providers.update(providerId, { exposure_mode: mode }),
    onSuccess: (_, mode) => {
      toast({ title: `Exposure mode → ${mode}` })
      qc.invalidateQueries({ queryKey: ['provider', providerId] })
      qc.invalidateQueries({ queryKey: ['providers'] })
    },
    onError: (e: any) => toast({ title: 'Failed to change mode', description: e.message, variant: 'destructive' }),
  })

  if (isLoading || !p) return <p className="text-sm text-muted-foreground">Loading…</p>

  const currentMode: ExposureMode = (p.exposure_mode as ExposureMode) || 'managed'

  return (
    <div className="space-y-6 max-w-2xl">
      {/* Stats row */}
      <div className="grid grid-cols-4 gap-4 text-sm">
        <div className="rounded-lg border bg-white p-3 space-y-0.5">
          <p className="text-xs text-muted-foreground">Health</p>
          <p className="font-medium capitalize">{p.health}</p>
          <p className="text-[10px] text-muted-foreground">
            {p.last_health_check ? new Date(p.last_health_check).toLocaleString() : 'not checked'}
          </p>
        </div>
        <div className="rounded-lg border bg-white p-3 space-y-0.5">
          <p className="text-xs text-muted-foreground">Catalog models</p>
          <p className="font-medium tabular-nums">{p.catalog_model_count}</p>
          <p className="text-[10px] text-muted-foreground">{p.catalog_sync_status}</p>
        </div>
        <div className="rounded-lg border bg-white p-3 space-y-0.5">
          <p className="text-xs text-muted-foreground">Last sync</p>
          <p className="font-medium text-xs">
            {p.catalog_last_synced_at ? new Date(p.catalog_last_synced_at).toLocaleDateString() : 'never'}
          </p>
          <p className="text-[10px] text-muted-foreground">
            {p.catalog_last_synced_at ? new Date(p.catalog_last_synced_at).toLocaleTimeString() : '—'}
          </p>
        </div>
        <div className="rounded-lg border bg-white p-3 space-y-0.5">
          <p className="text-xs text-muted-foreground">Transport</p>
          <p className="font-medium text-xs truncate font-mono">{p.proxy_url || 'direct'}</p>
          <p className="text-[10px] text-muted-foreground">backend: {p.backend_type}</p>
        </div>
      </div>

      {p.catalog_sync_error && (
        <div className="rounded-md bg-red-50 border border-red-200 px-3 py-2 text-xs text-red-700">
          <strong>Sync error:</strong> {p.catalog_sync_error}
        </div>
      )}

      {/* Exposure mode selector */}
      <div className="rounded-lg border bg-white p-4">
        <ExposureModeSelector
          current={currentMode}
          onChange={(m) => exposureMut.mutate(m)}
          disabled={exposureMut.isPending}
        />
      </div>

      {/* Connection settings */}
      <div className="rounded-lg border bg-white p-4 space-y-3">
        <p className="text-xs font-semibold">Connection Settings</p>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <Label className="text-xs">Base URL</Label>
            <Input
              value={baseUrl}
              onChange={e => setBaseUrl(e.target.value)}
              className="mt-1 font-mono text-xs h-8"
            />
          </div>
          <div>
            <Label className="text-xs">Outbound Proxy</Label>
            <Input
              value={proxyUrl}
              onChange={e => setProxyUrl(e.target.value)}
              className="mt-1 font-mono text-xs h-8"
              placeholder="socks5://host:port"
            />
          </div>
          <div>
            <Label className="text-xs">New API Key <span className="text-muted-foreground font-normal">(leave blank to keep current)</span></Label>
            <Input
              type="password"
              value={apiKey}
              onChange={e => setApiKey(e.target.value)}
              className="mt-1 h-8"
              placeholder="sk-…"
              autoComplete="new-password"
            />
          </div>
          <div>
            <Label className="text-xs">Expose Prefix</Label>
            <Input
              value={exposePrefix}
              onChange={e => setExposePrefix(e.target.value)}
              className="mt-1 font-mono text-xs h-8"
              placeholder={p.name}
            />
            <p className="text-[10px] text-muted-foreground mt-0.5">
              Virtual models appear as <code className="bg-gray-100 px-0.5 rounded">{exposePrefix || p.name}/model-id</code>
            </p>
          </div>
        </div>
        <Button
          size="sm"
          onClick={() => updateMut.mutate({
            base_url: baseUrl || undefined,
            proxy_url: proxyUrl !== undefined ? proxyUrl : undefined,
            api_key: apiKey || undefined,
            catalog_expose_prefix: exposePrefix || undefined,
          })}
          disabled={updateMut.isPending}
        >
          {updateMut.isPending ? 'Saving…' : 'Save Connection Settings'}
        </Button>
      </div>

      {/* Sync settings */}
      <div className="rounded-lg border bg-white p-4 space-y-3">
        <p className="text-xs font-semibold">Catalog Sync</p>
        <div className="flex items-center gap-4 text-sm">
          <label className="flex items-center gap-2 cursor-pointer">
            <input
              type="checkbox"
              defaultChecked={p.catalog_sync_enabled}
              className="h-4 w-4"
              onChange={e => updateMut.mutate({ catalog_sync_enabled: e.target.checked })}
            />
            Enable automatic sync
          </label>
          <div className="flex items-center gap-2">
            <Label className="text-xs">Interval (s)</Label>
            <Input
              type="number"
              defaultValue={p.catalog_sync_interval}
              className="w-24 h-8 text-xs"
              onBlur={e => {
                const v = Number(e.target.value)
                if (v > 0) updateMut.mutate({ catalog_sync_interval: v })
              }}
            />
          </div>
        </div>
        <p className="text-[10px] text-muted-foreground">
          Sync fetches the provider's <code>/v1/models</code> list and updates <code>provider_remote_models</code>.
          Capability flags (tools, vision, etc.) are preserved across syncs.
        </p>
      </div>
    </div>
  )
}


// ── Project Access tab (shown on provider detail page) ────────────────────────
// Read-only summary of which projects have been granted access to this provider.
// Grant management lives in Projects → Provider Access (single source of truth).

function ProjectAccessTab({ providerId, providerName }: { providerId: string; providerName: string }) {
  const qc = useQueryClient()

  // Load all projects so we can map IDs to names and check grants per-project.
  const { data: projectsData, isLoading: projectsLoading } = useQuery({
    queryKey: ['projects'],
    queryFn: () => api.projects.list(),
    staleTime: 60_000,
  })
  const allProjects = projectsData?.data ?? []

  // For each project, check whether it has a grant for this provider.
  // We load per-project data lazily only when a project is selected.
  const [selectedProjectId, setSelectedProjectId] = useState<string | null>(null)

  const { data: grantData, isLoading: grantLoading } = useQuery({
    queryKey: ['project-provider-access', selectedProjectId],
    queryFn: () => api.providerAccess.list(selectedProjectId!),
    enabled: !!selectedProjectId,
    staleTime: 30_000,
  })

  const grantsForThisProvider = (grantData?.data ?? []).filter(
    g => g.provider_id === providerId,
  )

  const revokeMut = useMutation({
    mutationFn: (pid: string) =>
      api.providerAccess.revoke(selectedProjectId!, pid),
    onSuccess: () => {
      toast({ title: 'Access revoked' })
      qc.invalidateQueries({ queryKey: ['project-provider-access', selectedProjectId] })
    },
    onError: (e: any) => toast({ title: 'Revoke failed', description: e.message, variant: 'destructive' }),
  })

  return (
    <div className="space-y-5 max-w-2xl">
      {/* Explainer */}
      <div className="rounded-md bg-violet-50 border border-violet-200 px-4 py-3 text-xs text-violet-800 space-y-1">
        <p className="font-semibold flex items-center gap-1.5">
          <Users className="w-3.5 h-3.5" />Project-level Authorization
        </p>
        <p>
          This view shows which projects have been granted access to virtual models from this provider.
          To grant or edit access, open the project and go to its{' '}
          <strong>Provider Access</strong> tab — that is the single place to manage grants.
        </p>
      </div>

      {/* Project selector */}
      <div className="rounded-lg border bg-white p-4 space-y-3">
        <p className="text-xs font-semibold">Check a project's access</p>
        <div className="flex gap-2 items-center">
          <select
            className="flex-1 border rounded-md h-9 px-3 text-sm"
            value={selectedProjectId ?? ''}
            onChange={e => setSelectedProjectId(e.target.value || null)}
          >
            <option value="">— select a project —</option>
            {allProjects.map(p => (
              <option key={p.id} value={p.id}>{p.name}</option>
            ))}
          </select>
        </div>

        {selectedProjectId && grantLoading && (
          <p className="text-xs text-muted-foreground flex items-center gap-1.5">
            <Loader2 className="w-3.5 h-3.5 animate-spin" />Loading…
          </p>
        )}

        {selectedProjectId && !grantLoading && grantsForThisProvider.length === 0 && (
          <div className="rounded-md bg-gray-50 border px-3 py-3 text-xs text-muted-foreground space-y-2">
            <p>This project has no access grant for <strong>{providerName}</strong>.</p>
            <p>
              To grant access, open the project in{' '}
              <a
                href={`/projects/${selectedProjectId}`}
                className="text-blue-600 underline hover:no-underline"
              >
                Projects → {allProjects.find(p => p.id === selectedProjectId)?.name ?? selectedProjectId}
              </a>{' '}
              and go to its <strong>Provider Access</strong> tab.
            </p>
          </div>
        )}

        {grantsForThisProvider.length > 0 && (
          <div className="space-y-2">
            {grantsForThisProvider.map(g => (
              <div key={g.id} className={`rounded-lg border px-3 py-2.5 text-xs space-y-1.5 ${
                g.enabled ? 'border-violet-200 bg-violet-50/40' : 'border-gray-200 bg-gray-50 opacity-60'
              }`}>
                <div className="flex items-start justify-between gap-3">
                  <div className="space-y-1 flex-1">
                    <div className="flex items-center gap-2 flex-wrap">
                      <span className={`px-2 py-0.5 rounded-full text-[10px] font-medium border ${
                        g.enabled
                          ? 'bg-green-100 text-green-700 border-green-200'
                          : 'bg-gray-100 text-gray-500 border-gray-200'
                      }`}>
                        {g.enabled ? 'active' : 'revoked'}
                      </span>
                      <span className="text-muted-foreground">
                        granted {new Date(g.created_at).toLocaleDateString()}
                      </span>
                    </div>
                    {g.allowed_prefixes?.length > 0 && (
                      <div className="flex flex-wrap items-center gap-1">
                        <span className="text-muted-foreground">Allow:</span>
                        {g.allowed_prefixes.map(p => (
                          <code key={p} className="bg-white border border-violet-200 px-1.5 py-0.5 rounded text-violet-700">{p}</code>
                        ))}
                      </div>
                    )}
                    {g.denied_prefixes?.length > 0 && (
                      <div className="flex flex-wrap items-center gap-1">
                        <span className="text-muted-foreground">Deny:</span>
                        {g.denied_prefixes.map(p => (
                          <code key={p} className="bg-white border border-red-200 px-1.5 py-0.5 rounded text-red-700">{p}</code>
                        ))}
                      </div>
                    )}
                    {!g.allowed_prefixes?.length && !g.denied_prefixes?.length && (
                      <span className="text-green-700 flex items-center gap-1">
                        <CheckCircle2 className="w-3 h-3" />All virtual models allowed
                      </span>
                    )}
                  </div>
                  <div className="flex gap-1 shrink-0">
                    <a
                      href={`/projects/${selectedProjectId}`}
                      className="text-[10px] px-2 py-1 rounded border border-blue-200 text-blue-700 hover:bg-blue-50 transition-colors"
                    >
                      Edit in project →
                    </a>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-7 px-2 text-xs text-red-600 hover:bg-red-50"
                      disabled={revokeMut.isPending || !g.enabled}
                      onClick={() => revokeMut.mutate(g.provider_id)}
                    >
                      Revoke
                    </Button>
                  </div>
                </div>
              </div>
            ))}
          </div>
        )}

        {!selectedProjectId && (
          <p className="text-xs text-muted-foreground">
            Select a project above to see its current access grant for this provider.
          </p>
        )}
      </div>

      {/* Quick navigation hint */}
      <div className="rounded-md border bg-white px-4 py-3 text-xs text-muted-foreground">
        <p className="font-medium text-foreground mb-1">Where to manage grants</p>
        <p>
          Go to <strong>Projects</strong>, open any project, and select the{' '}
          <strong>Provider Access</strong> tab to grant, edit prefix rules, or revoke access.
          Changes take effect in the gateway within 60 seconds.
        </p>
      </div>
    </div>
  )
}

// ── Credentials tab ─────────────────────────────────────────────────────────────
// Manages the named credential pool for this provider (migration 062). This is
// what lets two projects hitting the same provider (e.g. two OpenRouter
// accounts) land on two different upstream tokens — see the "Project Access"
// tab, where each grant is pinned to one of these credentials. Secrets are
// entered here once, encrypted server-side, and never shown again.

function CredentialsTab({ providerId, providerName }: { providerId: string; providerName: string }) {
  const qc = useQueryClient()
  const { data, isLoading } = useQuery({
    queryKey: ['provider-credentials', providerId],
    queryFn: () => api.providerCredentials.list(providerId),
    refetchInterval: 30_000,
  })
  const credentials: ProviderCredential[] = data?.data ?? []

  const [showForm, setShowForm] = useState(false)
  const [name, setName] = useState('')
  const [secret, setSecret] = useState('')
  const [isDefault, setIsDefault] = useState(credentials.length === 0)

  const createMut = useMutation({
    mutationFn: () => api.providerCredentials.create(providerId, { name, secret, is_default: isDefault }),
    onSuccess: (c) => {
      toast({ title: 'Credential created', description: `"${c.name}" is ready to assign to a project.` })
      qc.invalidateQueries({ queryKey: ['provider-credentials', providerId] })
      setShowForm(false); setName(''); setSecret(''); setIsDefault(false)
    },
    onError: (e: any) => toast({ title: 'Create failed', description: e.message, variant: 'destructive' }),
  })

  const toggleMut = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) =>
      api.providerCredentials.update(providerId, id, { enabled }),
    onSuccess: () => {
      toast({ title: 'Credential updated' })
      qc.invalidateQueries({ queryKey: ['provider-credentials', providerId] })
    },
    onError: (e: any) => toast({ title: 'Update failed', description: e.message, variant: 'destructive' }),
  })

  const setDefaultMut = useMutation({
    mutationFn: (id: string) => api.providerCredentials.update(providerId, id, { is_default: true }),
    onSuccess: () => {
      toast({ title: 'Default credential changed' })
      qc.invalidateQueries({ queryKey: ['provider-credentials', providerId] })
    },
    onError: (e: any) => toast({ title: 'Update failed', description: e.message, variant: 'destructive' }),
  })

  const deleteMut = useMutation({
    mutationFn: (id: string) => api.providerCredentials.delete(providerId, id),
    onSuccess: (r) => {
      toast({ title: 'Credential deleted', description: r.note })
      qc.invalidateQueries({ queryKey: ['provider-credentials', providerId] })
    },
    onError: (e: any) => toast({ title: 'Delete failed', description: e.message, variant: 'destructive' }),
  })

  return (
    <div className="space-y-5 max-w-2xl">
      <div className="rounded-md bg-amber-50 border border-amber-200 px-4 py-3 text-xs text-amber-800 space-y-1">
        <p className="font-semibold flex items-center gap-1.5">
          <KeyRound className="w-3.5 h-3.5" />Multi-Credential Routing
        </p>
        <p>
          Add one credential per upstream account (e.g. two different {providerName} API tokens for two
          different apps). Assign each to a project in that project's <strong>Provider Access</strong> tab.
          A project with no credential assigned uses whichever one is marked <strong>default</strong> below.
        </p>
        <p>Secrets are encrypted at rest and never displayed again after creation — not here, not in any API response.</p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-base flex items-center justify-between">
            <span className="flex items-center gap-2"><KeyRound className="w-4 h-4" />Credentials</span>
            {!showForm && (
              <Button size="sm" onClick={() => { setShowForm(true); setIsDefault(credentials.length === 0) }}>
                <Plus className="w-3.5 h-3.5 mr-1" />Add credential
              </Button>
            )}
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          {isLoading && (
            <p className="text-sm text-muted-foreground flex items-center gap-2">
              <Loader2 className="w-3.5 h-3.5 animate-spin" />Loading…
            </p>
          )}

          {!isLoading && credentials.length === 0 && !showForm && (
            <div className="rounded-lg border bg-white p-6 text-center space-y-1">
              <KeyRound className="w-6 h-6 mx-auto text-muted-foreground opacity-30" />
              <p className="text-sm text-muted-foreground">
                No named credentials yet — every project shares this provider's single legacy API key
                (set in the Overview tab) until you add one.
              </p>
            </div>
          )}

          {credentials.map(c => (
            <div key={c.id} className={`rounded-lg border p-3 flex items-center justify-between gap-3 ${
              c.enabled ? 'border-amber-200 bg-amber-50/40' : 'border-gray-200 bg-gray-50 opacity-60'
            }`}>
              <div className="space-y-0.5">
                <div className="flex items-center gap-2 flex-wrap">
                  <span className="font-semibold text-sm">{c.name}</span>
                  {c.is_default && (
                    <span className="text-[10px] px-1.5 py-0.5 rounded-full border font-medium bg-violet-100 text-violet-700 border-violet-200">
                      default
                    </span>
                  )}
                  <span className={`text-[10px] px-1.5 py-0.5 rounded-full border font-medium ${
                    c.enabled ? 'bg-green-100 text-green-700 border-green-200' : 'bg-gray-100 text-gray-500 border-gray-200'
                  }`}>
                    {c.enabled ? 'active' : 'disabled'}
                  </span>
                </div>
                <p className="text-[10px] text-muted-foreground">
                  Assigned to {c.assigned_count} project{c.assigned_count === 1 ? '' : 's'}
                  {c.last_used_at && ` · Last used ${new Date(c.last_used_at).toLocaleString()}`}
                </p>
              </div>
              <div className="flex gap-1 shrink-0">
                {!c.is_default && (
                  <Button size="sm" variant="ghost" className="h-7 px-2 text-xs"
                    disabled={setDefaultMut.isPending}
                    onClick={() => setDefaultMut.mutate(c.id)}>
                    Make default
                  </Button>
                )}
                <Button size="sm" variant="ghost" className="h-7 px-2 text-xs"
                  disabled={toggleMut.isPending}
                  onClick={() => toggleMut.mutate({ id: c.id, enabled: !c.enabled })}>
                  {c.enabled ? <><Ban className="w-3 h-3 mr-1" />Disable</> : 'Enable'}
                </Button>
                <Button size="sm" variant="ghost" className="h-7 px-2 text-xs text-red-600 hover:text-red-700 hover:bg-red-50"
                  disabled={deleteMut.isPending}
                  onClick={() => deleteMut.mutate(c.id)}>
                  <Trash2 className="w-3 h-3" />
                </Button>
              </div>
            </div>
          ))}

          {showForm && (
            <div className="space-y-3 rounded-lg border p-3">
              <div>
                <Label className="text-xs">Name <span className="text-muted-foreground font-normal">(internal label, e.g. "production-app-a")</span></Label>
                <Input value={name} onChange={e => setName(e.target.value)} placeholder="production-app-a" className="mt-1" />
              </div>
              <div>
                <Label className="text-xs">Secret <span className="text-muted-foreground font-normal">(the actual {providerName} API token — encrypted on save, shown only now)</span></Label>
                <Input type="password" value={secret} onChange={e => setSecret(e.target.value)} placeholder="sk-or-v1-…" className="mt-1 font-mono" />
              </div>
              <label className="flex items-center gap-2 text-xs cursor-pointer select-none">
                <input type="checkbox" checked={isDefault} onChange={e => setIsDefault(e.target.checked)} className="h-3.5 w-3.5" />
                Make this the provider's default credential (used by any project with no credential pinned)
              </label>
              <div className="flex gap-2">
                <Button size="sm" disabled={createMut.isPending || !name || !secret}
                  onClick={() => createMut.mutate()}>
                  {createMut.isPending ? <><Loader2 className="w-3 h-3 animate-spin mr-1" />Creating…</> : 'Create credential'}
                </Button>
                <Button size="sm" variant="outline" onClick={() => { setShowForm(false); setName(''); setSecret('') }}>
                  Cancel
                </Button>
              </div>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}

// ── Live Models tab ────────────────────────────────────────────────────────────
// Calls GET /admin/v1/providers/:id/live-models on the admin server, which
// proxies the request to the provider using stored credentials and transport
// config (including proxy). No client-side API key or localStorage needed.

function LiveModelsTab({ providerId, providerDisplayName }: { providerId: string; providerDisplayName: string }) {
  const [q, setQ] = useState('')

  const { data, isLoading, error, refetch, isFetching } = useQuery({
    queryKey: ['provider-live-models', providerId],
    queryFn: () => api.providers.liveModels(providerId),
    staleTime: 5 * 60 * 1000,
    retry: false,
  })

  const allModels = data?.data ?? []
  const models = allModels.filter(m =>
    !q || m.id.toLowerCase().includes(q.toLowerCase()) ||
    (m.name ?? '').toLowerCase().includes(q.toLowerCase())
  )

  const fmtCtx = (n?: number) => {
    if (!n) return '—'
    return n >= 1_000_000 ? (n / 1_000_000).toFixed(1) + 'M' : (n / 1_000).toFixed(0) + 'K'
  }
  const fmtPrice = (s?: string) => {
    if (!s || s === '0') return '—'
    const v = parseFloat(s)
    if (isNaN(v)) return s
    const perM = v * 1_000_000
    return '$' + (perM < 0.01 ? perM.toExponential(2) : perM.toFixed(perM < 1 ? 3 : 2))
  }
  const modalityIcon = (mod?: string) => {
    if (!mod) return '💬'
    if (mod.includes('image')) return '🖼'
    if (mod.includes('audio') || mod.includes('video')) return '🎤'
    return '💬'
  }

  return (
    <div className="space-y-4">
      {/* Search + refresh */}
      <div className="flex items-center gap-2 flex-wrap">
        <div className="flex items-center gap-1.5 border rounded-md px-2.5 h-8 bg-white text-xs flex-1 min-w-48">
          <Search className="w-3 h-3 text-muted-foreground shrink-0" />
          <input
            className="outline-none bg-transparent w-full"
            placeholder="filter models…"
            value={q}
            onChange={e => setQ(e.target.value)}
          />
        </div>
        <Button size="sm" variant="outline" className="h-8 shrink-0"
          disabled={isFetching} onClick={() => refetch()}>
          <RefreshCw className={`w-3 h-3 mr-1 ${isFetching ? 'animate-spin' : ''}`} />
          {isFetching ? 'Loading…' : 'Refresh'}
        </Button>
        {data && (
          <span className="text-xs text-muted-foreground">
            {models.length} / {allModels.length} models
          </span>
        )}
        {data && (
          <span className="text-xs text-green-700 font-medium flex items-center gap-1">
            <CheckCircle2 className="w-3 h-3" />Live from {providerDisplayName}
          </span>
        )}
      </div>

      {/* Error */}
      {error && (
        <div className="rounded-md bg-red-50 border border-red-200 px-4 py-3 text-xs text-red-800">
          <strong>Error:</strong> {(error as Error).message}
          <p className="mt-1 opacity-70">
            Check that the provider's API key and proxy are configured correctly in the Overview tab.
          </p>
        </div>
      )}

      {/* Loading */}
      {isLoading && (
        <div className="flex items-center gap-2 text-sm text-muted-foreground py-12 justify-center">
          <Loader2 className="w-4 h-4 animate-spin" />Fetching from provider…
        </div>
      )}

      {/* Model table */}
      {models.length > 0 && (
        <div className="rounded-lg border bg-white overflow-hidden">
          <table className="w-full text-xs">
            <thead>
              <tr className="bg-gray-50 border-b text-muted-foreground">
                <th className="text-left px-3 py-2 font-medium">Model ID</th>
                <th className="text-left px-3 py-2 font-medium">Name</th>
                <th className="text-left px-3 py-2 font-medium">Modality</th>
                <th className="text-right px-3 py-2 font-medium">Context</th>
                <th className="text-right px-3 py-2 font-medium">$/1M in</th>
                <th className="text-right px-3 py-2 font-medium">$/1M out</th>
                <th className="text-left px-3 py-2 font-medium">Parameters</th>
                <th className="text-left px-3 py-2 font-medium">Reasoning</th>
              </tr>
            </thead>
            <tbody className="divide-y">
              {models.map(m => (
                <tr key={m.id} className="hover:bg-gray-50 transition-colors">
                  <td className="px-3 py-2 font-mono text-[10px] max-w-[200px] truncate" title={m.id}>
                    {m.id}
                  </td>
                  <td className="px-3 py-2 max-w-[160px]">
                    <p className="font-medium truncate" title={m.name}>{m.name || m.id}</p>
                    {m.description && (
                      <p className="text-[10px] text-muted-foreground line-clamp-2 max-w-[150px]" title={m.description}>
                        {m.description}
                      </p>
                    )}
                  </td>
                  <td className="px-3 py-2" title={m.architecture?.modality}>
                    <span className="text-base">{modalityIcon(m.architecture?.modality)}</span>
                    <span className="text-[9px] text-muted-foreground ml-0.5 hidden sm:inline">
                      {m.architecture?.modality}
                    </span>
                  </td>
                  <td className="px-3 py-2 tabular-nums text-right whitespace-nowrap">
                    {fmtCtx(m.context_length)}
                    {m.top_provider?.max_completion_tokens && (
                      <div className="text-[9px] text-muted-foreground">
                        out: {fmtCtx(m.top_provider.max_completion_tokens)}
                      </div>
                    )}
                  </td>
                  <td className="px-3 py-2 tabular-nums font-mono text-right">{fmtPrice(m.pricing?.prompt)}</td>
                  <td className="px-3 py-2 tabular-nums font-mono text-right">{fmtPrice(m.pricing?.completion)}</td>
                  <td className="px-3 py-2 max-w-[180px]">
                    <div className="flex flex-wrap gap-0.5">
                      {(m.supported_parameters ?? []).slice(0, 6).map(p => (
                        <span key={p} className="text-[9px] px-1 py-0.5 rounded bg-gray-100 text-gray-600 whitespace-nowrap">{p}</span>
                      ))}
                      {(m.supported_parameters?.length ?? 0) > 6 && (
                        <span className="text-[9px] text-muted-foreground">
                          +{(m.supported_parameters?.length ?? 0) - 6}
                        </span>
                      )}
                    </div>
                  </td>
                  <td className="px-3 py-2">
                    {m.reasoning ? (
                      <div className="space-y-0.5">
                        <span className={`text-[9px] px-1.5 py-0.5 rounded-full border font-medium ${
                          m.reasoning.mandatory
                            ? 'bg-orange-50 text-orange-700 border-orange-200'
                            : 'bg-indigo-50 text-indigo-700 border-indigo-200'
                        }`}>
                          {m.reasoning.mandatory ? 'required' : 'optional'}
                        </span>
                        {m.reasoning.default_effort && (
                          <div className="text-[9px] text-muted-foreground">default: {m.reasoning.default_effort}</div>
                        )}
                        {(m.reasoning.supported_efforts ?? []).length > 0 && (
                          <div className="flex flex-wrap gap-0.5 mt-0.5">
                            {m.reasoning.supported_efforts!.map(e => (
                              <span key={e} className="text-[8px] px-1 py-0.5 rounded bg-indigo-50 text-indigo-600">{e}</span>
                            ))}
                          </div>
                        )}
                      </div>
                    ) : (
                      <span className="text-[9px] text-muted-foreground">—</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* Empty filter */}
      {data && models.length === 0 && q && (
        <p className="text-sm text-muted-foreground text-center py-6">
          No models match <strong>{q}</strong>
        </p>
      )}

      {/* Empty — no data loaded yet */}
      {!data && !isLoading && !error && (
        <div className="rounded-lg border bg-white p-12 text-center text-muted-foreground">
          <Globe className="w-8 h-8 mx-auto mb-2 opacity-20" />
          <p className="text-sm font-medium">Click Refresh to load live models from the provider</p>
        </div>
      )}
    </div>
  )
}

// ---- Main provider detail page

type Tab = 'overview' | 'catalog' | 'rules' | 'access' | 'credentials' | 'live'

export default function ProviderDetailPage() {
  const params = useParams()
  const router = useRouter()
  const qc = useQueryClient()
  const id = params.id as string
  const [tab, setTab] = useState<Tab>('overview')

  const { data: p } = useQuery({ queryKey: ['provider', id], queryFn: () => api.providers.get(id) })

  const sync = useMutation({
    mutationFn: () => api.providers.sync(id),
    onSuccess: () => { toast({ title: 'Sync triggered' }); qc.invalidateQueries({ queryKey: ['provider', id] }) },
    onError: (e: any) => toast({ title: 'Sync failed', description: e.message, variant: 'destructive' }),
  })

  const isVirtual = p?.exposure_mode === 'catalog' || p?.exposure_mode === 'hybrid'

  const tabs: { key: Tab; label: string; icon: React.ElementType }[] = [
    { key: 'overview', label: 'Overview',                                              icon: Settings },
    { key: 'catalog',  label: `Catalog (${p?.catalog_model_count ?? '…'})`,           icon: Zap },
    { key: 'rules',    label: 'Exposure Rules',                                        icon: Shield },
    { key: 'access',   label: 'Project Access',                                        icon: Users },
    { key: 'credentials', label: 'Credentials',                                       icon: KeyRound },
    { key: 'live',     label: 'Live Models',                                           icon: Globe },
  ]

  return (
    <div className="space-y-5">
      <div className="flex items-center justify-between flex-wrap gap-2">
        <div className="flex items-center gap-3">
          <Button variant="ghost" size="sm" onClick={() => router.push('/providers')} className="h-8">
            <ArrowLeft className="w-4 h-4 mr-1" />Providers
          </Button>
          <div>
            <div className="flex items-center gap-2">
              <h1 className="text-xl font-bold">{p?.display_name ?? id}</h1>
              {p?.exposure_mode && p.exposure_mode !== 'managed' && (
                <span className={`text-[10px] px-2 py-0.5 rounded-full border font-medium ${
                  p.exposure_mode === 'catalog'
                    ? 'bg-violet-50 text-violet-700 border-violet-200'
                    : 'bg-blue-50 text-blue-700 border-blue-200'
                }`}>
                  {p.exposure_mode}
                </span>
              )}
            </div>
            <p className="text-xs text-muted-foreground font-mono">{p?.backend_type} · {p?.base_url}</p>
          </div>
        </div>
        <Button size="sm" variant="outline" disabled={sync.isPending} onClick={() => sync.mutate()}>
          {sync.isPending ? <Loader2 className="w-3.5 h-3.5 mr-1 animate-spin" /> : <RefreshCw className="w-3.5 h-3.5 mr-1" />}
          Sync Catalog
        </Button>
      </div>

      {/* Prompt to switch to catalog/hybrid if still in managed mode */}
      {p && !isVirtual && (
        <div className="rounded-md bg-amber-50 border border-amber-200 px-4 py-2.5 text-xs text-amber-800 flex items-center gap-3">
          <span>
            This provider is in <strong>Managed</strong> mode. Switch to <strong>Catalog</strong> or <strong>Hybrid</strong>
            in the Overview tab to enable project-level access and dynamic virtual models.
          </span>
          <Button size="sm" variant="outline" className="h-7 shrink-0 border-amber-400 text-amber-800 hover:bg-amber-100"
            onClick={() => setTab('overview')}>
            Go to Overview
          </Button>
        </div>
      )}

      <div className="flex gap-0 border-b">
        {tabs.map(t => (
          <button key={t.key} onClick={() => setTab(t.key)}
            className={`flex items-center gap-1.5 px-4 py-2 text-sm font-medium border-b-2 transition-colors ${
              tab === t.key ? 'border-blue-600 text-blue-700' : 'border-transparent text-muted-foreground hover:text-foreground'
            }`}>
            <t.icon className="w-3.5 h-3.5" />{t.label}
            {t.key === 'access' && isVirtual && (
              <span className="ml-1 w-1.5 h-1.5 rounded-full bg-violet-500" title="Catalog/Hybrid mode active" />
            )}
          </button>
        ))}
      </div>

      <div>
        {tab === 'overview' && <OverviewTab    providerId={id} />}
        {tab === 'catalog'  && <CatalogTab     providerId={id} />}
        {tab === 'rules'    && <RulesTab        providerId={id} />}
        {tab === 'access'   && (
          <ProjectAccessTab
            providerId={id}
            providerName={p?.catalog_expose_prefix || p?.name || id}
          />
        )}
        {tab === 'credentials' && (
          <CredentialsTab providerId={id} providerName={p?.display_name ?? p?.name ?? id} />
        )}
        {tab === 'live' && (
          <LiveModelsTab providerId={id} providerDisplayName={p?.display_name ?? p?.name ?? id} />
        )}
      </div>
    </div>
  )
}
