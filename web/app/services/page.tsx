'use client'

import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type ServiceRecord } from '@/lib/api'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'
import { toast } from '@/components/ui/toaster'
import { Plus, Filter } from 'lucide-react'

const SERVICE_TYPES = ['CHAT','EMBEDDING','RERANK','STT','TTS','OCR','AGENT','MCP']
const RUNTIME_TYPES = ['GPU_RUNTIME','CPU_RUNTIME']
const PRIORITIES    = ['critical','high','normal','low','best_effort']

const SERVICE_COLORS: Record<string, string> = {
  CHAT:      'bg-blue-100 text-blue-700',
  EMBEDDING: 'bg-purple-100 text-purple-700',
  RERANK:    'bg-indigo-100 text-indigo-700',
  STT:       'bg-teal-100 text-teal-700',
  TTS:       'bg-cyan-100 text-cyan-700',
  OCR:       'bg-orange-100 text-orange-700',
  AGENT:     'bg-pink-100 text-pink-700',
  MCP:       'bg-gray-100 text-gray-700',
}

const RUNTIME_COLORS: Record<string, string> = {
  GPU_RUNTIME: 'bg-green-100 text-green-700',
  CPU_RUNTIME: 'bg-yellow-100 text-yellow-700',
}

// ── Register service form ─────────────────────────────────────────────────────
function RegisterServiceForm({ onDone }: { onDone: () => void }) {
  const [form, setForm] = useState({
    name: '', display_name: '', service_type: 'EMBEDDING',
    runtime_type: 'CPU_RUNTIME', host: 'localhost', port: '7997',
    cpu_cores: '32', ram_mb: '65536', numa_node: '0', priority: 'normal',
    min_vram_mb: '0', max_vram_mb: '0',
  })
  const set = (k: string) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm(p => ({ ...p, [k]: e.target.value }))

  const isGPU = form.runtime_type === 'GPU_RUNTIME'

  const mut = useMutation({
    mutationFn: () => api.services.register({
      name: form.name, display_name: form.display_name,
      service_type: form.service_type, runtime_type: form.runtime_type,
      host: form.host, port: parseInt(form.port),
      cpu_cores:   parseInt(form.cpu_cores)  || 0,
      ram_mb:      parseInt(form.ram_mb)     || 0,
      numa_node:   parseInt(form.numa_node)  || -1,
      min_vram_mb: parseInt(form.min_vram_mb) || 0,
      max_vram_mb: parseInt(form.max_vram_mb) || 0,
      priority:    form.priority,
    } as any),
    onSuccess: () => { toast({ title: 'Service registered', description: form.name }); onDone() },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  return (
    <form onSubmit={e => { e.preventDefault(); mut.mutate() }} className="space-y-3">
      <div className="p-2 bg-blue-50 border border-blue-100 rounded text-xs text-blue-700">
        Registers an already-running service. NexusLLM routes to it but won't manage the container.
      </div>
      <div className="grid grid-cols-2 gap-3">
        <div><Label>Service name *</Label><Input value={form.name} onChange={set('name')} placeholder="bge-m3" required /></div>
        <div><Label>Display name *</Label><Input value={form.display_name} onChange={set('display_name')} placeholder="BGE-M3 Embeddings" required /></div>
        <div>
          <Label>Service type</Label>
          <select className="w-full border rounded-md h-9 px-3 text-sm mt-1" value={form.service_type} onChange={set('service_type')}>
            {SERVICE_TYPES.map(t => <option key={t} value={t}>{t}</option>)}
          </select>
        </div>
        <div>
          <Label>Runtime type</Label>
          <select className="w-full border rounded-md h-9 px-3 text-sm mt-1" value={form.runtime_type} onChange={set('runtime_type')}>
            {RUNTIME_TYPES.map(t => <option key={t} value={t}>{t}</option>)}
          </select>
        </div>
        <div><Label>Host *</Label><Input value={form.host} onChange={set('host')} required /></div>
        <div><Label>Port *</Label><Input type="number" value={form.port} onChange={set('port')} required /></div>
        <div>
          <Label>Priority</Label>
          <select className="w-full border rounded-md h-9 px-3 text-sm mt-1" value={form.priority} onChange={set('priority')}>
            {PRIORITIES.map(p => <option key={p} value={p}>{p}</option>)}
          </select>
        </div>
        {isGPU ? <>
          <div><Label>Min VRAM (MB)</Label><Input type="number" value={form.min_vram_mb} onChange={set('min_vram_mb')} /></div>
          <div><Label>Max VRAM (MB)</Label><Input type="number" value={form.max_vram_mb} onChange={set('max_vram_mb')} /></div>
        </> : <>
          <div><Label>CPU cores</Label><Input type="number" value={form.cpu_cores} onChange={set('cpu_cores')} /></div>
          <div><Label>RAM (MB)</Label><Input type="number" value={form.ram_mb} onChange={set('ram_mb')} /></div>
          <div><Label>NUMA node (-1 = any)</Label><Input type="number" value={form.numa_node} onChange={set('numa_node')} /></div>
        </>}
      </div>
      <Button type="submit" className="w-full" disabled={mut.isPending}>
        {mut.isPending ? 'Registering…' : '📦 Register Service'}
      </Button>
    </form>
  )
}

// ── Main page ─────────────────────────────────────────────────────────────────
export default function ServicesPage() {
  const qc = useQueryClient()
  const [filter, setFilter] = useState('')
  const [openRegister, setOpenRegister] = useState(false)

  const { data, isLoading } = useQuery({
    queryKey: ['services', filter],
    queryFn:  () => api.services.list(filter || undefined),
    refetchInterval: 20_000,
  })

  const reload = () => qc.invalidateQueries({ queryKey: ['services'] })

  const grouped = (data?.data ?? []).reduce<Record<string, ServiceRecord[]>>((acc, s) => {
    acc[s.service_type] = acc[s.service_type] ?? []
    acc[s.service_type].push(s)
    return acc
  }, {})

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between flex-wrap gap-2">
        <div>
          <h1 className="text-2xl font-bold">AI Services</h1>
          <p className="text-muted-foreground text-sm mt-0.5">
            All service types: embeddings, rerankers, STT, TTS, OCR, agents, MCP
          </p>
        </div>
        <div className="flex gap-2">
          <div className="flex items-center gap-1.5 border rounded-md px-3 h-9 text-sm">
            <Filter className="w-3.5 h-3.5 text-muted-foreground" />
            <select className="bg-transparent outline-none text-sm" value={filter}
              onChange={e => setFilter(e.target.value)}>
              <option value="">All types</option>
              {SERVICE_TYPES.map(t => <option key={t} value={t}>{t}</option>)}
            </select>
          </div>
          <Dialog open={openRegister} onOpenChange={setOpenRegister}>
            <DialogTrigger asChild>
              <Button><Plus className="w-4 h-4 mr-1" />Register Service</Button>
            </DialogTrigger>
            <DialogContent className="max-w-2xl">
              <DialogHeader><DialogTitle>Register AI Service</DialogTitle></DialogHeader>
              <RegisterServiceForm onDone={() => { setOpenRegister(false); reload() }} />
            </DialogContent>
          </Dialog>
        </div>
      </div>

      {isLoading ? (
        <p className="text-muted-foreground">Loading services…</p>
      ) : Object.keys(grouped).length === 0 ? (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground space-y-2">
            <p className="font-medium">No AI services registered yet.</p>
            <p className="text-sm">
              Register embedding models, rerankers, STT/TTS engines, OCR services, or MCP servers.
            </p>
            <div className="flex flex-wrap justify-center gap-2 pt-2">
              {SERVICE_TYPES.map(t => (
                <span key={t} className={`text-xs px-2 py-1 rounded-full font-medium ${SERVICE_COLORS[t]}`}>{t}</span>
              ))}
            </div>
          </CardContent>
        </Card>
      ) : (
        <div className="space-y-6">
          {SERVICE_TYPES.filter(t => grouped[t]?.length).map(serviceType => (
            <div key={serviceType}>
              <div className="flex items-center gap-2 mb-3">
                <span className={`text-xs px-2 py-1 rounded-full font-semibold ${SERVICE_COLORS[serviceType]}`}>
                  {serviceType}
                </span>
                <span className="text-sm text-muted-foreground">{grouped[serviceType].length} service(s)</span>
              </div>
              <div className="grid gap-3">
                {grouped[serviceType].map(s => (
                  <Card key={s.id}>
                    <CardContent className="pt-4 pb-4">
                      <div className="flex items-center justify-between flex-wrap gap-2">
                        <div>
                          <p className="font-semibold">{s.name}</p>
                          <p className="text-sm text-muted-foreground">{s.display_name} · <span className="font-mono text-xs">{s.backend_type}</span></p>
                        </div>
                        <div className="flex items-center gap-2 flex-wrap">
                          <Badge variant={s.enabled ? 'success' : 'secondary'}>{s.enabled ? 'enabled' : 'disabled'}</Badge>
                          <span className={`text-xs px-2 py-0.5 rounded-full font-medium ${RUNTIME_COLORS[s.runtime_type] ?? ''}`}>
                            {s.runtime_type}
                          </span>
                          <span className={`text-xs font-semibold ${s.healthy_count > 0 ? 'text-green-600' : s.endpoint_count > 0 ? 'text-red-500' : 'text-gray-400'}`}>
                            {s.healthy_count}/{s.endpoint_count} healthy
                          </span>
                        </div>
                      </div>
                    </CardContent>
                  </Card>
                ))}
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
