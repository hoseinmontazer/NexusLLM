'use client'

import { useState } from 'react'
import { useQuery, useMutation } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { toast } from '@/components/ui/toaster'
import { MapPin, Play, CheckCircle, XCircle, Clock } from 'lucide-react'

const SERVICE_TYPES  = ['CHAT','EMBEDDING','RERANK','STT','TTS','OCR','AGENT','MCP']
const RUNTIME_TYPES  = ['GPU_RUNTIME','CPU_RUNTIME']
const PRIORITIES     = ['critical','high','normal','low','best_effort']

// ─── Simulator ───────────────────────────────────────────────────────────────
function PlacementSimulator() {
  const [form, setForm] = useState({
    model_name:   '',
    service_type: 'CHAT',
    runtime_type: 'GPU_RUNTIME',
    min_vram_mb:  '65536',
    gpu_count:    '1',
    cpu_cores:    '0',
    numa_node:    '-1',
    ram_mb:       '0',
    priority:     'normal',
  })
  const [result, setResult] = useState<{
    feasible: boolean
    decision?: Record<string, unknown>
    error?: string
  } | null>(null)

  const set = (k: string) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm(p => ({ ...p, [k]: e.target.value }))

  const isGPU = form.runtime_type === 'GPU_RUNTIME'

  const mut = useMutation({
    mutationFn: () => api.placement.simulate({
      model_name:   form.model_name || 'test-model',
      service_type: form.service_type,
      runtime_type: form.runtime_type,
      min_vram_mb:  isGPU ? parseInt(form.min_vram_mb) || 0 : 0,
      gpu_count:    isGPU ? parseInt(form.gpu_count)   || 1 : 0,
      cpu_cores:    parseInt(form.cpu_cores) || 0,
      numa_node:    parseInt(form.numa_node) || -1,
      ram_mb:       parseInt(form.ram_mb)    || 0,
      priority:     form.priority,
    }),
    onSuccess: (r) => {
      setResult(r)
      if (!r.feasible) toast({ title: 'Placement infeasible', description: r.error, variant: 'destructive' })
    },
    onError: (e: any) => toast({ title: 'Simulation error', description: e.message, variant: 'destructive' }),
  })

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center gap-2">
          <MapPin className="w-4 h-4 text-muted-foreground" />
          <CardTitle className="text-base">Simulate Placement</CardTitle>
        </div>
        <p className="text-sm text-muted-foreground">
          Dry-run: shows what resources would be assigned without deploying anything.
        </p>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="grid grid-cols-2 sm:grid-cols-3 gap-3">
          <div>
            <Label>Model name</Label>
            <Input value={form.model_name} onChange={set('model_name')} placeholder="qwen3-32b" />
          </div>
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
          <div>
            <Label>Priority</Label>
            <select className="w-full border rounded-md h-9 px-3 text-sm mt-1" value={form.priority} onChange={set('priority')}>
              {PRIORITIES.map(p => <option key={p} value={p}>{p}</option>)}
            </select>
          </div>
          {isGPU ? <>
            <div>
              <Label>Min VRAM (MB)</Label>
              <Input type="number" value={form.min_vram_mb} onChange={set('min_vram_mb')} placeholder="65536 = 64GB" />
            </div>
            <div>
              <Label>GPU count</Label>
              <Input type="number" value={form.gpu_count} onChange={set('gpu_count')} min="1" />
            </div>
          </> : <>
            <div>
              <Label>CPU cores</Label>
              <Input type="number" value={form.cpu_cores} onChange={set('cpu_cores')} placeholder="32" />
            </div>
            <div>
              <Label>RAM (MB)</Label>
              <Input type="number" value={form.ram_mb} onChange={set('ram_mb')} placeholder="65536 = 64GB" />
            </div>
            <div>
              <Label>NUMA node (-1 = any)</Label>
              <Input type="number" value={form.numa_node} onChange={set('numa_node')} />
            </div>
          </>}
        </div>

        <Button onClick={() => mut.mutate()} disabled={mut.isPending} className="w-full sm:w-auto">
          <Play className="w-4 h-4 mr-2" />
          {mut.isPending ? 'Simulating…' : 'Run Placement Simulation'}
        </Button>

        {/* Result */}
        {result && (
          <div className={`rounded-lg border p-4 space-y-3 ${
            result.feasible ? 'border-green-200 bg-green-50' : 'border-red-200 bg-red-50'
          }`}>
            <div className="flex items-center gap-2">
              {result.feasible
                ? <CheckCircle className="w-5 h-5 text-green-600" />
                : <XCircle className="w-5 h-5 text-red-600" />}
              <span className={`font-semibold ${result.feasible ? 'text-green-700' : 'text-red-700'}`}>
                {result.feasible ? 'Placement feasible' : 'Placement infeasible'}
              </span>
            </div>

            {result.feasible && result.decision && (
              <div className="grid grid-cols-2 sm:grid-cols-4 gap-3 text-sm">
                {[
                  { label: 'Node',        value: (result.decision.node_host as string) || '—' },
                  { label: 'GPU devices', value: JSON.stringify(result.decision.gpu_devices ?? []) },
                  { label: 'VRAM',        value: result.decision.vram_mb ? `${Math.round((result.decision.vram_mb as number)/1024)}GB` : '—' },
                  { label: 'CPU cores',   value: String(result.decision.cpu_cores ?? 0) },
                  { label: 'NUMA node',   value: String(result.decision.numa_node ?? -1) },
                  { label: 'RAM',         value: result.decision.ram_mb ? `${Math.round((result.decision.ram_mb as number)/1024)}GB` : '—' },
                  { label: 'Strategy',    value: result.decision.strategy as string || '—' },
                  { label: 'Score',       value: typeof result.decision.score === 'number' ? (result.decision.score as number).toFixed(1) : '—' },
                ].map(item => (
                  <div key={item.label}>
                    <p className="text-xs text-muted-foreground">{item.label}</p>
                    <p className="font-medium font-mono text-xs mt-0.5">{item.value}</p>
                  </div>
                ))}
              </div>
            )}

            {!result.feasible && result.error && (
              <p className="text-sm text-red-700">{result.error}</p>
            )}

            {result.feasible && result.decision?.reason && (
              <p className="text-xs text-muted-foreground">{result.decision.reason as string}</p>
            )}
          </div>
        )}

        {/* H200 quick presets */}
        <div className="border-t pt-3">
          <p className="text-xs text-muted-foreground mb-2">Quick presets (H200 server):</p>
          <div className="flex flex-wrap gap-2">
            {[
              { label: 'Qwen3-32B (GPU 0)',   type: 'CHAT',      rt: 'GPU_RUNTIME', vram: 65536, gpus: 1 },
              { label: 'Llama-70B (GPU 1)',    type: 'CHAT',      rt: 'GPU_RUNTIME', vram: 140000,gpus: 1 },
              { label: 'DeepSeek-V3 (Both)',   type: 'CHAT',      rt: 'GPU_RUNTIME', vram: 144000,gpus: 2 },
              { label: 'Embedding (CPU)',       type: 'EMBEDDING', rt: 'CPU_RUNTIME', cores: 32, ram: 65536 },
              { label: 'Whisper STT (CPU)',     type: 'STT',       rt: 'CPU_RUNTIME', cores: 32, ram: 16384 },
              { label: 'Reranker (CPU)',        type: 'RERANK',    rt: 'CPU_RUNTIME', cores: 16, ram: 32768 },
            ].map(p => (
              <button key={p.label} onClick={() => {
                setResult(null)
                setForm(prev => ({
                  ...prev,
                  model_name:   p.label.toLowerCase().replace(/\s+/g, '-'),
                  service_type: p.type,
                  runtime_type: p.rt,
                  min_vram_mb:  String(p.vram  ?? 0),
                  gpu_count:    String(p.gpus  ?? 1),
                  cpu_cores:    String(p.cores ?? 0),
                  ram_mb:       String(p.ram   ?? 0),
                }))
              }} className="text-xs px-3 py-1.5 rounded border hover:bg-gray-50 transition-colors">
                {p.label}
              </button>
            ))}
          </div>
        </div>
      </CardContent>
    </Card>
  )
}

// ─── Decision history ─────────────────────────────────────────────────────────
function DecisionHistory() {
  const { data, isLoading } = useQuery({
    queryKey: ['placement-decisions'],
    queryFn:  api.placement.listDecisions,
    refetchInterval: 30_000,
  })

  if (isLoading) return <p className="text-muted-foreground text-sm">Loading…</p>

  const decisions = data?.data ?? []

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center gap-2">
          <Clock className="w-4 h-4 text-muted-foreground" />
          <CardTitle className="text-base">Placement History</CardTitle>
        </div>
      </CardHeader>
      <CardContent>
        {decisions.length === 0 ? (
          <p className="text-sm text-muted-foreground text-center py-6">
            No placement decisions yet. Use the simulator above or deploy a service.
          </p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-xs">
              <thead>
                <tr className="border-b text-muted-foreground">
                  <th className="text-left pb-2 pr-4">When</th>
                  <th className="text-left pb-2 pr-4">Model</th>
                  <th className="text-left pb-2 pr-4">GPUs</th>
                  <th className="text-left pb-2 pr-4">CPU</th>
                  <th className="text-left pb-2 pr-4">NUMA</th>
                  <th className="text-left pb-2 pr-4">Strategy</th>
                  <th className="text-left pb-2 pr-4">Score</th>
                  <th className="text-left pb-2">Applied</th>
                </tr>
              </thead>
              <tbody>
                {decisions.map(d => (
                  <tr key={d.id} className="border-b last:border-0">
                    <td className="py-2 pr-4 text-muted-foreground whitespace-nowrap">
                      {new Date(d.created_at).toLocaleString()}
                    </td>
                    <td className="py-2 pr-4 font-mono truncate max-w-[100px]">
                      {d.model_id.slice(0, 8)}…
                    </td>
                    <td className="py-2 pr-4 font-mono">{d.gpu_devices || '[]'}</td>
                    <td className="py-2 pr-4">{d.cpu_cores}</td>
                    <td className="py-2 pr-4">{d.numa_node < 0 ? '—' : d.numa_node}</td>
                    <td className="py-2 pr-4">{d.strategy}</td>
                    <td className="py-2 pr-4">{d.score.toFixed(1)}</td>
                    <td className="py-2">
                      {d.applied
                        ? <span className="text-green-600">✓</span>
                        : <span className="text-muted-foreground">—</span>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </CardContent>
    </Card>
  )
}

// ─── Main page ────────────────────────────────────────────────────────────────
export default function PlacementPage() {
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold">Resource Placement</h1>
        <p className="text-muted-foreground text-sm mt-0.5">
          Simulate where a service would be placed before deploying, and review placement history.
        </p>
      </div>
      <PlacementSimulator />
      <DecisionHistory />
    </div>
  )
}
