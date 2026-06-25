'use client'

import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type Model } from '@/lib/api'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import {
  Activity, RefreshCw, Filter,
  CheckCircle2, AlertTriangle, XCircle, Loader2, Clock,
} from 'lucide-react'

// ─── types ────────────────────────────────────────────────────────────────────
interface RuntimeRow {
  runtime_id: string
  model_id: string
  model_name: string
  node_id: string
  node_hostname: string
  state: string
  bind_host: string
  bind_port: number
  container_id?: string
  last_used_at?: string
  updated_at: string
  replica_index?: number
  effective_mode?: string
  workload_policy?: string
  error_msg?: string
  recovery_attempt?: number
}

// ─── helpers ──────────────────────────────────────────────────────────────────
function ago(ts?: string) {
  if (!ts) return '—'
  const ms = Date.now() - new Date(ts).getTime()
  const s = Math.round(ms / 1000)
  if (s < 5) return 'just now'
  if (s < 60) return `${s}s`
  const m = Math.round(s / 60)
  if (m < 60) return `${m}m`
  const h = Math.round(m / 60)
  if (h < 24) return `${h}h`
  return `${Math.round(h / 24)}d`
}

// ─── State badge — Kubernetes-style ──────────────────────────────────────────
function StateBadge({ state }: { state: string }) {
  const running = ['active', 'ready', 'warm', 'idle']
  const starting = ['loading_model', 'waiting_ready', 'starting', 'pending', 'downloading', 'validating', 'recovering']
  const failed = ['lost', 'failed', 'unhealthy', 'stopped']

  if (running.includes(state))
    return <span className="inline-flex items-center gap-1 text-xs font-medium text-green-700"><CheckCircle2 className="w-3 h-3" />Running</span>
  if (starting.includes(state))
    return <span className="inline-flex items-center gap-1 text-xs font-medium text-blue-600"><Loader2 className="w-3 h-3 animate-spin" />{state}</span>
  if (failed.includes(state))
    return <span className="inline-flex items-center gap-1 text-xs font-medium text-red-500"><XCircle className="w-3 h-3" />{state}</span>
  return <span className="text-xs text-muted-foreground">{state}</span>
}

const LIVE_STATES = ['active', 'ready', 'warm', 'idle', 'loading_model', 'waiting_ready', 'starting', 'pending', 'downloading', 'validating', 'recovering']
const ACTIVE_STATES = ['active', 'ready', 'warm', 'idle']
const STARTING_STATES = ['loading_model', 'waiting_ready', 'starting', 'pending', 'downloading', 'validating', 'recovering']
const DEAD_STATES = ['failed', 'lost', 'unhealthy', 'stopped']

// ─── Main page ────────────────────────────────────────────────────────────────
export default function RuntimesPage() {
  const qc = useQueryClient()
  const [stateFilter, setStateFilter] = useState<'live' | 'failed' | 'all'>('live')
  const [modelFilter, setModelFilter] = useState('')

  const { data: models, isLoading: modelsLoading } = useQuery({
    queryKey: ['models'],
    queryFn: () => api.models.list(),
    refetchInterval: 30_000,
  })

  const modelList: Model[] = models?.data ?? []
  const { data: allRuntimes = [], isLoading: runtimesLoading } = useQuery({
    queryKey: ['all-runtimes', modelList.map(m => m.id).join(',')],
    queryFn: async () => {
      if (modelList.length === 0) return []
      const results = await Promise.allSettled(
        modelList.map(m => api.models.getRuntimeStatus(m.id))
      )
      const all: RuntimeRow[] = []
      results.forEach((r, i) => {
        if (r.status === 'fulfilled' && r.value?.runtimes) {
          r.value.runtimes.forEach(rt => {
            all.push({ ...rt, model_id: modelList[i].id, model_name: modelList[i].name, node_hostname: (rt as any).hostname ?? rt.node_id } as unknown as RuntimeRow)
          })
        }
      })
      return all
    },
    enabled: modelList.length > 0,
    refetchInterval: 8_000,
  })

  const { data: haData } = useQuery({
    queryKey: ['ha-status'],
    queryFn: api.ha.getClusterStatus,
    refetchInterval: 10_000,
  })
  const haMap: Record<string, any> = {}
  ;(haData?.models ?? []).forEach((m: any) => { haMap[m.model_id] = m })

  // Filter runtimes
  const filtered = allRuntimes.filter(rt => {
    if (stateFilter === 'live'   && !LIVE_STATES.includes(rt.state))  return false
    if (stateFilter === 'failed' && !DEAD_STATES.includes(rt.state))  return false
    if (modelFilter && !rt.model_name.toLowerCase().includes(modelFilter.toLowerCase())) return false
    return true
  })

  // Sort: active first, starting, then dead — then by model name
  const sorted = [...filtered].sort((a, b) => {
    const rank = (s: string) => ACTIVE_STATES.includes(s) ? 0 : STARTING_STATES.includes(s) ? 1 : 2
    const r = rank(a.state) - rank(b.state)
    if (r !== 0) return r
    return a.model_name.localeCompare(b.model_name)
  })

  // Counts
  const active   = allRuntimes.filter(r => ACTIVE_STATES.includes(r.state)).length
  const starting = allRuntimes.filter(r => STARTING_STATES.includes(r.state)).length
  const dead     = allRuntimes.filter(r => DEAD_STATES.includes(r.state)).length

  const isLoading = modelsLoading || runtimesLoading

  return (
    <div className="space-y-5">
      {/* Header */}
      <div className="flex items-center justify-between flex-wrap gap-2">
        <div>
          <h1 className="text-2xl font-bold flex items-center gap-2">
            <Activity className="w-6 h-6 text-green-600" />Runtimes
          </h1>
          <p className="text-sm text-muted-foreground mt-0.5">Live containers across all nodes</p>
        </div>
        <Button variant="outline" size="sm" onClick={() => qc.invalidateQueries({ queryKey: ['all-runtimes'] })}>
          <RefreshCw className="w-3.5 h-3.5 mr-1" />Refresh
        </Button>
      </div>

      {/* Summary + filter bar */}
      <div className="flex items-center gap-3 flex-wrap">
        {/* State filter tabs */}
        <div className="flex gap-0 border rounded-md overflow-hidden text-xs">
          {([
            { key: 'live',   label: `Live (${active + starting})`,    cls: active + starting > 0 ? 'text-green-700' : '' },
            { key: 'failed', label: `Failed (${dead})`,               cls: dead > 0 ? 'text-red-500' : '' },
            { key: 'all',    label: `All (${allRuntimes.length})`,    cls: '' },
          ] as const).map(f => (
            <button key={f.key} onClick={() => setStateFilter(f.key)}
              className={`px-3 py-1.5 font-medium transition-colors ${
                stateFilter === f.key
                  ? 'bg-gray-900 text-white'
                  : 'bg-white text-muted-foreground hover:bg-gray-50'
              } ${f.cls}`}>
              {f.label}
            </button>
          ))}
        </div>

        {/* Model filter */}
        <div className="flex items-center gap-1.5 border rounded-md px-2.5 h-8 bg-white text-xs">
          <Filter className="w-3 h-3 text-muted-foreground" />
          <input
            className="outline-none bg-transparent w-32"
            placeholder="filter model…"
            value={modelFilter}
            onChange={e => setModelFilter(e.target.value)}
          />
        </div>

        <span className="text-xs text-muted-foreground ml-auto">{sorted.length} shown</span>
      </div>

      {/* Kubernetes-style table */}
      {isLoading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : sorted.length === 0 ? (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground">
            <Activity className="w-8 h-8 mx-auto mb-2 opacity-20" />
            <p className="font-medium text-sm">
              {allRuntimes.length === 0 ? 'No runtimes' : 'No runtimes match the filter'}
            </p>
          </CardContent>
        </Card>
      ) : (
        <div className="rounded-lg border bg-white overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b bg-gray-50 text-xs text-muted-foreground">
                <th className="text-left px-4 py-2.5 font-medium">Model</th>
                <th className="text-left px-3 py-2.5 font-medium">Status</th>
                <th className="text-left px-3 py-2.5 font-medium hidden sm:table-cell">Node</th>
                <th className="text-left px-3 py-2.5 font-medium hidden md:table-cell">Replicas</th>
                <th className="text-left px-3 py-2.5 font-medium hidden lg:table-cell">Age</th>
                <th className="text-left px-3 py-2.5 font-medium">Last error</th>
              </tr>
            </thead>
            <tbody className="divide-y">
              {sorted.map(rt => {
                const ha = haMap[rt.model_id]
                const isFailed = DEAD_STATES.includes(rt.state)
                return (
                  <tr key={rt.runtime_id}
                    className={`hover:bg-gray-50 transition-colors ${isFailed ? 'bg-red-50/30' : ''}`}>

                    {/* Model */}
                    <td className="px-4 py-3">
                      <div className="font-medium">{rt.model_name}</div>
                      <div className="text-xs text-muted-foreground font-mono">
                        {rt.runtime_id.slice(0, 8)}…
                      </div>
                    </td>

                    {/* Status */}
                    <td className="px-3 py-3">
                      <StateBadge state={rt.state} />
                      {(rt.recovery_attempt ?? 0) > 0 && (
                        <div className="text-xs text-orange-600 mt-0.5">
                          ↻ restart {rt.recovery_attempt}
                        </div>
                      )}
                    </td>

                    {/* Node — shows only hostname, never IP */}
                    <td className="px-3 py-3 hidden sm:table-cell">
                      <span className="text-sm font-medium">{rt.node_hostname}</span>
                      {rt.effective_mode && rt.effective_mode !== 'auto' && (
                        <span className="ml-1.5 text-xs text-muted-foreground">{rt.effective_mode}</span>
                      )}
                    </td>

                    {/* Replica info from HA */}
                    <td className="px-3 py-3 hidden md:table-cell">
                      {ha ? (
                        <span className={`text-xs font-medium ${
                          ha.ha_status === 'healthy' ? 'text-green-700' :
                          ha.ha_status === 'degraded' ? 'text-yellow-700' : 'text-red-500'
                        }`}>
                          {ha.active_replicas}/{ha.desired_replicas}
                          {ha.starting_replicas > 0 && <span className="text-blue-500 ml-1">(+{ha.starting_replicas})</span>}
                        </span>
                      ) : (
                        <span className="text-xs text-muted-foreground">—</span>
                      )}
                    </td>

                    {/* Age */}
                    <td className="px-3 py-3 hidden lg:table-cell">
                      <div className="text-xs text-muted-foreground flex items-center gap-1">
                        <Clock className="w-3 h-3" />{ago(rt.updated_at)}
                      </div>
                      {rt.last_used_at && (
                        <div className="text-xs text-muted-foreground">used {ago(rt.last_used_at)}</div>
                      )}
                    </td>

                    {/* Last error — only show when failed, truncated */}
                    <td className="px-3 py-3">
                      {isFailed && rt.error_msg ? (
                        <span className="text-xs text-red-600 max-w-[200px] truncate block" title={rt.error_msg}>
                          {rt.error_msg.slice(0, 60)}{rt.error_msg.length > 60 ? '…' : ''}
                        </span>
                      ) : isFailed ? (
                        <span className="text-xs text-red-400">—</span>
                      ) : (
                        <span className="text-xs text-muted-foreground">—</span>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}

      {/* Subtle info note */}
      <p className="text-xs text-muted-foreground px-1">
        Node IPs and ports are not shown — clients route by model name only. Auto-refreshes every 8s.
      </p>
    </div>
  )
}
