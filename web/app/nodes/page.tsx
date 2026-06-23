'use client'

import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type ClusterNode, type NodeGPUDevice } from '@/lib/api'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'
import { toast } from '@/components/ui/toaster'
import { Plus, Activity, RefreshCw, Trash2, Cpu, Thermometer, Zap } from 'lucide-react'

// ── Status badge ──────────────────────────────────────────────────────────────
function StatusBadge({ status }: { status: string }) {
  const cls =
    status === 'online'      ? 'bg-green-100 text-green-700' :
    status === 'unhealthy'   ? 'bg-yellow-100 text-yellow-800' :
    status === 'degraded'    ? 'bg-yellow-100 text-yellow-700' :
    status === 'offline'     ? 'bg-red-100 text-red-700' :
    status === 'draining'    ? 'bg-blue-100 text-blue-700' :
    status === 'maintenance' ? 'bg-purple-100 text-purple-700' :
                               'bg-gray-100 text-gray-600'
  return <span className={`text-xs px-2 py-0.5 rounded-full font-medium ${cls}`}>{status}</span>
}

// ── GPU panel (live telemetry from node agent) ────────────────────────────────
function GPUPanel({ nodeId }: { nodeId: string }) {
  const { data, isLoading } = useQuery({
    queryKey: ['node-gpus', nodeId],
    queryFn:  () => api.nodes.getGPUs(nodeId),
    refetchInterval: 10_000,
  })

  const gpus = data?.data ?? []

  if (isLoading) return <p className="text-sm text-muted-foreground py-2">Loading GPU data…</p>
  if (gpus.length === 0) return (
    <p className="text-sm text-muted-foreground py-2">
      No GPUs detected — nvidia-smi not available or node agent not reporting GPU data yet.
    </p>
  )

  return (
    <div className="space-y-3">
      {gpus.map(gpu => {
        const vramUsedPct = gpu.vram_mb > 0 ? Math.round((gpu.mem_used_mb / gpu.vram_mb) * 100) : 0
        const utilColor =
          gpu.utilization_pct > 90 ? 'bg-red-500' :
          gpu.utilization_pct > 60 ? 'bg-yellow-500' : 'bg-green-500'
        const vramColor =
          vramUsedPct > 90 ? 'bg-red-500' :
          vramUsedPct > 70 ? 'bg-yellow-500' : 'bg-blue-500'

        return (
          <div key={gpu.id} className="border rounded-lg p-3 space-y-2">
            <div className="flex items-center justify-between">
              <div className="flex items-center gap-2">
                <Cpu className="w-4 h-4 text-muted-foreground" />
                <span className="font-medium text-sm">GPU {gpu.device_index} — {gpu.name}</span>
                <span className={`text-xs px-1.5 py-0.5 rounded-full ${
                  gpu.status === 'available' ? 'bg-green-100 text-green-700' :
                  gpu.status === 'allocated' ? 'bg-blue-100 text-blue-700' :
                  'bg-gray-100 text-gray-600'
                }`}>{gpu.status}</span>
              </div>
              <div className="flex items-center gap-3 text-xs text-muted-foreground">
                <span className="flex items-center gap-1">
                  <Thermometer className="w-3 h-3" />{gpu.temperature_c}°C
                </span>
                <span className="flex items-center gap-1">
                  <Zap className="w-3 h-3" />{gpu.power_draw_w}W
                  {gpu.power_limit_w > 0 && <span className="opacity-60">/{gpu.power_limit_w}W</span>}
                </span>
                <span className="font-mono">{Math.round(gpu.vram_mb / 1024)}GB VRAM</span>
              </div>
            </div>

            {/* GPU utilisation bar */}
            <div>
              <div className="flex justify-between text-xs text-muted-foreground mb-1">
                <span>GPU utilisation</span><span>{gpu.utilization_pct}%</span>
              </div>
              <div className="h-2 bg-gray-100 rounded-full overflow-hidden">
                <div className={`h-full rounded-full transition-all ${utilColor}`}
                  style={{ width: `${Math.min(gpu.utilization_pct, 100)}%` }} />
              </div>
            </div>

            {/* VRAM bar */}
            <div>
              <div className="flex justify-between text-xs text-muted-foreground mb-1">
                <span>VRAM used</span>
                <span>{Math.round(gpu.mem_used_mb / 1024)}GB / {Math.round(gpu.vram_mb / 1024)}GB ({vramUsedPct}%)</span>
              </div>
              <div className="h-2 bg-gray-100 rounded-full overflow-hidden">
                <div className={`h-full rounded-full transition-all ${vramColor}`}
                  style={{ width: `${vramUsedPct}%` }} />
              </div>
            </div>

            {gpu.pcie_bus_id && (
              <p className="text-xs text-muted-foreground">PCIe: {gpu.pcie_bus_id} · NUMA node {gpu.numa_node}</p>
            )}
          </div>
        )
      })}
    </div>
  )
}

// ── Telemetry panel ───────────────────────────────────────────────────────────
function TelemetryPanel({ nodeId }: { nodeId: string }) {
  const { data, isLoading } = useQuery({
    queryKey: ['node-telemetry', nodeId],
    queryFn:  () => api.nodes.getTelemetry(nodeId),
    refetchInterval: 15_000,
  })
  const latest = data?.data?.[0]
  if (isLoading) return <p className="text-sm text-muted-foreground py-2">Loading…</p>
  if (!latest)   return <p className="text-sm text-muted-foreground py-2">No telemetry yet — node agent not reporting.</p>

  const ramUsedPct = latest.ram_total_mb > 0
    ? Math.round((latest.ram_used_mb / latest.ram_total_mb) * 100) : 0

  return (
    <div className="space-y-3">
      <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
        {[
          { label: 'CPU util',   value: `${latest.cpu_util_pct.toFixed(1)}%` },
          { label: 'RAM used',   value: `${Math.round(latest.ram_used_mb/1024)}GB / ${Math.round(latest.ram_total_mb/1024)}GB (${ramUsedPct}%)` },
          { label: 'Disk used',  value: `${latest.disk_used_gb}GB / ${latest.disk_total_gb}GB` },
          { label: 'NUMA nodes', value: `${latest.numa_nodes}` },
        ].map(s => (
          <div key={s.label} className="rounded-lg border p-3">
            <p className="text-xs text-muted-foreground">{s.label}</p>
            <p className="text-sm font-semibold mt-0.5">{s.value}</p>
          </div>
        ))}
      </div>
      <div>
        <div className="flex justify-between text-xs text-muted-foreground mb-1">
          <span>CPU utilisation</span><span>{latest.cpu_util_pct.toFixed(1)}%</span>
        </div>
        <div className="h-2 bg-gray-100 rounded-full overflow-hidden">
          <div className={`h-full rounded-full transition-all ${
            latest.cpu_util_pct > 85 ? 'bg-red-500' :
            latest.cpu_util_pct > 60 ? 'bg-yellow-500' : 'bg-green-500'
          }`} style={{ width: `${Math.min(latest.cpu_util_pct, 100)}%` }} />
        </div>
      </div>
      <div>
        <div className="flex justify-between text-xs text-muted-foreground mb-1">
          <span>RAM utilisation</span><span>{ramUsedPct}%</span>
        </div>
        <div className="h-2 bg-gray-100 rounded-full overflow-hidden">
          <div className={`h-full rounded-full transition-all ${
            ramUsedPct > 85 ? 'bg-red-500' :
            ramUsedPct > 60 ? 'bg-yellow-500' : 'bg-blue-500'
          }`} style={{ width: `${ramUsedPct}%` }} />
        </div>
      </div>
      <p className="text-xs text-muted-foreground">
        Last recorded: {new Date(latest.recorded_at).toLocaleString()}
      </p>
    </div>
  )
}

// ── Node detail modal ─────────────────────────────────────────────────────────
function NodeModal({ node, onDeleted }: { node: ClusterNode; onDeleted: () => void }) {
  const qc = useQueryClient()
  const [tab, setTab] = useState<'gpus' | 'telemetry' | 'inventory' | 'events'>('gpus')
  const [confirmDelete, setConfirmDelete] = useState(false)

  const { data: inv } = useQuery({
    queryKey: ['node-inventory', node.id],
    queryFn:  () => api.nodes.getInventory(node.id),
    enabled:  tab === 'inventory',
  })
  const { data: eventsData } = useQuery({
    queryKey: ['node-health-events', node.id],
    queryFn:  () => api.nodes.getHealthEvents(node.id),
    enabled:  tab === 'events',
    refetchInterval: 15_000,
  })

  const drainMut = useMutation({
    mutationFn: () => api.nodes.drain(node.id),
    onSuccess: () => {
      toast({ title: 'Node draining', description: 'No new deployments will be scheduled' })
      qc.invalidateQueries({ queryKey: ['nodes'] })
    },
    onError: (e: any) => toast({ title: 'Drain failed', description: e.message, variant: 'destructive' }),
  })

  const deleteMut = useMutation({
    mutationFn: () => api.nodes.delete(node.id),
    onSuccess: () => {
      toast({ title: 'Node deleted', description: node.hostname })
      qc.invalidateQueries({ queryKey: ['nodes'] })
      onDeleted()
    },
    onError: (e: any) => {
      setConfirmDelete(false)
      toast({ title: 'Delete failed', description: e.message, variant: 'destructive' })
    },
  })

  let labels: Record<string, string> = {}
  try { labels = JSON.parse(node.labels || '{}') } catch {}

  const tabs = ['gpus', 'telemetry', 'inventory', 'events'] as const
  const tabLabels = { gpus: 'GPUs', telemetry: 'Telemetry', inventory: 'Inventory', events: 'Health Events' }

  return (
    <DialogContent className="max-w-3xl max-h-[85vh] overflow-y-auto">
      <DialogHeader>
        <div className="flex items-center justify-between pr-6 flex-wrap gap-2">
          <DialogTitle>{node.hostname} — {node.display_name}</DialogTitle>
          <div className="flex gap-2">
            {node.status === 'online' && (
              <Button size="sm" variant="outline"
                className="text-yellow-600 border-yellow-300 hover:bg-yellow-50"
                onClick={() => drainMut.mutate()} disabled={drainMut.isPending}>
                {drainMut.isPending ? 'Draining…' : '⏸ Drain'}
              </Button>
            )}
            {confirmDelete ? (
              <>
                <Button size="sm" variant="destructive"
                  disabled={deleteMut.isPending}
                  onClick={() => deleteMut.mutate()}>
                  {deleteMut.isPending ? 'Deleting…' : 'Yes, delete'}
                </Button>
                <Button size="sm" variant="outline" onClick={() => setConfirmDelete(false)}>
                  Cancel
                </Button>
              </>
            ) : (
              <Button size="sm" variant="outline"
                className="text-red-500 border-red-300 hover:bg-red-50"
                onClick={() => setConfirmDelete(true)}>
                <Trash2 className="w-3.5 h-3.5 mr-1" />Delete
              </Button>
            )}
          </div>
        </div>
      </DialogHeader>

      {/* Hardware summary */}
      <div className="grid grid-cols-3 gap-3 text-sm">
        <div className="rounded border p-3 text-center">
          <p className="text-xs text-muted-foreground">vCPUs</p>
          <p className="text-2xl font-bold">{node.total_cpu}</p>
        </div>
        <div className="rounded border p-3 text-center">
          <p className="text-xs text-muted-foreground">RAM</p>
          <p className="text-2xl font-bold">{Math.round((node.total_ram_mb || 0) / 1024)}GB</p>
        </div>
        <div className="rounded border p-3 text-center">
          <p className="text-xs text-muted-foreground">VRAM</p>
          <p className="text-2xl font-bold">{Math.round((node.total_vram_mb || 0) / 1024)}GB</p>
        </div>
      </div>

      {node.ip_address && (
        <p className="text-xs text-muted-foreground font-mono">IP: {node.ip_address}</p>
      )}

      {Object.keys(labels).length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {Object.entries(labels).map(([k, v]) => (
            <span key={k} className="text-xs bg-gray-100 rounded px-2 py-0.5">{k}={v}</span>
          ))}
        </div>
      )}

      {/* Tabs */}
      <div className="flex gap-1 border-b">
        {tabs.map(t => (
          <button key={t} onClick={() => setTab(t)}
            className={`px-4 py-2 text-sm font-medium border-b-2 transition-colors ${
              tab === t
                ? 'border-blue-600 text-blue-600'
                : 'border-transparent text-muted-foreground hover:text-foreground'
            }`}>{tabLabels[t]}</button>
        ))}
      </div>

      {tab === 'gpus'      && <GPUPanel nodeId={node.id} />}
      {tab === 'telemetry' && <TelemetryPanel nodeId={node.id} />}
      {tab === 'inventory' && (
        inv ? (
          <div className="space-y-2">
            <p className="text-xs text-muted-foreground">
              Reported: {new Date(inv.reported_at).toLocaleString()} · Agent: {inv.agent_version}
            </p>
            <pre className="text-xs bg-gray-50 rounded p-3 overflow-auto max-h-64">
              {JSON.stringify(JSON.parse(inv.snapshot || '{}'), null, 2)}
            </pre>
          </div>
        ) : <p className="text-sm text-muted-foreground py-4 text-center">No inventory snapshot yet.</p>
      )}
      {tab === 'events' && (
        <div className="space-y-1 max-h-64 overflow-y-auto">
          {(eventsData?.data ?? []).length === 0 ? (
            <p className="text-sm text-muted-foreground py-4 text-center">No health events yet.</p>
          ) : (eventsData?.data ?? []).map(e => (
            <div key={e.id} className="flex items-center gap-3 text-xs py-1.5 border-b last:border-0">
              <span className="text-muted-foreground whitespace-nowrap">
                {new Date(e.created_at).toLocaleString()}
              </span>
              <span className="px-1.5 py-0.5 rounded text-xs font-medium bg-gray-100">
                {e.from_status || '—'}
              </span>
              <span className="text-muted-foreground">→</span>
              <span className={`px-1.5 py-0.5 rounded text-xs font-semibold ${
                e.to_status === 'online'    ? 'bg-green-100 text-green-700' :
                e.to_status === 'offline'   ? 'bg-red-100 text-red-700' :
                e.to_status === 'unhealthy' ? 'bg-yellow-100 text-yellow-700' :
                e.to_status === 'draining'  ? 'bg-blue-100 text-blue-700' :
                'bg-gray-100 text-gray-700'
              }`}>{e.to_status}</span>
              <span className="text-muted-foreground flex-1 truncate">{e.reason}</span>
            </div>
          ))}
        </div>
      )}
    </DialogContent>
  )
}

// ── Register node form ────────────────────────────────────────────────────────
function RegisterNodeForm({ onDone }: { onDone: () => void }) {
  const [form, setForm] = useState({
    hostname: '', display_name: '', total_cpu: '384', total_ram_mb: '1048576',
  })
  const set = (k: string) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm(p => ({ ...p, [k]: e.target.value }))

  const mut = useMutation({
    mutationFn: () => api.nodes.register({
      hostname:     form.hostname,
      display_name: form.display_name || form.hostname,
      total_cpu:    parseInt(form.total_cpu)    || 0,
      total_ram_mb: parseInt(form.total_ram_mb) || 0,
    }),
    onSuccess: () => { toast({ title: 'Node registered', description: form.hostname }); onDone() },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  return (
    <form onSubmit={e => { e.preventDefault(); mut.mutate() }} className="space-y-3">
      <div className="grid grid-cols-2 gap-3">
        <div><Label>Hostname *</Label><Input value={form.hostname} onChange={set('hostname')} placeholder="nexus-gpu-02" required /></div>
        <div><Label>Display name</Label><Input value={form.display_name} onChange={set('display_name')} placeholder="Secondary AI Server" /></div>
        <div><Label>Total vCPUs</Label><Input type="number" value={form.total_cpu} onChange={set('total_cpu')} /></div>
        <div><Label>Total RAM (MB)</Label><Input type="number" value={form.total_ram_mb} onChange={set('total_ram_mb')} /></div>
      </div>
      <p className="text-xs text-muted-foreground">
        CPU/RAM/VRAM totals and GPU devices are updated automatically when the node agent reports in.
      </p>
      <Button type="submit" disabled={mut.isPending} className="w-full">
        {mut.isPending ? 'Registering…' : 'Register Node'}
      </Button>
    </form>
  )
}

// ── Main page ─────────────────────────────────────────────────────────────────
export default function NodesPage() {
  const qc = useQueryClient()
  const [selectedNode, setSelectedNode] = useState<ClusterNode | null>(null)
  const [openRegister, setOpenRegister] = useState(false)

  const { data, isLoading } = useQuery({
    queryKey: ['nodes'],
    queryFn:  api.nodes.list,
    refetchInterval: 15_000,
  })

  const nodes = data?.data ?? []

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold">Cluster Nodes</h1>
          <p className="text-muted-foreground text-sm mt-0.5">
            {nodes.length} node{nodes.length !== 1 ? 's' : ''} — GPU data auto-populated by node agent
          </p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" size="sm"
            onClick={() => qc.invalidateQueries({ queryKey: ['nodes'] })}>
            <RefreshCw className="w-3.5 h-3.5 mr-1" />Refresh
          </Button>
          <Dialog open={openRegister} onOpenChange={setOpenRegister}>
            <DialogTrigger asChild>
              <Button><Plus className="w-4 h-4 mr-1" />Register Node</Button>
            </DialogTrigger>
            <DialogContent className="max-w-lg">
              <DialogHeader><DialogTitle>Register Cluster Node</DialogTitle></DialogHeader>
              <RegisterNodeForm onDone={() => {
                setOpenRegister(false)
                qc.invalidateQueries({ queryKey: ['nodes'] })
              }} />
            </DialogContent>
          </Dialog>
        </div>
      </div>

      {isLoading ? (
        <p className="text-muted-foreground">Loading nodes…</p>
      ) : nodes.length === 0 ? (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground space-y-2">
            <p className="font-medium">No nodes registered.</p>
            <p className="text-sm">Run <code className="bg-gray-100 px-1 rounded">make migrate</code> to seed the default node, or register one manually.</p>
          </CardContent>
        </Card>
      ) : (
        <div className="grid gap-4">
          {nodes.map(node => (
            <Card key={node.id}>
              <CardHeader className="pb-2">
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-3">
                    <div>
                      <CardTitle className="text-base">{node.hostname}</CardTitle>
                      <p className="text-sm text-muted-foreground mt-0.5">{node.display_name}</p>
                    </div>
                    <StatusBadge status={node.status} />
                    {node.agent_version && (
                      <span className="text-xs text-muted-foreground bg-gray-100 rounded px-2 py-0.5">
                        agent {node.agent_version}
                      </span>
                    )}
                  </div>
                  <Dialog
                    open={selectedNode?.id === node.id}
                    onOpenChange={open => setSelectedNode(open ? node : null)}>
                    <DialogTrigger asChild>
                      <Button size="sm" variant="outline">
                        <Activity className="w-3.5 h-3.5 mr-1" />Details
                      </Button>
                    </DialogTrigger>
                    {selectedNode?.id === node.id && (
                      <NodeModal node={node} onDeleted={() => setSelectedNode(null)} />
                    )}
                  </Dialog>
                </div>
              </CardHeader>
              <CardContent>
                <div className="grid grid-cols-2 sm:grid-cols-4 gap-4 text-sm">
                  <div>
                    <p className="text-xs text-muted-foreground">vCPUs</p>
                    <p className="font-bold text-lg">{node.total_cpu}</p>
                  </div>
                  <div>
                    <p className="text-xs text-muted-foreground">RAM</p>
                    <p className="font-bold text-lg">{Math.round((node.total_ram_mb || 0) / 1024)} GB</p>
                  </div>
                  <div>
                    <p className="text-xs text-muted-foreground">VRAM</p>
                    <p className="font-bold text-lg">{Math.round((node.total_vram_mb || 0) / 1024)} GB</p>
                  </div>
                  <div>
                    <p className="text-xs text-muted-foreground">Last heartbeat</p>
                    <p className="text-sm">
                      {node.last_heartbeat_at
                        ? new Date(node.last_heartbeat_at).toLocaleTimeString()
                        : '—'}
                    </p>
                  </div>
                </div>
              </CardContent>
            </Card>
          ))}
        </div>
      )}
    </div>
  )
}
