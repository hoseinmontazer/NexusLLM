'use client'

import { useState } from 'react'
import { useParams, useRouter } from 'next/navigation'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type AdmissionPolicy, type PriorityPreset } from '@/lib/api'
import { PriorityBadge, PriorityBar, EffectivePriorityCard, weightLabel } from '@/components/projects/PriorityBadge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { toast } from '@/components/ui/toaster'
import {
  ArrowLeft, Shield, Zap, Activity, AlertTriangle,
  Server, BarChart2, Clock, DollarSign, Layers, Gauge
} from 'lucide-react'

// ── Priority panel ─────────────────────────────────────────────────────────────
function PriorityPanel({ projectId, current, presets }: {
  projectId: string
  current: { priority_weight: number; effective_priority: number; waiting_bonus: number; reservation_bonus: number; resource_penalty: number }
  presets: PriorityPreset[]
}) {
  const qc = useQueryClient()
  const [weight, setWeight] = useState(current.priority_weight)

  const mut = useMutation({
    mutationFn: () => api.projects.setPriority(projectId, weight),
    onSuccess: (r) => {
      if (r.changed) {
        toast({ title: 'Priority updated', description: `${r.old_priority_weight} → ${r.new_priority_weight} (${r.new_priority_label})` })
      } else {
        toast({ title: 'No change' })
      }
      qc.invalidateQueries({ queryKey: ['project', projectId] })
      qc.invalidateQueries({ queryKey: ['projects'] })
    },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  return (
    <Card>
      <CardHeader><CardTitle className="text-base flex items-center gap-2">
        <Gauge className="w-4 h-4" />Priority Management
      </CardTitle></CardHeader>
      <CardContent className="space-y-4">
        {/* Effective priority breakdown */}
        <div className="bg-gray-50 rounded-lg p-3">
          <p className="text-xs font-medium text-muted-foreground mb-2">Effective Priority Breakdown</p>
          <EffectivePriorityCard
            baseWeight={current.priority_weight}
            waitingBonus={current.waiting_bonus}
            reservationBonus={current.reservation_bonus}
            resourcePenalty={current.resource_penalty}
            effective={current.effective_priority}
          />
        </div>

        {/* Change weight */}
        <div className="space-y-2">
          <Label className="text-xs">Change Priority Weight (0–1000)</Label>
          <Input
            type="number" min={0} max={1000} value={weight}
            onChange={e => setWeight(Math.min(1000, Math.max(0, parseInt(e.target.value) || 0)))}
            className="w-32"
          />
          <PriorityBar weight={weight} />
          <div className="text-xs text-muted-foreground">{weightLabel(weight)}</div>
          <div className="flex flex-wrap gap-1.5">
            {presets.map(p => (
              <button key={p.weight} type="button"
                onClick={() => setWeight(p.weight)}
                className={`text-xs px-2 py-0.5 rounded border transition-colors ${
                  weight === p.weight ? 'bg-blue-600 text-white border-blue-600' : 'hover:bg-gray-50 border-gray-200'
                }`}>
                {p.weight} · {p.label}
              </button>
            ))}
          </div>
        </div>
        <Button size="sm" onClick={() => mut.mutate()} disabled={mut.isPending}>
          {mut.isPending ? 'Saving…' : 'Apply Priority'}
        </Button>
        <p className="text-xs text-muted-foreground">
          Change takes effect on the next scheduler cycle (within 60 s). Recorded in the audit log.
        </p>
      </CardContent>
    </Card>
  )
}

// ── Reservation panel ──────────────────────────────────────────────────────────
function ReservationPanel({ projectId, current }: {
  projectId: string
  current: { reserved_vram_mb: number; reserved_cpu_cores: number; reserved_memory_mb: number; max_gpu_vram_mb: number; max_cpu: number; max_memory_mb: number }
}) {
  const qc = useQueryClient()
  const [vram, setVram]   = useState(String(current.reserved_vram_mb))
  const [cpu, setCpu]     = useState(String(current.reserved_cpu_cores))
  const [mem, setMem]     = useState(String(current.reserved_memory_mb))
  const [maxVram, setMaxVram] = useState(String(current.max_gpu_vram_mb))
  const [maxCpu, setMaxCpu]   = useState(String(current.max_cpu))
  const [maxMem, setMaxMem]   = useState(String(current.max_memory_mb))

  const mut = useMutation({
    mutationFn: () => api.projects.reserve(projectId, {
      reserved_vram_mb: parseInt(vram) || 0, reserved_cpu_cores: parseInt(cpu) || 0,
      reserved_memory_mb: parseInt(mem) || 0, max_gpu_vram_mb: parseInt(maxVram) || 0,
      max_cpu: parseInt(maxCpu) || 0, max_memory_mb: parseInt(maxMem) || 0,
    }),
    onSuccess: () => { toast({ title: 'Reservation updated' }); qc.invalidateQueries({ queryKey: ['project', projectId] }) },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  const field = (label: string, val: string, set: (v: string) => void, suffix = '') => (
    <div>
      <Label className="text-xs">{label}</Label>
      <Input type="number" min={0} value={val} onChange={e => set(e.target.value)} className="mt-1" />
      {suffix && <p className="text-xs text-muted-foreground mt-0.5">{suffix}</p>}
    </div>
  )

  return (
    <Card>
      <CardHeader><CardTitle className="text-base">Resource Reservation &amp; Quota</CardTitle></CardHeader>
      <CardContent className="space-y-4">
        <div>
          <p className="text-xs font-medium text-muted-foreground mb-2">Guaranteed minimums (reserved)</p>
          <div className="grid grid-cols-3 gap-3">
            {field('Reserved VRAM (MB)', vram, setVram, `${Math.round(parseInt(vram||'0')/1024)} GB`)}
            {field('Reserved CPU cores', cpu, setCpu)}
            {field('Reserved Memory (MB)', mem, setMem, `${Math.round(parseInt(mem||'0')/1024)} GB`)}
          </div>
        </div>
        <div>
          <p className="text-xs font-medium text-muted-foreground mb-2">Maximum quota (0 = unlimited)</p>
          <div className="grid grid-cols-3 gap-3">
            {field('Max VRAM (MB)', maxVram, setMaxVram, `${Math.round(parseInt(maxVram||'0')/1024)} GB`)}
            {field('Max CPU cores', maxCpu, setMaxCpu)}
            {field('Max Memory (MB)', maxMem, setMaxMem, `${Math.round(parseInt(maxMem||'0')/1024)} GB`)}
          </div>
        </div>
        <Button size="sm" onClick={() => mut.mutate()} disabled={mut.isPending}>
          {mut.isPending ? 'Saving…' : 'Update Reservation'}
        </Button>
        <p className="text-xs text-muted-foreground">
          Reserved resources are guaranteed for this project. Quota limits prevent over-consumption (excess triggers resource penalty on effective priority).
        </p>
      </CardContent>
    </Card>
  )
}

// ── Protection panel ──────────────────────────────────────────────────────────
function ProtectionPanel({ projectId, current }: {
  projectId: string
  current: { always_running: boolean; protected: boolean; minimum_replicas: number; admission_policy: AdmissionPolicy; preemptible: boolean }
}) {
  const qc = useQueryClient()
  const [alwaysRunning, setAlwaysRunning] = useState(current.always_running)
  const [prot, setProt]                   = useState(current.protected)
  const [minReplicas, setMinReplicas]     = useState(String(current.minimum_replicas))
  const [policy, setPolicy]               = useState<AdmissionPolicy>(current.admission_policy)

  const protMut = useMutation({
    mutationFn: () => api.projects.setProtection(projectId, {
      always_running: alwaysRunning, protected: prot,
      minimum_replicas: parseInt(minReplicas) || 0, admission_policy: policy,
    }),
    onSuccess: () => { toast({ title: 'Protection settings saved' }); qc.invalidateQueries({ queryKey: ['project', projectId] }) },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  const preemptMut = useMutation({
    mutationFn: (preemptible: boolean) => api.projects.update(projectId, { preemptible }),
    onSuccess: () => { toast({ title: 'Preemption setting saved' }); qc.invalidateQueries({ queryKey: ['project', projectId] }) },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  return (
    <Card>
      <CardHeader><CardTitle className="text-base">Runtime Protection</CardTitle></CardHeader>
      <CardContent className="space-y-4">
        <div className="space-y-2">
          <label className="flex items-center gap-2 cursor-pointer">
            <input type="checkbox" checked={alwaysRunning} onChange={e => setAlwaysRunning(e.target.checked)} className="w-4 h-4" />
            <span className="text-sm font-medium">Always running</span>
            <Zap className="w-4 h-4 text-green-600" />
          </label>
          <p className="text-xs text-muted-foreground pl-6">Idle Manager never automatically unloads runtimes for this project.</p>
          <label className="flex items-center gap-2 cursor-pointer">
            <input type="checkbox" checked={prot} onChange={e => setProt(e.target.checked)} className="w-4 h-4" />
            <span className="text-sm font-medium">Protected from preemption</span>
            <Shield className="w-4 h-4 text-purple-600" />
          </label>
          <p className="text-xs text-muted-foreground pl-6">Preemption Engine never evicts runtimes for this project under resource pressure.</p>
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <Label className="text-xs">Minimum replicas (0–100)</Label>
            <Input type="number" min={0} max={100} value={minReplicas} onChange={e => setMinReplicas(e.target.value)} className="mt-1" />
          </div>
          <div>
            <Label className="text-xs">Admission policy</Label>
            <select className="w-full border rounded-md h-9 px-3 text-sm mt-1"
              value={policy} onChange={e => setPolicy(e.target.value as AdmissionPolicy)}>
              <option value="queue">Queue (default)</option>
              <option value="preempt_then_queue">Preempt then queue</option>
              <option value="reject">Reject immediately</option>
            </select>
          </div>
        </div>
        <div className="flex items-center gap-3">
          <Button size="sm" onClick={() => protMut.mutate()} disabled={protMut.isPending}>
            {protMut.isPending ? 'Saving…' : 'Save Protection'}
          </Button>
          <label className="flex items-center gap-2 cursor-pointer">
            <input type="checkbox" checked={current.preemptible}
              onChange={e => preemptMut.mutate(e.target.checked)} className="w-4 h-4" />
            <span className="text-sm">Preemptible</span>
            <span className="text-xs text-muted-foreground">(allow other projects to evict this project's runtimes)</span>
          </label>
        </div>
      </CardContent>
    </Card>
  )
}

// ── Queue panel ────────────────────────────────────────────────────────────────
function QueuePanel({ projectId }: { projectId: string }) {
  const { data } = useQuery({
    queryKey: ['project-queue', projectId],
    queryFn: () => api.projects.getQueue(projectId),
    refetchInterval: 15_000,
  })
  const rows = data?.data ?? []
  if (rows.length === 0) return (
    <p className="text-sm text-muted-foreground py-4 text-center">No queued deployments.</p>
  )
  const waitMins = (since: string) => Math.round((Date.now() - new Date(since).getTime()) / 60000)

  return (
    <table className="w-full text-sm">
      <thead>
        <tr className="border-b text-xs text-muted-foreground">
          <th className="text-left pb-2">Model</th>
          <th className="text-left pb-2">Eff. Priority</th>
          <th className="text-left pb-2">Resources</th>
          <th className="text-left pb-2">Wait</th>
          <th className="text-left pb-2">Attempts</th>
          <th className="text-left pb-2">Status</th>
        </tr>
      </thead>
      <tbody>
        {rows.map(q => (
          <tr key={q.id} className="border-b last:border-0">
            <td className="py-2 font-medium">{q.model_name || '—'}</td>
            <td className="py-2">
              <div className="flex items-center gap-1.5">
                <span className="font-semibold">{q.effective_priority}</span>
                <span className="text-xs text-muted-foreground">/{q.priority_weight}</span>
              </div>
              <PriorityBar weight={q.effective_priority} className="w-16 mt-0.5" />
            </td>
            <td className="py-2 text-xs text-muted-foreground">
              {q.required_vram_mb > 0 && <span>{Math.round(q.required_vram_mb/1024)}GB VRAM </span>}
              {q.required_ram_mb > 0 && <span>{Math.round(q.required_ram_mb/1024)}GB RAM </span>}
              {q.required_cpu > 0 && <span>{q.required_cpu} CPU</span>}
            </td>
            <td className="py-2 text-xs">{waitMins(q.waiting_since)}m</td>
            <td className="py-2 text-xs">{q.attempts}</td>
            <td className="py-2">
              <span className={`px-2 py-0.5 rounded-full text-xs ${q.status === 'pending' ? 'bg-yellow-100 text-yellow-800' : 'bg-gray-100 text-gray-600'}`}>
                {q.status}
              </span>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

// ── Preemption history ────────────────────────────────────────────────────────
function PreemptionHistory({ projectId }: { projectId: string }) {
  const [page, setPage] = useState(0)
  const limit = 10
  const { data } = useQuery({
    queryKey: ['project-preemptions', projectId, page],
    queryFn: () => api.projects.getPreemptions(projectId, limit, page * limit),
  })
  const events = data?.data ?? []
  if (events.length === 0) return (
    <p className="text-sm text-muted-foreground py-4 text-center">No preemption events recorded.</p>
  )
  const triggerColor = (t: string) => ({
    gpu_utilization:   'bg-orange-100 text-orange-700',
    vram_exhaustion:   'bg-red-100 text-red-700',
    memory_exhaustion: 'bg-yellow-100 text-yellow-700',
    admission:         'bg-blue-100 text-blue-700',
  }[t] ?? 'bg-gray-100 text-gray-600')

  return (
    <div className="space-y-2">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b text-xs text-muted-foreground">
            <th className="text-left pb-2">Preempted weight</th>
            <th className="text-left pb-2">Requesting weight</th>
            <th className="text-left pb-2">Trigger</th>
            <th className="text-left pb-2">Time</th>
          </tr>
        </thead>
        <tbody>
          {events.map(ev => (
            <tr key={ev.id} className="border-b last:border-0">
              <td className="py-2">
                {ev.preempted_weight != null
                  ? <PriorityBadge weight={ev.preempted_weight} showWeight />
                  : <span className="text-xs text-muted-foreground">—</span>}
              </td>
              <td className="py-2">
                {ev.requesting_weight != null
                  ? <PriorityBadge weight={ev.requesting_weight} showWeight />
                  : <span className="text-xs text-muted-foreground">—</span>}
              </td>
              <td className="py-2">
                <span className={`px-2 py-0.5 rounded-full text-xs ${triggerColor(ev.trigger)}`}>{ev.trigger}</span>
              </td>
              <td className="py-2 text-xs text-muted-foreground">{new Date(ev.created_at).toLocaleString()}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <div className="flex gap-2 justify-end">
        <Button size="sm" variant="outline" disabled={page === 0} onClick={() => setPage(p => p - 1)}>← Prev</Button>
        <Button size="sm" variant="outline" disabled={events.length < limit} onClick={() => setPage(p => p + 1)}>Next →</Button>
      </div>
    </div>
  )
}

// ── Runtimes table ────────────────────────────────────────────────────────────
function RuntimesTable({ projectId }: { projectId: string }) {
  const { data, isLoading } = useQuery({
    queryKey: ['project-runtimes', projectId],
    queryFn: () => api.projects.getRuntimes(projectId),
    refetchInterval: 8_000,
  })
  const rows = data?.data ?? []
  const stateColor = (s: string) => {
    if (['active','warm'].includes(s)) return 'bg-green-100 text-green-800'
    if (['loading','starting'].includes(s)) return 'bg-blue-100 text-blue-800'
    if (['idle','stopped'].includes(s)) return 'bg-yellow-100 text-yellow-800'
    return 'bg-gray-100 text-gray-600'
  }
  if (isLoading) return <p className="text-xs text-muted-foreground">Loading runtimes…</p>
  if (rows.length === 0) return <p className="text-sm text-muted-foreground py-4 text-center">No active runtimes.</p>
  return (
    <table className="w-full text-sm">
      <thead><tr className="border-b text-xs text-muted-foreground">
        <th className="text-left pb-2">Runtime ID</th>
        <th className="text-left pb-2">State</th>
        <th className="text-left pb-2">Endpoint</th>
        <th className="text-left pb-2">Last used</th>
      </tr></thead>
      <tbody>
        {rows.map(rt => (
          <tr key={rt.id} className="border-b last:border-0">
            <td className="py-2 font-mono text-xs">{rt.id.slice(0,8)}…</td>
            <td className="py-2"><span className={`px-2 py-0.5 rounded-full text-xs font-medium ${stateColor(rt.state)}`}>{rt.state}</span></td>
            <td className="py-2 font-mono text-xs">{rt.bind_host}:{rt.bind_port}</td>
            <td className="py-2 text-xs text-muted-foreground">{rt.last_used_at ? new Date(rt.last_used_at).toLocaleTimeString() : '—'}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

// ── Main detail page ──────────────────────────────────────────────────────────
export default function ProjectDetailPage() {
  const params  = useParams<{ id: string }>()
  const router  = useRouter()
  const id      = params.id

  // ── All hooks must be declared before any conditional returns ──────────────
  const { data: project, isLoading, error } = useQuery({
    queryKey: ['project', id],
    queryFn:  () => api.projects.get(id),
    refetchInterval: 30_000,
  })
  const { data: presetData } = useQuery({
    queryKey: ['priority-presets'],
    queryFn: api.scheduler.getPriorityPresets,
    staleTime: Infinity,
  })
  const { data: usageData } = useQuery({
    queryKey: ['project-usage', id],
    queryFn: () => {
      const from = new Date(Date.now() - 30 * 86400 * 1000).toISOString()
      const to   = new Date().toISOString()
      return api.projects.getUsage(id, from, to)
    },
    refetchInterval: 60_000,
    enabled: !!id,
  })

  const presets = presetData?.presets ?? []

  // ── Conditional returns AFTER all hooks ────────────────────────────────────
  if (isLoading) return <div className="text-muted-foreground text-sm p-8">Loading…</div>
  if (error || !project) return (
    <div className="p-8 space-y-4">
      <p className="text-red-600">Project not found.</p>
      <Button variant="ghost" onClick={() => router.push('/projects')}><ArrowLeft className="w-4 h-4 mr-2" />Back</Button>
    </div>
  )

  const stats = usageData ? [
    { label: 'Requests',    value: usageData.total_requests.toLocaleString(),        icon: Activity },
    { label: 'Tokens',      value: (usageData.total_tokens / 1000).toFixed(1) + 'K', icon: Layers },
    { label: 'Avg latency', value: usageData.avg_latency_ms.toFixed(0) + 'ms',       icon: Clock },
    { label: 'Cost',        value: '$' + usageData.cost_usd.toFixed(4),              icon: DollarSign },
    { label: 'Errors',      value: usageData.error_count.toLocaleString(),           icon: AlertTriangle },
    { label: 'Preemptions', value: usageData.preemption_count.toLocaleString(),      icon: Zap },
  ] : []

  return (
    <div className="space-y-6">
      <div>
        <Button variant="ghost" size="sm" className="mb-3 -ml-2 text-muted-foreground"
          onClick={() => router.push('/projects')}>
          <ArrowLeft className="w-4 h-4 mr-1" />Projects
        </Button>
        <div className="flex items-start justify-between gap-4 flex-wrap">
          <div>
            <div className="flex items-center gap-3 flex-wrap">
              <h1 className="text-2xl font-bold">{project.name}</h1>
              <PriorityBadge weight={project.priority_weight} label={project.priority_label} showWeight />
              <span className={`px-2 py-0.5 rounded-full text-xs font-medium ${
                project.status === 'active' ? 'bg-green-100 text-green-700' : 'bg-gray-100 text-gray-500'
              }`}>{project.status}</span>
              {!project.preemptible && (
                <span className="text-xs text-purple-700 bg-purple-50 px-2 py-0.5 rounded border border-purple-200">non-preemptible</span>
              )}
              {project.protected && (
                <span className="flex items-center gap-1 text-xs text-purple-700 bg-purple-50 px-2 py-0.5 rounded border border-purple-200">
                  <Shield className="w-3 h-3" />protected
                </span>
              )}
              {project.always_running && (
                <span className="flex items-center gap-1 text-xs text-green-700 bg-green-50 px-2 py-0.5 rounded border border-green-200">
                  <Zap className="w-3 h-3" />always-running
                </span>
              )}
            </div>
            {project.description && <p className="text-muted-foreground mt-1">{project.description}</p>}
          </div>
          <div className="flex gap-4 text-sm text-muted-foreground">
            <div><span className="font-medium text-foreground">{project.runtime_count}</span> active runtimes</div>
            {project.reserved_vram_mb > 0 && (
              <div><span className="font-medium text-foreground">{(project.reserved_vram_mb/1024).toFixed(0)} GB</span> reserved VRAM</div>
            )}
          </div>
        </div>
      </div>

      {/* Usage summary */}
      {usageData && (
        <Card>
          <CardHeader><CardTitle className="text-base flex items-center gap-2">
            <BarChart2 className="w-4 h-4" />Last 30 Days Usage
          </CardTitle></CardHeader>
          <CardContent>
            <div className="grid grid-cols-3 gap-3">
              {stats.map(s => (
                <div key={s.label} className="flex items-center gap-3 p-3 rounded-lg border bg-white">
                  <s.icon className="w-4 h-4 text-muted-foreground shrink-0" />
                  <div>
                    <div className="text-xs text-muted-foreground">{s.label}</div>
                    <div className="font-semibold text-sm">{s.value}</div>
                  </div>
                </div>
              ))}
            </div>
          </CardContent>
        </Card>
      )}

      {/* Controls */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
        <PriorityPanel projectId={id} current={{
          priority_weight: project.priority_weight,
          effective_priority: project.effective_priority,
          waiting_bonus: project.waiting_bonus,
          reservation_bonus: project.reservation_bonus,
          resource_penalty: project.resource_penalty,
        }} presets={presets} />
        <ReservationPanel projectId={id} current={{
          reserved_vram_mb: project.reserved_vram_mb,
          reserved_cpu_cores: project.reserved_cpu_cores,
          reserved_memory_mb: project.reserved_memory_mb,
          max_gpu_vram_mb: project.max_gpu_vram_mb,
          max_cpu: project.max_cpu,
          max_memory_mb: project.max_memory_mb,
        }} />
      </div>

      <ProtectionPanel projectId={id} current={{
        always_running: project.always_running, protected: project.protected,
        minimum_replicas: project.minimum_replicas, admission_policy: project.admission_policy,
        preemptible: project.preemptible,
      }} />

      {/* Queue */}
      <Card>
        <CardHeader><CardTitle className="text-base flex items-center gap-2">
          <Clock className="w-4 h-4" />Deployment Queue
        </CardTitle></CardHeader>
        <CardContent><QueuePanel projectId={id} /></CardContent>
      </Card>

      {/* Runtimes */}
      <Card>
        <CardHeader><CardTitle className="text-base flex items-center gap-2">
          <Server className="w-4 h-4" />Active Runtimes
        </CardTitle></CardHeader>
        <CardContent><RuntimesTable projectId={id} /></CardContent>
      </Card>

      {/* Preemption history */}
      <Card>
        <CardHeader><CardTitle className="text-base flex items-center gap-2">
          <AlertTriangle className="w-4 h-4" />Preemption History
        </CardTitle></CardHeader>
        <CardContent><PreemptionHistory projectId={id} /></CardContent>
      </Card>

      {/* Metadata */}
      <Card>
        <CardHeader><CardTitle className="text-base">Metadata</CardTitle></CardHeader>
        <CardContent className="text-sm grid grid-cols-2 gap-2 text-muted-foreground">
          <div><span className="font-medium text-foreground">ID:</span> <span className="font-mono">{project.id}</span></div>
          <div><span className="font-medium text-foreground">Team:</span> {project.team_id}</div>
          <div><span className="font-medium text-foreground">Org:</span> {project.organization_id}</div>
          <div><span className="font-medium text-foreground">Admission:</span> {project.admission_policy}</div>
          <div><span className="font-medium text-foreground">Created:</span> {new Date(project.created_at).toLocaleString()}</div>
          <div><span className="font-medium text-foreground">Updated:</span> {new Date(project.updated_at).toLocaleString()}</div>
        </CardContent>
      </Card>
    </div>
  )
}
