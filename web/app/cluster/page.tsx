'use client'

// Unified Infrastructure page — Nodes · GPU · Placement Simulator
// Replaces the three separate /nodes, /gpu, /placement pages.

import { useState, useEffect, Suspense } from 'react'
import { useSearchParams, useRouter } from 'next/navigation'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type ClusterNode } from '@/lib/api'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'
import { toast } from '@/components/ui/toaster'
import {
  Network, Server, Cpu, Thermometer, Zap, RefreshCw,
  ChevronDown, ChevronRight, Plus, Trash2, MapPin,
  CheckCircle, XCircle, Play, Clock
} from 'lucide-react'

// ─────────────────────────────────────────────────────────────────────────────
// Shared helpers
// ─────────────────────────────────────────────────────────────────────────────
function ProgressBar({ pct, color }: { pct: number; color: string }) {
  return (
    <div className="h-1.5 w-full rounded-full bg-gray-100 overflow-hidden">
      <div className={`h-full rounded-full transition-all ${color}`} style={{ width: `${Math.min(pct, 100)}%` }} />
    </div>
  )
}

function NodeStatusBadge({ status }: { status: string }) {
  const map: Record<string, string> = {
    online:      'bg-green-100 text-green-700',
    unhealthy:   'bg-yellow-100 text-yellow-800',
    degraded:    'bg-yellow-100 text-yellow-700',
    offline:     'bg-red-100 text-red-700',
    draining:    'bg-blue-100 text-blue-700',
    maintenance: 'bg-purple-100 text-purple-700',
  }
  return (
    <span className={`text-xs px-2 py-0.5 rounded-full font-medium ${map[status] ?? 'bg-gray-100 text-gray-600'}`}>
      {status}
    </span>
  )
}

// ─────────────────────────────────────────────────────────────────────────────
// GPU panel (reused inside both Nodes and GPU tabs)
// ─────────────────────────────────────────────────────────────────────────────
function NodeGPUPanel({ nodeId }: { nodeId: string }) {
  const { data, isLoading } = useQuery({
    queryKey: ['node-gpus', nodeId],
    queryFn:  () => api.nodes.getGPUs(nodeId),
    refetchInterval: 10_000,
  })
  const gpus = data?.data ?? []
  if (isLoading) return <p className="text-xs text-muted-foreground py-2">Loading GPU data…</p>
  if (gpus.length === 0) return (
    <p className="text-xs text-muted-foreground py-2">No GPUs detected — nvidia-smi not available or node agent hasn't reported yet.</p>
  )
  return (
    <div className="mt-3 space-y-2">
      {gpus.map(g => {
        const vramPct = g.vram_mb > 0 ? Math.round((g.mem_used_mb / g.vram_mb) * 100) : 0
        return (
          <div key={g.id} className="border rounded-md p-3 bg-white text-sm">
            <div className="flex items-center justify-between mb-2">
              <span className="font-medium flex items-center gap-1.5">
                <Cpu className="w-3.5 h-3.5 text-muted-foreground" />
                GPU {g.device_index}: {g.name}
                <span className={`text-xs px-1.5 rounded-full ${g.status === 'available' ? 'bg-green-100 text-green-700' : 'bg-blue-100 text-blue-700'}`}>
                  {g.status}
                </span>
              </span>
              <div className="flex items-center gap-3 text-xs text-muted-foreground">
                <span className="flex items-center gap-0.5"><Thermometer className="w-3 h-3" />{g.temperature_c}°C</span>
                <span className="flex items-center gap-0.5"><Zap className="w-3 h-3" />{g.power_draw_w}W</span>
                <span className="font-mono">{Math.round(g.vram_mb / 1024)}GB</span>
              </div>
            </div>
            <div className="space-y-1">
              <div className="flex justify-between text-xs text-muted-foreground">
                <span>GPU util</span><span>{g.utilization_pct}%</span>
              </div>
              <ProgressBar pct={g.utilization_pct} color={g.utilization_pct > 90 ? 'bg-red-500' : g.utilization_pct > 60 ? 'bg-yellow-500' : 'bg-green-500'} />
              <div className="flex justify-between text-xs text-muted-foreground mt-1">
                <span>VRAM</span><span>{Math.round(g.mem_used_mb / 1024)}GB / {Math.round(g.vram_mb / 1024)}GB</span>
              </div>
              <ProgressBar pct={vramPct} color={vramPct > 90 ? 'bg-red-500' : vramPct > 70 ? 'bg-yellow-500' : 'bg-blue-500'} />
            </div>
          </div>
        )
      })}
    </div>
  )
}


// ─────────────────────────────────────────────────────────────────────────────
// NODES TAB
// ─────────────────────────────────────────────────────────────────────────────
// Confirm-action state: 'drain' | 'delete' | 'cordon' | null
type NodeAction = { id: string; action: 'drain' | 'delete' | 'cordon' } | null

function NodesTab() {
  const qc = useQueryClient()
  const [expanded, setExpanded] = useState<string | null>(null)
  const [openRegister, setOpenRegister] = useState(false)
  const [confirm, setConfirm] = useState<NodeAction>(null)

  const { data, isLoading } = useQuery({
    queryKey: ['nodes'],
    queryFn: api.nodes.list,
    refetchInterval: 15_000,
  })
  const nodes: ClusterNode[] = data?.data ?? []

  const drain = useMutation({
    mutationFn: (id: string) => api.nodes.drain(id),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['nodes'] }); setConfirm(null); toast({ title: 'Node draining' }) },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  const del = useMutation({
    mutationFn: (id: string) => api.nodes.delete(id),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['nodes'] }); setConfirm(null); toast({ title: 'Node deleted' }) },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  const cordon = useMutation({
    mutationFn: (id: string) => api.nodes.cordon(id),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['nodes'] }); setConfirm(null); toast({ title: 'Node cordoned — no new workloads' }) },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  const uncordon = useMutation({
    mutationFn: (id: string) => api.nodes.uncordon(id),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['nodes'] }); toast({ title: 'Node uncordoned' }) },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  const onlineCount  = nodes.filter(n => n.status === 'online' && !n.cordoned).length
  const cordonedCount = nodes.filter(n => n.cordoned).length
  const offlineCount = nodes.filter(n => n.status === 'offline').length

  return (
    <div className="space-y-4">
      <div className="flex justify-between items-center">
        <div className="flex items-center gap-3">
          <p className="text-sm text-muted-foreground">{nodes.length} node{nodes.length !== 1 ? 's' : ''}</p>
          {onlineCount > 0   && <span className="text-xs px-2 py-0.5 rounded-full bg-green-100 text-green-700">{onlineCount} online</span>}
          {cordonedCount > 0 && <span className="text-xs px-2 py-0.5 rounded-full bg-yellow-100 text-yellow-800">{cordonedCount} cordoned</span>}
          {offlineCount > 0  && <span className="text-xs px-2 py-0.5 rounded-full bg-red-100 text-red-600">{offlineCount} offline</span>}
        </div>
        <div className="flex gap-2">
          <Button variant="outline" size="sm" onClick={() => qc.invalidateQueries({ queryKey: ['nodes'] })}>
            <RefreshCw className="w-3.5 h-3.5 mr-1" />Refresh
          </Button>
          <Dialog open={openRegister} onOpenChange={setOpenRegister}>
            <DialogTrigger asChild>
              <Button size="sm"><Plus className="w-3.5 h-3.5 mr-1" />Register Node</Button>
            </DialogTrigger>
            <DialogContent>
              <DialogHeader><DialogTitle>Register Cluster Node</DialogTitle></DialogHeader>
              <RegisterNodeForm onDone={() => { setOpenRegister(false); qc.invalidateQueries({ queryKey: ['nodes'] }) }} />
            </DialogContent>
          </Dialog>
        </div>
      </div>

      {isLoading && <p className="text-muted-foreground text-sm">Loading…</p>}
      {!isLoading && nodes.length === 0 && (
        <Card><CardContent className="py-10 text-center text-muted-foreground">
          <p className="font-medium">No nodes registered.</p>
          <p className="text-sm mt-1">Run the node agent on a server — it auto-registers and reports GPU/CPU/RAM.</p>
        </CardContent></Card>
      )}

      {nodes.map(node => {
        const heartbeatSecs = node.last_heartbeat_at
          ? Math.round((Date.now() - new Date(node.last_heartbeat_at).getTime()) / 1000)
          : null
        const isConfirming = confirm?.id === node.id
        const isPending = drain.isPending || del.isPending || cordon.isPending

        return (
          <Card key={node.id} className={node.cordoned ? 'border-yellow-300 bg-yellow-50/30' : ''}>
            <CardContent className="pt-4 pb-4">
              {/* Header row */}
              <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3 min-w-0">
                  <Server className="w-4 h-4 text-muted-foreground shrink-0" />
                  <div className="min-w-0">
                    <div className="flex items-center gap-2 flex-wrap">
                      <span className="font-semibold">
                        {node.hostname && node.hostname !== node.id
                          ? node.hostname
                          : <span className="font-mono text-sm">{node.id.slice(0, 8)}</span>}
                      </span>
                      <NodeStatusBadge status={node.status} />
                      {node.cordoned && (
                        <span className="text-xs px-2 py-0.5 rounded-full bg-yellow-100 text-yellow-800 font-medium">
                          cordoned
                        </span>
                      )}
                      {node.agent_version && (
                        <span className="text-xs text-muted-foreground hidden sm:inline">
                          agent {node.agent_version}
                        </span>
                      )}
                    </div>
                    <p className="text-xs text-muted-foreground mt-0.5 flex flex-wrap gap-x-2">
                      {node.ip_address && <span>{node.ip_address}</span>}
                      {node.total_cpu > 0 && <span>{node.total_cpu} CPUs</span>}
                      {node.total_ram_mb > 0 && <span>{Math.round(node.total_ram_mb / 1024)}GB RAM</span>}
                      {(node.total_vram_mb ?? 0) > 0 && <span>{Math.round((node.total_vram_mb ?? 0) / 1024)}GB VRAM</span>}
                      {node.cordoned && node.cordon_reason && (
                        <span className="text-yellow-700">— {node.cordon_reason}</span>
                      )}
                    </p>
                  </div>
                </div>

                <div className="flex items-center gap-2 shrink-0">
                  {heartbeatSecs !== null && (
                    <span className={`text-xs ${heartbeatSecs > 60 ? 'text-red-500' : 'text-muted-foreground'}`}>
                      {heartbeatSecs}s ago
                    </span>
                  )}

                  {/* Confirm zone */}
                  {isConfirming ? (
                    <>
                      <span className="text-xs text-muted-foreground">
                        {confirm.action === 'drain' ? 'Drain node?' : confirm.action === 'cordon' ? 'Cordon node?' : 'Delete node?'}
                      </span>
                      <Button size="sm" variant="destructive" disabled={isPending}
                        onClick={() => {
                          if (confirm.action === 'drain') drain.mutate(node.id)
                          else if (confirm.action === 'cordon') cordon.mutate(node.id)
                          else del.mutate(node.id)
                        }}>
                        {isPending ? '…' : 'Confirm'}
                      </Button>
                      <Button size="sm" variant="outline" onClick={() => setConfirm(null)}>Cancel</Button>
                    </>
                  ) : (
                    <>
                      {node.cordoned ? (
                        <Button size="sm" variant="outline"
                          className="text-green-700 border-green-200 hover:bg-green-50"
                          disabled={uncordon.isPending}
                          onClick={() => uncordon.mutate(node.id)}>
                          Uncordon
                        </Button>
                      ) : (
                        <Button size="sm" variant="outline"
                          className="text-yellow-700 border-yellow-200 hover:bg-yellow-50"
                          onClick={() => setConfirm({ id: node.id, action: 'cordon' })}>
                          Cordon
                        </Button>
                      )}
                      <Button size="sm" variant="outline"
                        onClick={() => setConfirm({ id: node.id, action: 'drain' })}>
                        Drain
                      </Button>
                      <Button size="sm" variant="outline"
                        className="text-red-600 border-red-200 hover:bg-red-50"
                        onClick={() => setConfirm({ id: node.id, action: 'delete' })}>
                        <Trash2 className="w-3.5 h-3.5" />
                      </Button>
                    </>
                  )}

                  <Button variant="ghost" size="sm"
                    onClick={() => setExpanded(e => e === node.id ? null : node.id)}>
                    {expanded === node.id ? <ChevronDown className="w-4 h-4" /> : <ChevronRight className="w-4 h-4" />}
                  </Button>
                </div>
              </div>

              {/* Expanded: telemetry + GPUs */}
              {expanded === node.id && <NodeDetail nodeId={node.id} />}
            </CardContent>
          </Card>
        )
      })}
    </div>
  )
}

function NodeDetail({ nodeId }: { nodeId: string }) {
  const qc = useQueryClient()
  const [editingLabels, setEditingLabels] = useState(false)
  const [labelPairs, setLabelPairs] = useState<{k: string; v: string}[]>([])

  const { data } = useQuery({
    queryKey: ['node-telemetry', nodeId],
    queryFn: () => api.nodes.getTelemetry(nodeId),
    refetchInterval: 15_000,
  })
  const { data: nodeData } = useQuery({
    queryKey: ['nodes'],
    queryFn: api.nodes.list,
  })
  const node = nodeData?.data?.find(n => n.id === nodeId)
  const existingLabels: Record<string, string> = (() => {
    if (!node?.labels || node.labels === '{}') return {}
    try { return JSON.parse(node.labels) } catch { return {} }
  })()

  const saveLabelsMut = useMutation({
    mutationFn: () => {
      const labels: Record<string, string> = {}
      labelPairs.forEach(p => { if (p.k) labels[p.k] = p.v })
      return api.nodes.setLabels(nodeId, labels)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['nodes'] })
      setEditingLabels(false)
      toast({ title: 'Labels saved' })
    },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  const startEditLabels = () => {
    const pairs = Object.entries(existingLabels).map(([k, v]) => ({ k, v }))
    if (pairs.length === 0) pairs.push({ k: '', v: '' })
    setLabelPairs(pairs)
    setEditingLabels(true)
  }

  const latest = data?.data?.[0]
  return (
    <div className="mt-4 border-t pt-4 space-y-3">
      {latest && (
        <div className="grid grid-cols-3 gap-3 text-sm">
          <div>
            <p className="text-xs text-muted-foreground mb-1">CPU utilization</p>
            <ProgressBar pct={latest.cpu_util_pct} color={latest.cpu_util_pct > 80 ? 'bg-red-500' : 'bg-blue-500'} />
            <p className="text-xs text-muted-foreground mt-0.5">{latest.cpu_util_pct.toFixed(1)}%</p>
          </div>
          <div>
            <p className="text-xs text-muted-foreground mb-1">RAM</p>
            <ProgressBar pct={latest.ram_total_mb > 0 ? (latest.ram_used_mb / latest.ram_total_mb) * 100 : 0} color="bg-purple-500" />
            <p className="text-xs text-muted-foreground mt-0.5">
              {Math.round(latest.ram_used_mb / 1024)}GB / {Math.round(latest.ram_total_mb / 1024)}GB
            </p>
          </div>
          <div>
            <p className="text-xs text-muted-foreground mb-1">Disk</p>
            <ProgressBar pct={latest.disk_total_gb > 0 ? (latest.disk_used_gb / latest.disk_total_gb) * 100 : 0} color="bg-orange-500" />
            <p className="text-xs text-muted-foreground mt-0.5">{latest.disk_used_gb}GB / {latest.disk_total_gb}GB</p>
          </div>
        </div>
      )}

      {/* Labels panel */}
      <div className="border rounded-md p-3 bg-gray-50/50">
        <div className="flex items-center justify-between mb-2">
          <p className="text-xs font-medium text-muted-foreground uppercase tracking-wide">Node Labels</p>
          {!editingLabels && (
            <Button variant="ghost" size="sm" className="h-7 text-xs" onClick={startEditLabels}>
              Edit labels
            </Button>
          )}
        </div>

        {editingLabels ? (
          <div className="space-y-2">
            {labelPairs.map((pair, i) => (
              <div key={i} className="flex gap-2 items-center">
                <input className="flex-1 border rounded px-2 py-1 text-xs"
                  value={pair.k} placeholder="key"
                  onChange={e => setLabelPairs(p => p.map((x, idx) => idx === i ? { ...x, k: e.target.value } : x))} />
                <span className="text-muted-foreground text-xs">=</span>
                <input className="flex-1 border rounded px-2 py-1 text-xs"
                  value={pair.v} placeholder="value"
                  onChange={e => setLabelPairs(p => p.map((x, idx) => idx === i ? { ...x, v: e.target.value } : x))} />
                <button className="text-red-400 text-xs px-1" type="button"
                  onClick={() => setLabelPairs(p => p.filter((_, idx) => idx !== i))}>×</button>
              </div>
            ))}
            <div className="flex gap-2 pt-1">
              <Button type="button" variant="outline" size="sm" className="h-7 text-xs"
                onClick={() => setLabelPairs(p => [...p, { k: '', v: '' }])}>+ Add</Button>
              <Button type="button" size="sm" className="h-7 text-xs"
                disabled={saveLabelsMut.isPending} onClick={() => saveLabelsMut.mutate()}>
                {saveLabelsMut.isPending ? 'Saving…' : 'Save'}
              </Button>
              <Button type="button" variant="ghost" size="sm" className="h-7 text-xs"
                onClick={() => setEditingLabels(false)}>Cancel</Button>
            </div>
            <p className="text-xs text-muted-foreground">
              Common labels: <code className="bg-white border rounded px-1">accelerator=h200</code>{' '}
              <code className="bg-white border rounded px-1">node_group=cluster-1</code>{' '}
              <code className="bg-white border rounded px-1">storage=nvme</code>
            </p>
          </div>
        ) : Object.keys(existingLabels).length === 0 ? (
          <p className="text-xs text-muted-foreground">No labels — click Edit to add placement labels.</p>
        ) : (
          <div className="flex flex-wrap gap-1.5">
            {Object.entries(existingLabels).map(([k, v]) => (
              <span key={k} className="text-xs bg-blue-100 text-blue-800 px-2 py-0.5 rounded-full font-mono">
                {k}={v}
              </span>
            ))}
          </div>
        )}
      </div>

      <NodeGPUPanel nodeId={nodeId} />
    </div>
  )
}

function RegisterNodeForm({ onDone }: { onDone: () => void }) {
  const [hostname, setHostname] = useState('')
  const mut = useMutation({
    mutationFn: () => api.nodes.register({ hostname, display_name: hostname }),
    onSuccess: () => { toast({ title: 'Node registered', description: hostname }); onDone() },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })
  return (
    <form onSubmit={e => { e.preventDefault(); mut.mutate() }} className="space-y-3">
      <div>
        <Label>Hostname *</Label>
        <Input value={hostname} onChange={e => setHostname(e.target.value)} placeholder="gpu-server-01" required className="mt-1" />
      </div>
      <p className="text-xs text-muted-foreground">
        Usually you don't need this — node agents self-register on first heartbeat.
      </p>
      <Button type="submit" disabled={mut.isPending} className="w-full">
        {mut.isPending ? 'Registering…' : 'Register'}
      </Button>
    </form>
  )
}


// ─────────────────────────────────────────────────────────────────────────────
// PLACEMENT SIMULATOR TAB
// ─────────────────────────────────────────────────────────────────────────────
const SERVICE_TYPES = ['CHAT','EMBEDDING','RERANK','STT','TTS','OCR','AGENT','MCP']
const RUNTIME_TYPES = ['GPU_RUNTIME','CPU_RUNTIME']

function PlacementTab() {
  const [form, setForm] = useState({
    model_name: '', service_type: 'CHAT', runtime_type: 'GPU_RUNTIME',
    min_vram_mb: '65536', gpu_count: '1', cpu_cores: '0',
    numa_node: '-1', ram_mb: '0', priority_weight: '500',
  })
  const [result, setResult] = useState<{ feasible: boolean; decision?: Record<string, unknown>; error?: string } | null>(null)
  const set = (k: string) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm(p => ({ ...p, [k]: e.target.value }))
  const isGPU = form.runtime_type === 'GPU_RUNTIME'

  const { data: decisions, isLoading: decisionsLoading } = useQuery({
    queryKey: ['placement-decisions'],
    queryFn: api.placement.listDecisions,
    refetchInterval: 30_000,
  })

  const mut = useMutation({
    mutationFn: () => api.placement.simulate({
      model_name:   form.model_name || 'test-model',
      service_type: form.service_type,
      runtime_type: form.runtime_type,
      min_vram_mb:  isGPU ? parseInt(form.min_vram_mb) || 0 : 0,
      gpu_count:    isGPU ? parseInt(form.gpu_count) || 1 : 0,
      cpu_cores:    parseInt(form.cpu_cores) || 0,
      numa_node:    parseInt(form.numa_node) || -1,
      ram_mb:       parseInt(form.ram_mb) || 0,
      priority:     String(parseInt(form.priority_weight) || 500),
    }),
    onSuccess: r => { setResult(r); if (!r.feasible) toast({ title: 'Infeasible', description: r.error, variant: 'destructive' }) },
    onError: (e: any) => toast({ title: 'Simulation error', description: e.message, variant: 'destructive' }),
  })

  return (
    <div className="space-y-6">
      {/* Simulator form */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base flex items-center gap-2">
            <MapPin className="w-4 h-4" />Placement Simulator
          </CardTitle>
          <p className="text-sm text-muted-foreground">Dry-run: shows what resources would be assigned without deploying.</p>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="grid grid-cols-2 sm:grid-cols-3 gap-3">
            <div><Label>Model name</Label>
              <Input value={form.model_name} onChange={set('model_name')} placeholder="qwen3-32b" className="mt-1" /></div>
            <div><Label>Service type</Label>
              <select className="w-full border rounded-md h-9 px-3 text-sm mt-1" value={form.service_type} onChange={set('service_type')}>
                {SERVICE_TYPES.map(t => <option key={t}>{t}</option>)}
              </select></div>
            <div><Label>Runtime type</Label>
              <select className="w-full border rounded-md h-9 px-3 text-sm mt-1" value={form.runtime_type} onChange={set('runtime_type')}>
                {RUNTIME_TYPES.map(t => <option key={t}>{t}</option>)}
              </select></div>
            <div><Label>Priority weight (0–1000)</Label>
              <Input type="number" min={0} max={1000} value={form.priority_weight} onChange={set('priority_weight')} className="mt-1" /></div>
            {isGPU ? <>
              <div><Label>Min VRAM (MB)</Label>
                <Input type="number" value={form.min_vram_mb} onChange={set('min_vram_mb')} className="mt-1" /></div>
              <div><Label>GPU count</Label>
                <Input type="number" value={form.gpu_count} onChange={set('gpu_count')} min="1" className="mt-1" /></div>
            </> : <>
              <div><Label>CPU cores</Label>
                <Input type="number" value={form.cpu_cores} onChange={set('cpu_cores')} className="mt-1" /></div>
              <div><Label>RAM (MB)</Label>
                <Input type="number" value={form.ram_mb} onChange={set('ram_mb')} className="mt-1" /></div>
            </>}
          </div>
          <Button onClick={() => mut.mutate()} disabled={mut.isPending}>
            <Play className="w-4 h-4 mr-2" />{mut.isPending ? 'Simulating…' : 'Run Simulation'}
          </Button>
          {result && (
            <div className={`rounded-lg border p-4 ${result.feasible ? 'border-green-200 bg-green-50' : 'border-red-200 bg-red-50'}`}>
              <div className="flex items-center gap-2 mb-2">
                {result.feasible ? <CheckCircle className="w-4 h-4 text-green-600" /> : <XCircle className="w-4 h-4 text-red-600" />}
                <span className={`font-medium ${result.feasible ? 'text-green-700' : 'text-red-700'}`}>
                  {result.feasible ? 'Placement feasible' : 'Placement infeasible'}
                </span>
              </div>
              {result.feasible && result.decision && (
                <div className="grid grid-cols-4 gap-3 text-sm">
                  {[
                    { label: 'Node',      value: result.decision.node_host as string },
                    { label: 'GPUs',      value: JSON.stringify(result.decision.gpu_devices ?? []) },
                    { label: 'VRAM',      value: result.decision.vram_mb ? `${Math.round((result.decision.vram_mb as number)/1024)}GB` : '—' },
                    { label: 'Strategy',  value: result.decision.strategy as string },
                    { label: 'Score',     value: typeof result.decision.score === 'number' ? (result.decision.score as number).toFixed(1) : '—' },
                    { label: 'CPU cores', value: String(result.decision.cpu_cores ?? 0) },
                    { label: 'NUMA',      value: String(result.decision.numa_node ?? -1) },
                    { label: 'RAM',       value: result.decision.ram_mb ? `${Math.round((result.decision.ram_mb as number)/1024)}GB` : '—' },
                  ].map(item => (
                    <div key={item.label}>
                      <p className="text-xs text-muted-foreground">{item.label}</p>
                      <p className="font-mono text-xs mt-0.5">{item.value || '—'}</p>
                    </div>
                  ))}
                </div>
              )}
              {!result.feasible && result.error && <p className="text-sm text-red-700 mt-1">{result.error}</p>}
            </div>
          )}
        </CardContent>
      </Card>

      {/* Decision history */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base flex items-center gap-2">
            <Clock className="w-4 h-4" />Placement History
          </CardTitle>
        </CardHeader>
        <CardContent>
          {decisionsLoading ? <p className="text-sm text-muted-foreground">Loading…</p> : (
            (decisions?.data ?? []).length === 0 ? (
              <p className="text-sm text-muted-foreground text-center py-4">No placement decisions yet.</p>
            ) : (
              <table className="w-full text-xs">
                <thead><tr className="border-b text-muted-foreground">
                  <th className="text-left pb-2">When</th>
                  <th className="text-left pb-2">Model</th>
                  <th className="text-left pb-2">GPUs</th>
                  <th className="text-left pb-2">Strategy</th>
                  <th className="text-left pb-2">Score</th>
                  <th className="text-left pb-2">Applied</th>
                </tr></thead>
                <tbody>
                  {(decisions?.data ?? []).map((d: any) => (
                    <tr key={d.id} className="border-b last:border-0">
                      <td className="py-2 text-muted-foreground">{new Date(d.created_at).toLocaleString()}</td>
                      <td className="py-2 font-mono">{d.model_id?.slice(0,8)}…</td>
                      <td className="py-2 font-mono">{d.gpu_devices || '[]'}</td>
                      <td className="py-2">{d.strategy}</td>
                      <td className="py-2">{d.score?.toFixed(1)}</td>
                      <td className="py-2">{d.applied ? <span className="text-green-600">✓</span> : '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )
          )}
        </CardContent>
      </Card>
    </div>
  )
}


// ─────────────────────────────────────────────────────────────────────────────
// MAIN CLUSTER PAGE — tabbed: Nodes | GPU Inventory | Placement
// ─────────────────────────────────────────────────────────────────────────────
type Tab = 'nodes' | 'gpu' | 'placement'

function ClusterPageInner() {
  const searchParams = useSearchParams()
  const router = useRouter()
  const [tab, setTab] = useState<Tab>(() => {
    const t = searchParams.get('tab')
    if (t === 'placement' || t === 'gpu') return t
    return 'nodes'
  })

  const changeTab = (t: Tab) => {
    setTab(t)
    router.replace(`/cluster?tab=${t}`, { scroll: false })
  }

  const tabs: { key: Tab; label: string; icon: React.ReactNode }[] = [
    { key: 'nodes',     label: 'Nodes',              icon: <Network className="w-4 h-4" /> },
    { key: 'gpu',       label: 'GPU Inventory',       icon: <Cpu className="w-4 h-4" /> },
    { key: 'placement', label: 'Placement Simulator', icon: <MapPin className="w-4 h-4" /> },
  ]

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-3">
        <Network className="w-7 h-7 text-blue-600" />
        <div>
          <h1 className="text-2xl font-bold">Cluster</h1>
          <p className="text-sm text-muted-foreground">Nodes · GPU inventory · Resource placement</p>
        </div>
      </div>

      {/* Tab bar */}
      <div className="flex gap-0 border-b">
        {tabs.map(t => (
          <button key={t.key} onClick={() => changeTab(t.key)}
            className={`flex items-center gap-2 px-4 py-2.5 text-sm font-medium border-b-2 transition-colors ${
              tab === t.key
                ? 'border-blue-600 text-blue-600'
                : 'border-transparent text-muted-foreground hover:text-foreground'
            }`}>
            {t.icon}{t.label}
          </button>
        ))}
      </div>

      {/* Tab content */}
      {tab === 'nodes'     && <NodesTab />}
      {tab === 'gpu'       && <GPUInventoryTab />}
      {tab === 'placement' && <PlacementTab />}
    </div>
  )
}

// Suspense wrapper required because useSearchParams() reads from the URL
// during static generation — Next.js requires a boundary here.
export default function ClusterPage() {
  return (
    <Suspense fallback={<div className="p-8 text-muted-foreground text-sm">Loading…</div>}>
      <ClusterPageInner />
    </Suspense>
  )
}

// ─────────────────────────────────────────────────────────────────────────────
// GPU INVENTORY TAB (agent-populated + manual nodes)
// ─────────────────────────────────────────────────────────────────────────────
function GPUInventoryTab() {
  const qc = useQueryClient()
  const [expanded, setExpanded] = useState<string | null>(null)
  const { data, isLoading } = useQuery({
    queryKey: ['nodes'],
    queryFn: api.nodes.list,
    refetchInterval: 15_000,
  })
  const nodes: ClusterNode[] = data?.data ?? []

  return (
    <div className="space-y-4">
      <div className="flex justify-between items-center">
        <p className="text-sm text-muted-foreground">
          GPU devices are auto-detected via <code className="bg-gray-100 px-1 rounded text-xs">nvidia-smi</code> on each node agent.
        </p>
        <Button variant="outline" size="sm" onClick={() => qc.invalidateQueries({ queryKey: ['nodes'] })}>
          <RefreshCw className="w-3.5 h-3.5 mr-1" />Refresh
        </Button>
      </div>

      {isLoading && <p className="text-muted-foreground text-sm">Loading…</p>}
      {!isLoading && nodes.length === 0 && (
        <Card><CardContent className="py-10 text-center text-muted-foreground">
          No nodes with GPU data. Run the node agent on a GPU server.
        </CardContent></Card>
      )}

      {nodes.map(node => (
        <Card key={node.id}>
          <CardContent className="pt-4 pb-4">
            <div className="flex items-center justify-between">
              <div className="flex items-center gap-3">
                <Server className="w-4 h-4 text-muted-foreground" />
                <div>
                  <span className="font-semibold text-sm">{node.hostname}</span>
                  <p className="text-xs text-muted-foreground">
                    {(node.total_vram_mb ?? 0) > 0
                      ? `${Math.round((node.total_vram_mb ?? 0) / 1024)}GB total VRAM`
                      : 'No GPU reported'}
                  </p>
                </div>
                <NodeStatusBadge status={node.status} />
              </div>
              <Button variant="ghost" size="sm"
                onClick={() => setExpanded(e => e === node.id ? null : node.id)}>
                {expanded === node.id ? <ChevronDown className="w-4 h-4" /> : <ChevronRight className="w-4 h-4" />}
              </Button>
            </div>
            {expanded === node.id && (
              <div className="mt-3 border-t pt-3">
                <NodeGPUPanel nodeId={node.id} />
              </div>
            )}
          </CardContent>
        </Card>
      ))}
    </div>
  )
}
