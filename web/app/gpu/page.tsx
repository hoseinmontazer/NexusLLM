'use client'

import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'
import { toast } from '@/components/ui/toaster'
import { Plus, Server, ChevronDown, ChevronRight } from 'lucide-react'

function VramBar({ used, total }: { used: number; total: number }) {
  const pct = total > 0 ? Math.round((used / total) * 100) : 0
  return (
    <div className="flex items-center gap-2 text-xs">
      <div className="flex-1 bg-gray-200 rounded-full h-2">
        <div
          className={`h-2 rounded-full ${pct > 90 ? 'bg-red-500' : pct > 70 ? 'bg-yellow-500' : 'bg-green-500'}`}
          style={{ width: `${pct}%` }}
        />
      </div>
      <span className="text-muted-foreground w-16 text-right">{pct}% used</span>
    </div>
  )
}

function DeviceList({ nodeId }: { nodeId: string }) {
  const { data } = useQuery({
    queryKey: ['gpu-devices', nodeId],
    queryFn:  () => api.gpu.listDevices(nodeId),
    refetchInterval: 10_000,
  })

  if (!data) return <p className="text-xs text-muted-foreground ml-4">Loading devices…</p>
  if (data.data.length === 0) return <p className="text-xs text-muted-foreground ml-4">No devices registered.</p>

  return (
    <div className="ml-4 mt-2 space-y-2">
      {data.data.map(d => (
        <div key={d.id} className="border rounded-md p-3 text-sm">
          <div className="flex items-center justify-between mb-1">
            <span className="font-medium">GPU {d.device_index}: {d.name}</span>
            <span className={`text-xs px-2 py-0.5 rounded-full ${
              d.status === 'available' ? 'bg-green-100 text-green-700' :
              d.status === 'allocated' ? 'bg-blue-100 text-blue-700' :
              'bg-red-100 text-red-700'
            }`}>{d.status}</span>
          </div>
          <VramBar used={0} total={d.vram_mb} />
          <div className="flex gap-4 mt-1 text-xs text-muted-foreground">
            <span>VRAM: {(d.vram_mb / 1024).toFixed(0)} GB</span>
            <span>Util: {d.utilization_pct}%</span>
            <span>Temp: {d.temperature_c}°C</span>
            <span>Power: {d.power_draw_w}W</span>
          </div>
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
    mutationFn: () => api.gpu.registerDevice(nodeId, {
      device_index: parseInt(idx), name, vram_mb: parseInt(vram),
    }),
    onSuccess: () => { toast({ title: 'Device registered' }); onDone() },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })
  return (
    <form onSubmit={e => { e.preventDefault(); mut.mutate() }} className="space-y-3">
      <div><Label>Device index</Label><Input type="number" value={idx} onChange={e => setIdx(e.target.value)} /></div>
      <div><Label>GPU name *</Label><Input value={name} onChange={e => setName(e.target.value)} placeholder="NVIDIA A100 80GB" required /></div>
      <div><Label>VRAM (MB) *</Label><Input type="number" value={vram} onChange={e => setVram(e.target.value)} required /></div>
      <Button type="submit" disabled={mut.isPending}>{mut.isPending ? 'Adding…' : 'Add Device'}</Button>
    </form>
  )
}

export default function GpuPage() {
  const qc = useQueryClient()
  const [openNode, setOpenNode] = useState(false)
  const [openDevice, setOpenDevice] = useState<string | null>(null)
  const [expanded, setExpanded] = useState<string | null>(null)
  const [nodeName, setNodeName] = useState('')
  const [nodeHost, setNodeHost] = useState('localhost')

  const { data, isLoading } = useQuery({
    queryKey: ['gpu-nodes'],
    queryFn:  api.gpu.listNodes,
    refetchInterval: 15_000,
  })

  const addNode = useMutation({
    mutationFn: () => api.gpu.registerNode({ name: nodeName, host: nodeHost }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['gpu-nodes'] })
      toast({ title: 'GPU node registered' })
      setOpenNode(false); setNodeName(''); setNodeHost('localhost')
    },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold">GPU Inventory</h1>
          <p className="text-sm text-muted-foreground mt-0.5">Physical GPU servers and devices</p>
        </div>
        <Dialog open={openNode} onOpenChange={setOpenNode}>
          <DialogTrigger asChild>
            <Button><Plus className="w-4 h-4 mr-1" />Register Node</Button>
          </DialogTrigger>
          <DialogContent>
            <DialogHeader><DialogTitle>Register GPU Server</DialogTitle></DialogHeader>
            <form onSubmit={e => { e.preventDefault(); addNode.mutate() }} className="space-y-3">
              <div><Label>Node name *</Label><Input value={nodeName} onChange={e => setNodeName(e.target.value)} placeholder="gpu-server-01" required /></div>
              <div><Label>Host *</Label><Input value={nodeHost} onChange={e => setNodeHost(e.target.value)} placeholder="192.168.1.10" required /></div>
              <Button type="submit" disabled={addNode.isPending}>{addNode.isPending ? 'Registering…' : 'Register'}</Button>
            </form>
          </DialogContent>
        </Dialog>
      </div>

      {isLoading ? <p className="text-muted-foreground">Loading…</p> : (
        <div className="space-y-3">
          {(data?.data ?? []).map(node => (
            <Card key={node.id}>
              <CardContent className="pt-4">
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-3">
                    <Server className="w-5 h-5 text-gray-500" />
                    <div>
                      <p className="font-semibold">{node.name}</p>
                      <p className="text-xs text-muted-foreground">{node.host} · {node.driver_type}</p>
                    </div>
                    <span className={`text-xs px-2 py-0.5 rounded-full ${node.is_available ? 'bg-green-100 text-green-700' : 'bg-red-100 text-red-700'}`}>
                      {node.is_available ? 'available' : 'unavailable'}
                    </span>
                  </div>
                  <div className="flex gap-2">
                    <Dialog open={openDevice === node.id} onOpenChange={o => setOpenDevice(o ? node.id : null)}>
                      <DialogTrigger asChild>
                        <Button size="sm" variant="outline"><Plus className="w-3.5 h-3.5 mr-1" />Add GPU</Button>
                      </DialogTrigger>
                      <DialogContent>
                        <DialogHeader><DialogTitle>Add GPU Device to {node.name}</DialogTitle></DialogHeader>
                        <AddDeviceForm nodeId={node.id} onDone={() => {
                          setOpenDevice(null)
                          qc.invalidateQueries({ queryKey: ['gpu-devices', node.id] })
                        }} />
                      </DialogContent>
                    </Dialog>
                    <Button size="sm" variant="ghost" onClick={() => setExpanded(e => e === node.id ? null : node.id)}>
                      {expanded === node.id ? <ChevronDown className="w-4 h-4" /> : <ChevronRight className="w-4 h-4" />}
                    </Button>
                  </div>
                </div>
                {expanded === node.id && <DeviceList nodeId={node.id} />}
              </CardContent>
            </Card>
          ))}
          {(data?.data ?? []).length === 0 && (
            <Card><CardContent className="py-8 text-center text-muted-foreground">
              No GPU nodes registered. Click <strong>Register Node</strong> to add your GPU server.
            </CardContent></Card>
          )}
        </div>
      )}
    </div>
  )
}
