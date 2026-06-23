'use client'

import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type NodeGPUDevice } from '@/lib/api'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'
import { toast } from '@/components/ui/toaster'
import { Server, Cpu, Thermometer, Zap, Trash2, Plus, ChevronDown, ChevronRight, RefreshCw } from 'lucide-react'

// ── VRAM bar ──────────────────────────────────────────────────────────────────
function VramBar({ used, total }: { used: number; total: number }) {
  const pct = total > 0 ? Math.round((used / total) * 100) : 0
  return (
    <div className="flex items-center gap-2 text-xs">
      <div className="flex-1 bg-gray-100 rounded-full h-1.5">
        <div
          className={`h-1.5 rounded-full transition-all ${pct > 90 ? 'bg-red-500' : pct > 70 ? 'bg-yellow-500' : 'bg-green-500'}`}
          style={{ width: `${pct}%` }}
        />
      </div>
      <span className="text-muted-foreground w-10 text-right shrink-0">{pct}%</span>
    </div>
  )
}

// ── Cluster node GPU panel — populated by node agent ─────────────────────────
function ClusterNodeGPUs({ nodeId, hostname }: { nodeId: string; hostname: string }) {
  const { data, isLoading } = useQuery({
    queryKey: ['node-gpus', nodeId],
    queryFn:  () => api.nodes.getGPUs(nodeId),
    refetchInterval: 15_000,
  })
  const gpus = data?.data ?? []
  if (isLoading) return <p className="text-xs text-muted-foreground py-2 ml-4">Loading GPUs…</p>
  if (gpus.length === 0) return (
    <p className="text-xs text-muted-foreground py-2 ml-4">
      No GPUs detected — node agent will populate this when nvidia-smi reports data.
    </p>
  )
  return (
    <div className="ml-4 mt-2 space-y-2">
      {gpus.map((g: NodeGPUDevice) => (
        <div key={g.id} className="border rounded-md p-3 bg-white">
          <div className="flex items-center justify-between mb-2">
            <div className="flex items-center gap-2">
              <Cpu className="w-3.5 h-3.5 text-muted-foreground" />
              <span className="text-sm font-medium">GPU {g.device_index}: {g.name}</span>
              <span className={`text-xs px-1.5 py-0.5 rounded-full ${
                g.status === 'available' ? 'bg-green-100 text-green-700' :
                g.status === 'allocated' ? 'bg-blue-100 text-blue-700' :
                'bg-gray-100 text-gray-600'
              }`}>{g.status}</span>
            </div>
            <div className="flex items-center gap-3 text-xs text-muted-foreground">
              <span className="flex items-center gap-1">
                <Thermometer className="w-3 h-3" />{g.temperature_c}°C
              </span>
              <span className="flex items-center gap-1">
                <Zap className="w-3 h-3" />{g.power_draw_w}W
              </span>
              <span className="font-mono">{Math.round(g.vram_mb / 1024)}GB</span>
            </div>
          </div>
          <div className="space-y-1">
            <div className="flex justify-between text-xs text-muted-foreground mb-0.5">
              <span>GPU util</span><span>{g.utilization_pct}%</span>
            </div>
            <VramBar used={0} total={100} />
            <div className="flex justify-between text-xs text-muted-foreground mb-0.5 mt-1">
              <span>VRAM</span><span>{Math.round(g.mem_used_mb/1024)}GB / {Math.round(g.vram_mb/1024)}GB</span>
            </div>
            <VramBar used={g.mem_used_mb} total={g.vram_mb} />
          </div>
          {g.pcie_bus_id && (
            <p className="text-xs text-muted-foreground mt-1.5">PCIe: {g.pcie_bus_id} · NUMA {g.numa_node}</p>
          )}
        </div>
      ))}
    </div>
  )
}

// ── Legacy GPU node section ────────────────────────────────────────────────────
function LegacyGPUNodes() {
  const qc = useQueryClient()
  const [expanded, setExpanded] = useState<string | null>(null)
  const [openNode, setOpenNode] = useState(false)
  const [openDevice, setOpenDevice] = useState<string | null>(null)
  const [nodeName, setNodeName] = useState('')
  const [nodeHost, setNodeHost] = useState('')
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null)

  const { data, isLoading } = useQuery({
    queryKey: ['gpu-nodes'],
    queryFn:  api.gpu.listNodes,
    refetchInterval: 30_000,
  })
  const nodes = data?.data ?? []

  const addNode = useMutation({
    mutationFn: () => api.gpu.registerNode({ name: nodeName, host: nodeHost }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['gpu-nodes'] })
      toast({ title: 'GPU node registered' })
      setOpenNode(false); setNodeName(''); setNodeHost('')
    },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  const deleteNode = useMutation({
    mutationFn: (id: string) => api.gpu.deleteNode(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['gpu-nodes'] })
      setConfirmDelete(null)
      toast({ title: 'GPU node deleted' })
    },
    onError: (e: any) => {
      setConfirmDelete(null)
      toast({ title: 'Delete failed', description: e.message, variant: 'destructive' })
    },
  })

  if (isLoading) return <p className="text-muted-foreground text-sm">Loading…</p>
  if (nodes.length === 0) return (
    <div className="text-center py-6 text-muted-foreground border rounded-lg">
      <p className="text-sm">No manually registered GPU nodes.</p>
      <p className="text-xs mt-1">GPU nodes are auto-populated via the node agent. Manual registration is optional.</p>
    </div>
  )

  return (
    <div className="space-y-3">
      {nodes.map(node => (
        <Card key={node.id}>
          <CardContent className="pt-4">
            {confirmDelete === node.id ? (
              <div className="flex items-center justify-between gap-4">
                <span className="text-sm text-red-700">Delete <strong>{node.name}</strong>?</span>
                <div className="flex gap-2">
                  <Button size="sm" variant="destructive"
                    disabled={deleteNode.isPending}
                    onClick={() => deleteNode.mutate(node.id)}>
                    {deleteNode.isPending ? 'Deleting…' : 'Yes, delete'}
                  </Button>
                  <Button size="sm" variant="outline" onClick={() => setConfirmDelete(null)}>Cancel</Button>
                </div>
              </div>
            ) : (
              <>
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-3">
                    <Server className="w-4 h-4 text-gray-500" />
                    <div>
                      <p className="font-semibold">{node.name}</p>
                      <p className="text-xs text-muted-foreground">{node.host} · {node.driver_type}</p>
                    </div>
                    <span className={`text-xs px-2 py-0.5 rounded-full ${
                      node.is_available ? 'bg-green-100 text-green-700' : 'bg-red-100 text-red-700'
                    }`}>{node.is_available ? 'available' : 'unavailable'}</span>
                  </div>
                  <div className="flex gap-1">
                    <Dialog open={openDevice === node.id} onOpenChange={o => setOpenDevice(o ? node.id : null)}>
                      <DialogTrigger asChild>
                        <Button size="sm" variant="outline"><Plus className="w-3 h-3 mr-1" />Add GPU</Button>
                      </DialogTrigger>
                      <DialogContent>
                        <DialogHeader><DialogTitle>Add GPU to {node.name}</DialogTitle></DialogHeader>
                        <AddDeviceForm nodeId={node.id} onDone={() => {
                          setOpenDevice(null)
                          qc.invalidateQueries({ queryKey: ['gpu-devices', node.id] })
                        }} />
                      </DialogContent>
                    </Dialog>
                    <Button variant="ghost" size="sm"
                      className="text-red-400 hover:text-red-600"
                      onClick={() => setConfirmDelete(node.id)}>
                      <Trash2 className="w-3.5 h-3.5" />
                    </Button>
                    <Button variant="ghost" size="sm"
                      onClick={() => setExpanded(e => e === node.id ? null : node.id)}>
                      {expanded === node.id ? <ChevronDown className="w-4 h-4" /> : <ChevronRight className="w-4 h-4" />}
                    </Button>
                  </div>
                </div>
                {expanded === node.id && <LegacyDeviceList nodeId={node.id} />}
              </>
            )}
          </CardContent>
        </Card>
      ))}
    </div>
  )
}

function LegacyDeviceList({ nodeId }: { nodeId: string }) {
  const qc = useQueryClient()
  const [confirmDevice, setConfirmDevice] = useState<string | null>(null)
  const { data } = useQuery({
    queryKey: ['gpu-devices', nodeId],
    queryFn:  () => api.gpu.listDevices(nodeId),
    refetchInterval: 15_000,
  })
  const deleteDev = useMutation({
    mutationFn: (deviceId: string) => api.gpu.deleteDevice(nodeId, deviceId),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['gpu-devices', nodeId] }); setConfirmDevice(null); toast({ title: 'Device removed' }) },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })
  const devs = data?.data ?? []
  if (devs.length === 0) return <p className="text-xs text-muted-foreground ml-4 mt-2">No devices registered.</p>
  return (
    <div className="ml-4 mt-2 space-y-2">
      {devs.map(d => (
        <div key={d.id} className="border rounded-md p-3 bg-white">
          {confirmDevice === d.id ? (
            <div className="flex items-center justify-between gap-3">
              <span className="text-xs text-red-700">Remove GPU {d.device_index}?</span>
              <div className="flex gap-1">
                <Button size="sm" variant="destructive" className="h-6 text-xs"
                  disabled={deleteDev.isPending} onClick={() => deleteDev.mutate(d.id)}>
                  {deleteDev.isPending ? '…' : 'Remove'}
                </Button>
                <Button size="sm" variant="outline" className="h-6 text-xs" onClick={() => setConfirmDevice(null)}>Cancel</Button>
              </div>
            </div>
          ) : (
            <div className="flex items-center justify-between">
              <div>
                <span className="text-sm font-medium">GPU {d.device_index}: {d.name}</span>
                <div className="flex gap-4 mt-1 text-xs text-muted-foreground">
                  <span>{Math.round(d.vram_mb / 1024)} GB VRAM</span>
                  <span>Util: {d.utilization_pct}%</span>
                  <span className={`px-1.5 rounded-full ${
                    d.status === 'available' ? 'bg-green-100 text-green-700' :
                    d.status === 'allocated' ? 'bg-blue-100 text-blue-700' : 'bg-gray-100'
                  }`}>{d.status}</span>
                </div>
              </div>
              <Button variant="ghost" size="sm" className="text-red-400 hover:text-red-600"
                onClick={() => setConfirmDevice(d.id)}>
                <Trash2 className="w-3.5 h-3.5" />
              </Button>
            </div>
          )}
        </div>
      ))}
    </div>
  )
}

function AddDeviceForm({ nodeId, onDone }: { nodeId: string; onDone: () => void }) {
  const [idx, setIdx] = useState('0')
  const [name, setName] = useState('')
  const [vram, setVram] = useState('81920')
  const mut = useMutation({
    mutationFn: () => api.gpu.registerDevice(nodeId, { device_index: parseInt(idx), name, vram_mb: parseInt(vram) }),
    onSuccess: () => { toast({ title: 'Device registered' }); onDone() },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })
  return (
    <form onSubmit={e => { e.preventDefault(); mut.mutate() }} className="space-y-3">
      <div><Label>Device index</Label><Input type="number" value={idx} onChange={e => setIdx(e.target.value)} /></div>
      <div><Label>GPU name *</Label><Input value={name} onChange={e => setName(e.target.value)} placeholder="NVIDIA H100 80GB" required /></div>
      <div><Label>VRAM (MB) *</Label><Input type="number" value={vram} onChange={e => setVram(e.target.value)} required /></div>
      <Button type="submit" disabled={mut.isPending} className="w-full">{mut.isPending ? 'Adding…' : 'Add Device'}</Button>
    </form>
  )
}

// ── Main page ─────────────────────────────────────────────────────────────────
export default function GpuPage() {
  const qc = useQueryClient()
  const [expandedNode, setExpandedNode] = useState<string | null>(null)
  const [openManual, setOpenManual] = useState(false)
  const [tab, setTab] = useState<'agent' | 'manual'>('agent')

  const { data: nodesData, isLoading } = useQuery({
    queryKey: ['nodes'],
    queryFn:  api.nodes.list,
    refetchInterval: 15_000,
  })
  const nodes = nodesData?.data ?? []

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold">GPU Inventory</h1>
          <p className="text-sm text-muted-foreground mt-0.5">
            Live GPU data from node agents · auto-updated every 30s
          </p>
        </div>
        <Button variant="outline" size="sm" onClick={() => qc.invalidateQueries({ queryKey: ['nodes'] })}>
          <RefreshCw className="w-3.5 h-3.5 mr-1" />Refresh
        </Button>
      </div>

      {/* Tab selector */}
      <div className="flex gap-1 border-b">
        {([
          { key: 'agent',  label: 'Cluster Nodes (Live)' },
          { key: 'manual', label: 'Manual GPU Nodes' },
        ] as const).map(t => (
          <button key={t.key} onClick={() => setTab(t.key)}
            className={`px-4 py-2 text-sm font-medium border-b-2 transition-colors ${
              tab === t.key
                ? 'border-blue-600 text-blue-600'
                : 'border-transparent text-muted-foreground hover:text-foreground'
            }`}>
            {t.label}
          </button>
        ))}
      </div>

      {/* Agent-populated GPU view */}
      {tab === 'agent' && (
        <div className="space-y-4">
          <p className="text-xs text-muted-foreground">
            GPU devices are auto-detected via <code className="bg-gray-100 px-1 rounded">nvidia-smi</code> on each node agent.
            Go to <a href="/nodes" className="text-blue-600 hover:underline">Cluster Nodes</a> to manage nodes.
          </p>
          {isLoading ? (
            <p className="text-muted-foreground">Loading nodes…</p>
          ) : nodes.length === 0 ? (
            <Card>
              <CardContent className="py-10 text-center text-muted-foreground">
                <p>No cluster nodes registered.</p>
                <p className="text-sm mt-1">Run the node agent on your server — it will auto-register and detect GPUs.</p>
              </CardContent>
            </Card>
          ) : (
            nodes.map(node => (
              <Card key={node.id}>
                <CardHeader className="pb-2">
                  <div className="flex items-center justify-between">
                    <div className="flex items-center gap-3">
                      <Server className="w-4 h-4 text-muted-foreground" />
                      <div>
                        <CardTitle className="text-base">{node.hostname}</CardTitle>
                        <p className="text-xs text-muted-foreground">
                          {Math.round((node.total_vram_mb || 0) / 1024)}GB total VRAM · {node.status}
                        </p>
                      </div>
                      <span className={`text-xs px-2 py-0.5 rounded-full font-medium ${
                        node.status === 'online'  ? 'bg-green-100 text-green-700' :
                        node.status === 'offline' ? 'bg-red-100 text-red-700' :
                        'bg-gray-100 text-gray-600'
                      }`}>{node.status}</span>
                    </div>
                    <Button variant="ghost" size="sm"
                      onClick={() => setExpandedNode(e => e === node.id ? null : node.id)}>
                      {expandedNode === node.id ? <ChevronDown className="w-4 h-4" /> : <ChevronRight className="w-4 h-4" />}
                    </Button>
                  </div>
                </CardHeader>
                {expandedNode === node.id && (
                  <CardContent className="pt-0">
                    <ClusterNodeGPUs nodeId={node.id} hostname={node.hostname} />
                  </CardContent>
                )}
              </Card>
            ))
          )}
        </div>
      )}

      {/* Manual GPU node management */}
      {tab === 'manual' && (
        <div className="space-y-4">
          <div className="flex items-center justify-between">
            <p className="text-xs text-muted-foreground">
              Manually registered GPU nodes — used when the node agent is not available.
            </p>
            <Dialog open={openManual} onOpenChange={setOpenManual}>
              <DialogTrigger asChild>
                <Button size="sm"><Plus className="w-3.5 h-3.5 mr-1" />Register Node</Button>
              </DialogTrigger>
              <DialogContent>
                <DialogHeader><DialogTitle>Register GPU Node Manually</DialogTitle></DialogHeader>
                <form onSubmit={e => {
                  e.preventDefault()
                  const fd = new FormData(e.currentTarget)
                  api.gpu.registerNode({ name: fd.get('name') as string, host: fd.get('host') as string })
                    .then(() => { toast({ title: 'Registered' }); setOpenManual(false); qc.invalidateQueries({ queryKey: ['gpu-nodes'] }) })
                    .catch((err: any) => toast({ title: 'Error', description: err.message, variant: 'destructive' }))
                }} className="space-y-3">
                  <div><Label>Name *</Label><Input name="name" placeholder="gpu-server-01" required /></div>
                  <div><Label>Host *</Label><Input name="host" placeholder="192.168.1.10" required /></div>
                  <Button type="submit" className="w-full">Register</Button>
                </form>
              </DialogContent>
            </Dialog>
          </div>
          <LegacyGPUNodes />
        </div>
      )}
    </div>
  )
}
