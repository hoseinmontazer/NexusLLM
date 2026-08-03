'use client'

import { useState } from 'react'
import Link from 'next/link'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type CatalogProvider, type ExposureMode } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { toast } from '@/components/ui/toaster'
import {
  Globe, RefreshCw, Plus, Activity, CheckCircle2,
  XCircle, AlertTriangle, Loader2, Settings, Zap,
} from 'lucide-react'

const BACKEND_TYPES = [
  { id: 'openrouter_provider', label: 'OpenRouter',    base: 'https://openrouter.ai' },
  { id: 'openai_provider',     label: 'OpenAI',        base: 'https://api.openai.com' },
  { id: 'anthropic_provider',  label: 'Anthropic',     base: 'https://api.anthropic.com' },
  { id: 'google_provider',     label: 'Google Gemini', base: 'https://generativelanguage.googleapis.com' },
  { id: 'groq_provider',       label: 'Groq',          base: 'https://api.groq.com' },
  { id: 'together_provider',   label: 'Together AI',   base: 'https://api.together.xyz' },
  { id: 'mistral_provider',    label: 'Mistral AI',    base: 'https://api.mistral.ai' },
  { id: 'cohere_provider',     label: 'Cohere',        base: 'https://api.cohere.com' },
  { id: 'deepseek_provider',   label: 'DeepSeek',      base: 'https://api.deepseek.com' },
  { id: 'azure_openai_provider', label: 'Azure OpenAI', base: '' },
  { id: 'openai_compat',       label: 'OpenAI-Compatible', base: '' },
]

function HealthBadge({ health }: { health: string }) {
  if (health === 'healthy')
    return <span className="flex items-center gap-1 text-xs text-green-700"><CheckCircle2 className="w-3 h-3" />healthy</span>
  if (health === 'degraded')
    return <span className="flex items-center gap-1 text-xs text-yellow-600"><AlertTriangle className="w-3 h-3" />degraded</span>
  if (health === 'down')
    return <span className="flex items-center gap-1 text-xs text-red-500"><XCircle className="w-3 h-3" />down</span>
  return <span className="flex items-center gap-1 text-xs text-gray-400"><Activity className="w-3 h-3" />unknown</span>
}

function SyncStatusBadge({ status }: { status: string }) {
  if (status === 'ok')     return <span className="text-xs text-green-700">✓ synced</span>
  if (status === 'error')  return <span className="text-xs text-red-500">✗ error</span>
  if (status === 'syncing') return <span className="text-xs text-blue-600 flex items-center gap-1"><Loader2 className="w-3 h-3 animate-spin" />syncing</span>
  return <span className="text-xs text-gray-400">never</span>
}

function ExposureModeBadge({ mode }: { mode: ExposureMode | string }) {
  if (mode === 'catalog')
    return <span className="text-[10px] px-1.5 py-0.5 rounded-full bg-violet-100 text-violet-700 border border-violet-200 font-medium">Catalog</span>
  if (mode === 'hybrid')
    return <span className="text-[10px] px-1.5 py-0.5 rounded-full bg-blue-100 text-blue-700 border border-blue-200 font-medium">Hybrid</span>
  return <span className="text-[10px] px-1.5 py-0.5 rounded-full bg-gray-100 text-gray-500 border border-gray-200 font-medium">Managed</span>
}

function fmtDate(s?: string) {
  if (!s) return '—'
  return new Date(s).toLocaleString()
}

function CreateProviderForm({ onDone }: { onDone: () => void }) {
  const qc = useQueryClient()
  const [backendType, setBackendType] = useState(BACKEND_TYPES[0].id)
  const [name, setName] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [baseUrl, setBaseUrl] = useState(BACKEND_TYPES[0].base)
  const [apiKey, setApiKey] = useState('')
  const [exposureMode, setExposureMode] = useState<ExposureMode>('managed')
  const [syncEnabled, setSyncEnabled] = useState(false)
  const [syncInterval, setSyncInterval] = useState(3600)
  const [exposePrefix, setExposePrefix] = useState('')
  const [proxyUrl, setProxyUrl] = useState('')

  const mut = useMutation({
    mutationFn: () => api.providers.create({
      name, display_name: displayName || name,
      backend_type: backendType, base_url: baseUrl,
      api_key: apiKey || undefined,
      exposure_mode: exposureMode,
      catalog_sync_enabled: syncEnabled,
      catalog_sync_interval: syncInterval,
      catalog_expose_prefix: exposePrefix || undefined,
      proxy_url: proxyUrl || undefined,
    }),
    onSuccess: () => {
      toast({ title: 'Provider created' })
      qc.invalidateQueries({ queryKey: ['providers'] })
      onDone()
    },
    onError: (e: any) => toast({ title: 'Failed', description: e.message, variant: 'destructive' }),
  })

  return (
    <div className="space-y-3">
      <div className="grid grid-cols-2 gap-3">
        <div>
          <Label>Provider type</Label>
          <select className="w-full border rounded-md h-9 px-3 text-sm mt-1" value={backendType}
            onChange={e => {
              setBackendType(e.target.value)
              const found = BACKEND_TYPES.find(b => b.id === e.target.value)
              if (found?.base) setBaseUrl(found.base)
            }}>
            {BACKEND_TYPES.map(b => <option key={b.id} value={b.id}>{b.label}</option>)}
          </select>
        </div>
        <div>
          <Label>Internal name *</Label>
          <Input value={name} onChange={e => setName(e.target.value)} placeholder="openrouter-main" className="mt-1" />
        </div>
      </div>
      <div>
        <Label>Display name</Label>
        <Input value={displayName} onChange={e => setDisplayName(e.target.value)} placeholder="OpenRouter (Main)" className="mt-1" />
      </div>
      <div>
        <Label>Base URL *</Label>
        <Input value={baseUrl} onChange={e => setBaseUrl(e.target.value)} className="mt-1 font-mono text-xs" placeholder="https://api.example.com" />
      </div>
      <div>
        <Label>API Key</Label>
        <Input type="password" value={apiKey} onChange={e => setApiKey(e.target.value)} className="mt-1" placeholder="sk-..." />
      </div>
      <div>
        <Label>Outbound Proxy <span className="text-muted-foreground font-normal text-xs">(optional)</span></Label>
        <Input value={proxyUrl} onChange={e => setProxyUrl(e.target.value)} className="mt-1 font-mono text-xs" placeholder="socks5://192.168.0.207:3315" />
      </div>

      {/* Exposure Mode */}
      <div className="rounded-md border p-3 space-y-2">
        <p className="text-xs font-semibold">Exposure Mode</p>
        <p className="text-xs text-muted-foreground">Controls how this provider's models are surfaced to gateway clients.</p>
        <div className="grid grid-cols-3 gap-2">
          {(['managed', 'catalog', 'hybrid'] as ExposureMode[]).map(m => (
            <button key={m} type="button"
              onClick={() => setExposureMode(m)}
              className={`rounded-md border px-3 py-2 text-left transition-colors text-xs ${
                exposureMode === m
                  ? 'border-blue-500 bg-blue-50 text-blue-800'
                  : 'border-gray-200 hover:bg-gray-50 text-gray-700'
              }`}>
              <div className="font-semibold capitalize">{m}</div>
              <div className="text-[10px] text-muted-foreground mt-0.5">
                {m === 'managed' && 'Register models manually'}
                {m === 'catalog' && 'Dynamic virtual models'}
                {m === 'hybrid'  && 'Both simultaneously'}
              </div>
            </button>
          ))}
        </div>
        {(exposureMode === 'catalog' || exposureMode === 'hybrid') && (
          <div className="rounded-md bg-violet-50 border border-violet-200 px-3 py-2 text-xs text-violet-800 space-y-1">
            <p>In <strong>{exposureMode}</strong> mode, models are exposed as <code className="bg-violet-100 px-0.5 rounded">prefix/provider-model-id</code> virtual names.</p>
            <p>Grant projects access via <strong>Projects → Provider Access</strong> after creating the provider.</p>
          </div>
        )}
      </div>

      <div className="rounded-md border p-3 space-y-2">
        <p className="text-xs font-semibold">Catalog Sync</p>
        <label className="flex items-center gap-2 text-sm cursor-pointer">
          <input type="checkbox" checked={syncEnabled} onChange={e => setSyncEnabled(e.target.checked)} className="h-4 w-4" />
          Enable automatic catalog sync
        </label>
        {syncEnabled && (
          <div className="grid grid-cols-2 gap-3 pl-6">
            <div>
              <Label className="text-xs">Interval (seconds)</Label>
              <Input type="number" min={300} value={syncInterval} onChange={e => setSyncInterval(Number(e.target.value))} className="mt-1" />
            </div>
            <div>
              <Label className="text-xs">Expose prefix</Label>
              <Input value={exposePrefix} onChange={e => setExposePrefix(e.target.value)} placeholder={name || 'openrouter'} className="mt-1 font-mono text-xs" />
            </div>
          </div>
        )}
      </div>
      <div className="flex gap-2 pt-1">
        <Button variant="outline" onClick={onDone} className="flex-shrink-0">Cancel</Button>
        <Button onClick={() => mut.mutate()} disabled={mut.isPending || !name || !baseUrl} className="flex-1">
          {mut.isPending ? 'Creating…' : 'Create Provider'}
        </Button>
      </div>
    </div>
  )
}

function ProviderRow({ p }: { p: CatalogProvider }) {
  const qc = useQueryClient()

  const sync = useMutation({
    mutationFn: () => api.providers.sync(p.id),
    onSuccess: () => { toast({ title: 'Sync triggered', description: p.display_name }); qc.invalidateQueries({ queryKey: ['providers'] }) },
    onError: (e: any) => toast({ title: 'Sync failed', description: e.message, variant: 'destructive' }),
  })

  const health = useMutation({
    mutationFn: () => api.providers.health(p.id),
    onSuccess: (r) => {
      toast({ title: `Health: ${r.health}`, description: `${r.latency_ms}ms` })
      qc.invalidateQueries({ queryKey: ['providers'] })
    },
    onError: (e: any) => toast({ title: 'Health check failed', description: e.message, variant: 'destructive' }),
  })

  const disable = useMutation({
    mutationFn: () => api.providers.delete(p.id),
    onSuccess: () => { toast({ title: 'Provider disabled' }); qc.invalidateQueries({ queryKey: ['providers'] }) },
    onError: (e: any) => toast({ title: 'Failed', description: e.message, variant: 'destructive' }),
  })

  return (
    <tr className="border-b last:border-0 hover:bg-gray-50 transition-colors">
      <td className="px-4 py-3">
        <div className="flex items-center gap-1.5 flex-wrap">
          <span className="font-medium text-sm">{p.display_name}</span>
          <ExposureModeBadge mode={p.exposure_mode ?? 'managed'} />
        </div>
        <div className="text-xs text-muted-foreground font-mono">{p.name}</div>
        <div className="text-xs text-muted-foreground">{p.backend_type}</div>
      </td>
      <td className="px-4 py-3"><HealthBadge health={p.health} /></td>
      <td className="px-4 py-3 text-xs text-muted-foreground">{fmtDate(p.catalog_last_synced_at)}</td>
      <td className="px-4 py-3 text-xs tabular-nums">{p.catalog_model_count}</td>
      <td className="px-4 py-3"><SyncStatusBadge status={p.catalog_sync_status} /></td>
      <td className="px-4 py-3 text-xs">
        {p.proxy_url ? <span className="font-mono text-blue-700">{p.proxy_url}</span> : <span className="text-muted-foreground">direct</span>}
      </td>
      <td className="px-4 py-3">
        <div className="flex items-center justify-end gap-1">
          <Button size="sm" variant="ghost" className="h-7 px-2 text-xs" disabled={sync.isPending} onClick={() => sync.mutate()}>
            <RefreshCw className="w-3 h-3 mr-1" />Sync
          </Button>
          <Button size="sm" variant="ghost" className="h-7 px-2 text-xs" disabled={health.isPending} onClick={() => health.mutate()}>
            <Activity className="w-3 h-3 mr-1" />Test
          </Button>
          <Link href={`/providers/${p.id}`}>
            <Button size="sm" variant="ghost" className="h-7 px-2 text-xs">
              <Settings className="w-3 h-3 mr-1" />Edit
            </Button>
          </Link>
          <Button size="sm" variant="ghost" className="h-7 px-2 text-xs text-red-500 hover:text-red-600" disabled={disable.isPending} onClick={() => disable.mutate()}>
            Disable
          </Button>
        </div>
      </td>
    </tr>
  )
}

export default function ProvidersPage() {
  const qc = useQueryClient()
  const [openCreate, setOpenCreate] = useState(false)

  const { data, isLoading } = useQuery({
    queryKey: ['providers'],
    queryFn: api.providers.list,
    refetchInterval: 30_000,
  })

  const providers = data?.data ?? []
  const healthyCount = providers.filter(p => p.health === 'healthy').length
  const totalModels = providers.reduce((s, p) => s + p.catalog_model_count, 0)
  const catalogCount = providers.filter(p => p.exposure_mode === 'catalog' || p.exposure_mode === 'hybrid').length

  return (
    <div className="space-y-5">
      <div className="flex items-center justify-between flex-wrap gap-2">
        <div>
          <h1 className="text-2xl font-bold flex items-center gap-2">
            <Globe className="w-6 h-6 text-blue-600" />Cloud Providers
          </h1>
          <p className="text-sm text-muted-foreground mt-0.5">
            Manage external AI provider connections, catalog sync, and exposure rules
          </p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" size="sm" onClick={() => qc.invalidateQueries({ queryKey: ['providers'] })}>
            <RefreshCw className="w-3.5 h-3.5 mr-1" />Refresh
          </Button>
          <Button size="sm" onClick={() => setOpenCreate(true)}>
            <Plus className="w-3.5 h-3.5 mr-1" />Add Provider
          </Button>
        </div>
      </div>

      <Dialog open={openCreate} onOpenChange={setOpenCreate}>
        <DialogContent className="max-w-lg">
          <DialogHeader><DialogTitle>Add Cloud Provider</DialogTitle></DialogHeader>
          <CreateProviderForm onDone={() => setOpenCreate(false)} />
        </DialogContent>
      </Dialog>

      <div className="flex gap-4 text-sm text-muted-foreground">
        <span className="flex items-center gap-1"><CheckCircle2 className="w-3.5 h-3.5 text-green-600" />{healthyCount} healthy</span>
        <span className="flex items-center gap-1"><Zap className="w-3.5 h-3.5 text-blue-600" />{totalModels} catalog models</span>
        {catalogCount > 0 && (
          <span className="flex items-center gap-1 text-violet-600">
            <Globe className="w-3.5 h-3.5" />{catalogCount} in catalog/hybrid mode
          </span>
        )}
        <span>{providers.length} providers</span>
      </div>

      {isLoading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : providers.length === 0 ? (
        <div className="rounded-lg border bg-white p-12 text-center text-muted-foreground">
          <Globe className="w-8 h-8 mx-auto mb-2 opacity-20" />
          <p className="font-medium text-sm">No providers yet</p>
          <p className="text-xs mt-1">Add a cloud provider to start syncing models from OpenRouter, OpenAI, Anthropic, and more.</p>
        </div>
      ) : (
        <div className="rounded-lg border bg-white overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b bg-gray-50 text-xs text-muted-foreground">
                <th className="text-left px-4 py-2.5 font-medium">Provider</th>
                <th className="text-left px-4 py-2.5 font-medium">Health</th>
                <th className="text-left px-4 py-2.5 font-medium">Last Sync</th>
                <th className="text-left px-4 py-2.5 font-medium">Models</th>
                <th className="text-left px-4 py-2.5 font-medium">Sync Status</th>
                <th className="text-left px-4 py-2.5 font-medium">Proxy</th>
                <th className="text-right px-4 py-2.5 font-medium">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y">
              {providers.map(p => <ProviderRow key={p.id} p={p} />)}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
