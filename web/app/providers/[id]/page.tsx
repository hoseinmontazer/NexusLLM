'use client'

import { useState } from 'react'
import { useParams, useRouter } from 'next/navigation'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type CatalogEntry, type ExposureRule } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { toast } from '@/components/ui/toaster'
import {
  ArrowLeft, RefreshCw, Zap, Search, CheckCircle2,
  XCircle, Loader2, Settings, Shield, Eye,
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

// ── Catalog tab ────────────────────────────────────────────────────────────────

function CatalogTab({ providerId }: { providerId: string }) {
  const qc = useQueryClient()
  const [q, setQ] = useState('')
  const [cap, setCap] = useState('')
  const [tag, setTag] = useState('')
  const [page, setPage] = useState(1)
  const [selected, setSelected] = useState<Set<string>>(new Set())

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
            <Button size="sm" variant="ghost" className="h-7 text-xs"
              onClick={() => setSelected(new Set())}>
              Clear
            </Button>
          </div>
        </div>
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

// ── Overview tab ───────────────────────────────────────────────────────────────

function OverviewTab({ providerId }: { providerId: string }) {
  const qc = useQueryClient()
  const { data: p } = useQuery({ queryKey: ['provider', providerId], queryFn: () => api.providers.get(providerId) })

  const updateMut = useMutation({
    mutationFn: (b: Parameters<typeof api.providers.update>[1]) => api.providers.update(providerId, b),
    onSuccess: () => { toast({ title: 'Updated' }); qc.invalidateQueries({ queryKey: ['provider', providerId] }) },
    onError: (e: any) => toast({ title: 'Failed', description: e.message, variant: 'destructive' }),
  })

  if (!p) return <p className="text-sm text-muted-foreground">Loading…</p>

  return (
    <div className="grid grid-cols-2 gap-6 max-w-2xl">
      <div className="space-y-1">
        <p className="text-xs text-muted-foreground uppercase tracking-wide font-semibold">Provider</p>
        <p className="font-medium">{p.display_name}</p>
        <p className="text-xs text-muted-foreground font-mono">{p.name}</p>
        <p className="text-xs text-muted-foreground">{p.backend_type}</p>
      </div>
      <div className="space-y-1">
        <p className="text-xs text-muted-foreground uppercase tracking-wide font-semibold">Health</p>
        <p className="text-sm">{p.health}</p>
        <p className="text-xs text-muted-foreground">Last checked: {p.last_health_check ? new Date(p.last_health_check).toLocaleString() : '—'}</p>
      </div>
      <div className="space-y-1">
        <p className="text-xs text-muted-foreground uppercase tracking-wide font-semibold">Catalog</p>
        <p className="text-sm tabular-nums">{p.catalog_model_count} models</p>
        <p className="text-xs text-muted-foreground">Last sync: {p.catalog_last_synced_at ? new Date(p.catalog_last_synced_at).toLocaleString() : 'never'}</p>
        <p className="text-xs text-muted-foreground">Status: {p.catalog_sync_status}</p>
        {p.catalog_sync_error && <p className="text-xs text-red-500">{p.catalog_sync_error}</p>}
      </div>
      <div className="space-y-1">
        <p className="text-xs text-muted-foreground uppercase tracking-wide font-semibold">Transport</p>
        <p className="text-xs font-mono">{p.proxy_url || 'direct (no proxy)'}</p>
      </div>

      <div className="col-span-2 border-t pt-4 space-y-3">
        <p className="text-xs font-semibold">Quick Update</p>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <Label className="text-xs">Base URL</Label>
            <Input defaultValue={p.base_url} id="base-url" className="mt-1 font-mono text-xs h-8" />
          </div>
          <div>
            <Label className="text-xs">Proxy URL</Label>
            <Input defaultValue={p.proxy_url ?? ''} id="proxy-url" className="mt-1 font-mono text-xs h-8" placeholder="socks5://…" />
          </div>
        </div>
        <Button size="sm" onClick={() => {
          const base = (document.getElementById('base-url') as HTMLInputElement)?.value
          const proxy = (document.getElementById('proxy-url') as HTMLInputElement)?.value
          updateMut.mutate({ base_url: base || undefined, proxy_url: proxy !== undefined ? proxy : undefined })
        }} disabled={updateMut.isPending}>
          {updateMut.isPending ? 'Saving…' : 'Save Changes'}
        </Button>
      </div>
    </div>
  )
}

// ── Main provider detail page ──────────────────────────────────────────────────

type Tab = 'overview' | 'catalog' | 'rules'

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

  const tabs: { key: Tab; label: string; icon: React.ElementType }[] = [
    { key: 'overview', label: 'Overview', icon: Settings },
    { key: 'catalog',  label: `Catalog (${p?.catalog_model_count ?? '…'})`, icon: Zap },
    { key: 'rules',    label: 'Exposure Rules', icon: Shield },
  ]

  return (
    <div className="space-y-5">
      <div className="flex items-center justify-between flex-wrap gap-2">
        <div className="flex items-center gap-3">
          <Button variant="ghost" size="sm" onClick={() => router.push('/providers')} className="h-8">
            <ArrowLeft className="w-4 h-4 mr-1" />Providers
          </Button>
          <div>
            <h1 className="text-xl font-bold">{p?.display_name ?? id}</h1>
            <p className="text-xs text-muted-foreground font-mono">{p?.backend_type} · {p?.base_url}</p>
          </div>
        </div>
        <Button size="sm" variant="outline" disabled={sync.isPending} onClick={() => sync.mutate()}>
          {sync.isPending ? <Loader2 className="w-3.5 h-3.5 mr-1 animate-spin" /> : <RefreshCw className="w-3.5 h-3.5 mr-1" />}
          Sync Catalog
        </Button>
      </div>

      <div className="flex gap-0 border-b">
        {tabs.map(t => (
          <button key={t.key} onClick={() => setTab(t.key)}
            className={`flex items-center gap-1.5 px-4 py-2 text-sm font-medium border-b-2 transition-colors ${
              tab === t.key ? 'border-blue-600 text-blue-700' : 'border-transparent text-muted-foreground hover:text-foreground'
            }`}>
            <t.icon className="w-3.5 h-3.5" />{t.label}
          </button>
        ))}
      </div>

      <div>
        {tab === 'overview' && <OverviewTab providerId={id} />}
        {tab === 'catalog'  && <CatalogTab  providerId={id} />}
        {tab === 'rules'    && <RulesTab    providerId={id} />}
      </div>
    </div>
  )
}
