'use client'

import { useState } from 'react'
import { useParams, useRouter } from 'next/navigation'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type ProjectPriority, type AdmissionPolicy } from '@/lib/api'
import { PriorityBadge } from '@/components/projects/PriorityBadge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { toast } from '@/components/ui/toaster'
import {
  ArrowLeft, Shield, Zap, Activity, AlertTriangle,
  Server, BarChart2, Clock, DollarSign, Layers
} from 'lucide-react'

const PRIORITIES: ProjectPriority[] = ['CRITICAL', 'HIGH', 'NORMAL', 'LOW', 'BEST_EFFORT']

// ── Priority panel ─────────────────────────────────────────────────────────────
function PriorityPanel({ projectId, current }: { projectId: string; current: ProjectPriority }) {
  const qc = useQueryClient()
  const [val, setVal] = useState(current)

  const mut = useMutation({
    mutationFn: () => api.projects.setPriority(projectId, val),
    onSuccess: (r) => {
      if (r.changed) {
        toast({ title: 'Priority updated', description: `${r.old_priority} → ${r.new_priority}` })
      } else {
        toast({ title: 'No change', description: 'Priority is already ' + val })
      }
      qc.invalidateQueries({ queryKey: ['project', projectId] })
      qc.invalidateQueries({ queryKey: ['projects'] })
    },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  return (
    <Card>
      <CardHeader><CardTitle className="text-base">Priority Management</CardTitle></CardHeader>
      <CardContent className="space-y-3">
        <div className="flex items-center gap-2">
          <span className="text-sm text-muted-foreground">Current:</span>
          <PriorityBadge priority={current} />
        </div>
        <div className="flex gap-2 items-end">
          <div className="flex-1">
            <Label className="text-xs">Change to</Label>
            <select className="w-full border rounded-md h-9 px-3 text-sm mt-1"
              value={val} onChange={e => setVal(e.target.value as ProjectPriority)}>
              {PRIORITIES.map(p => <option key={p} value={p}>{p}</option>)}
            </select>
          </div>
          <Button size="sm" onClick={() => mut.mutate()} disabled={mut.isPending}>
            {mut.isPending ? 'Saving…' : 'Apply'}
          </Button>
        </div>
        <p className="text-xs text-muted-foreground">
          Priority change takes effect on the next scheduler tick (within 60 seconds).
          Change is recorded in the audit log.
        </p>
      </CardContent>
    </Card>
  )
}

// ── Reservation panel ──────────────────────────────────────────────────────────
function ReservationPanel({ projectId, current }: {
  projectId: string
  current: { reserved_vram_mb: number; reserved_cpu_cores: number; reserved_memory_mb: number }
}) {
  const qc = useQueryClient()
  const [vram, setVram]   = useState(String(current.reserved_vram_mb))
  const [cpu, setCpu]     = useState(String(current.reserved_cpu_cores))
  const [mem, setMem]     = useState(String(current.reserved_memory_mb))

  const mut = useMutation({
    mutationFn: () => api.projects.reserve(projectId, {
      reserved_vram_mb:   parseInt(vram) || 0,
      reserved_cpu_cores: parseInt(cpu)  || 0,
      reserved_memory_mb: parseInt(mem)  || 0,
    }),
    onSuccess: () => {
      toast({ title: 'Reservation updated' })
      qc.invalidateQueries({ queryKey: ['project', projectId] })
    },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  return (
    <Card>
      <CardHeader><CardTitle className="text-base">Resource Reservation</CardTitle></CardHeader>
      <CardContent className="space-y-3">
        <div className="grid grid-cols-3 gap-3">
          <div>
            <Label className="text-xs">Reserved VRAM (MB)</Label>
            <Input type="number" min={0} value={vram} onChange={e => setVram(e.target.value)} className="mt-1" />
            <p className="text-xs text-muted-foreground mt-0.5">{Math.round(parseInt(vram || '0') / 1024)} GB</p>
          </div>
          <div>
            <Label className="text-xs">Reserved CPU cores</Label>
            <Input type="number" min={0} value={cpu} onChange={e => setCpu(e.target.value)} className="mt-1" />
          </div>
          <div>
            <Label className="text-xs">Reserved Memory (MB)</Label>
            <Input type="number" min={0} value={mem} onChange={e => setMem(e.target.value)} className="mt-1" />
            <p className="text-xs text-muted-foreground mt-0.5">{Math.round(parseInt(mem || '0') / 1024)} GB</p>
          </div>
        </div>
        <Button size="sm" onClick={() => mut.mutate()} disabled={mut.isPending}>
          {mut.isPending ? 'Saving…' : 'Update Reservation'}
        </Button>
        <p className="text-xs text-muted-foreground">
          Reserved resources are guaranteed for this project and cannot be consumed by lower-priority projects.
          Set to 0 to remove reservation.
        </p>
      </CardContent>
    </Card>
  )
}

// ── Protection panel ──────────────────────────────────────────────────────────
function ProtectionPanel({ projectId, current }: {
  projectId: string
  current: { always_running: boolean; protected: boolean; minimum_replicas: number; admission_policy: AdmissionPolicy }
}) {
  const qc = useQueryClient()
  const [alwaysRunning, setAlwaysRunning] = useState(current.always_running)
  const [prot, setProt]                   = useState(current.protected)
  const [minReplicas, setMinReplicas]     = useState(String(current.minimum_replicas))
  const [policy, setPolicy]               = useState<AdmissionPolicy>(current.admission_policy)

  const mut = useMutation({
    mutationFn: () => api.projects.setProtection(projectId, {
      always_running:   alwaysRunning,
      protected:        prot,
      minimum_replicas: parseInt(minReplicas) || 0,
      admission_policy: policy,
    }),
    onSuccess: () => {
      toast({ title: 'Protection settings saved' })
      qc.invalidateQueries({ queryKey: ['project', projectId] })
    },
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
          <p className="text-xs text-muted-foreground pl-6">Idle Manager will never automatically unload runtimes for this project.</p>

          <label className="flex items-center gap-2 cursor-pointer">
            <input type="checkbox" checked={prot} onChange={e => setProt(e.target.checked)} className="w-4 h-4" />
            <span className="text-sm font-medium">Protected from preemption</span>
            <Shield className="w-4 h-4 text-purple-600" />
          </label>
          <p className="text-xs text-muted-foreground pl-6">Preemption Engine will never evict runtimes for this project, even under resource pressure.</p>
        </div>

        <div className="grid grid-cols-2 gap-3">
          <div>
            <Label className="text-xs">Minimum replicas (0–100)</Label>
            <Input type="number" min={0} max={100} value={minReplicas}
              onChange={e => setMinReplicas(e.target.value)} className="mt-1" />
            <p className="text-xs text-muted-foreground mt-0.5">Idle Manager maintains at least this many active runtimes.</p>
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

        <Button size="sm" onClick={() => mut.mutate()} disabled={mut.isPending}>
          {mut.isPending ? 'Saving…' : 'Save Protection Settings'}
        </Button>
      </CardContent>
    </Card>
  )
}

// ── Usage summary ─────────────────────────────────────────────────────────────
function UsageSummary({ projectId }: { projectId: string }) {
  const from = new Date(Date.now() - 30 * 86400 * 1000).toISOString()
  const to   = new Date().toISOString()

  const { data, isLoading } = useQuery({
    queryKey: ['project-usage', projectId],
    queryFn:  () => api.projects.getUsage(projectId, from, to),
    refetchInterval: 60_000,
  })

  if (isLoading) return <p className="text-xs text-muted-foreground">Loading usage…</p>
  if (!data) return null

  const stats = [
    { label: 'Requests',   value: data.total_requests.toLocaleString(),          icon: Activity },
    { label: 'Tokens',     value: (data.total_tokens / 1000).toFixed(1) + 'K',   icon: Layers },
    { label: 'Avg latency',value: data.avg_latency_ms.toFixed(0) + 'ms',         icon: Clock },
    { label: 'Cost',       value: '$' + data.cost_usd.toFixed(4),                icon: DollarSign },
    { label: 'Errors',     value: data.error_count.toLocaleString(),             icon: AlertTriangle },
    { label: 'Preemptions',value: data.preemption_count.toLocaleString(),        icon: Zap },
  ]

  return (
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
  )
}

// ── Preemption history table ──────────────────────────────────────────────────
function PreemptionHistory({ projectId }: { projectId: string }) {
  const [page, setPage] = useState(0)
  const limit = 10

  const { data } = useQuery({
    queryKey: ['project-preemptions', projectId, page],
    queryFn:  () => api.projects.getPreemptions(projectId, limit, page * limit),
  })

  const events = data?.data ?? []
  if (events.length === 0) return (
    <p className="text-sm text-muted-foreground py-4 text-center">No preemption events recorded.</p>
  )

  const triggerColor = (t: string) => ({
    gpu_utilization: 'bg-orange-100 text-orange-700',
    vram_exhaustion:  'bg-red-100 text-red-700',
    memory_exhaustion:'bg-yellow-100 text-yellow-700',
    admission:        'bg-blue-100 text-blue-700',
  }[t] ?? 'bg-gray-100 text-gray-600')

  return (
    <div className="space-y-2">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b text-xs text-muted-foreground">
            <th className="text-left pb-2">Preempted priority</th>
            <th className="text-left pb-2">Requesting priority</th>
            <th className="text-left pb-2">Trigger</th>
            <th className="text-left pb-2">Time</th>
          </tr>
        </thead>
        <tbody>
          {events.map(ev => (
            <tr key={ev.id} className="border-b last:border-0">
              <td className="py-2">
                {ev.preempted_priority
                  ? <span className="text-xs font-medium">{ev.preempted_priority}</span>
                  : <span className="text-xs text-muted-foreground">—</span>}
              </td>
              <td className="py-2">
                {ev.requesting_priority
                  ? <span className="text-xs font-medium text-green-700">{ev.requesting_priority}</span>
                  : <span className="text-xs text-muted-foreground">—</span>}
              </td>
              <td className="py-2">
                <span className={`px-2 py-0.5 rounded-full text-xs ${triggerColor(ev.trigger)}`}>
                  {ev.trigger}
                </span>
              </td>
              <td className="py-2 text-xs text-muted-foreground">
                {new Date(ev.created_at).toLocaleString()}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <div className="flex gap-2 justify-end">
        <Button size="sm" variant="outline" disabled={page === 0} onClick={() => setPage(p => p - 1)}>
          ← Prev
        </Button>
        <Button size="sm" variant="outline" disabled={events.length < limit} onClick={() => setPage(p => p + 1)}>
          Next →
        </Button>
      </div>
    </div>
  )
}

// ── Active runtimes table ─────────────────────────────────────────────────────
function RuntimesTable({ projectId }: { projectId: string }) {
  const { data, isLoading } = useQuery({
    queryKey: ['project-runtimes', projectId],
    queryFn:  () => api.projects.getRuntimes(projectId),
    refetchInterval: 8_000,
  })

  const rows = data?.data ?? []

  const stateColor = (s: string) => {
    if (['active', 'warm'].includes(s)) return 'bg-green-100 text-green-800'
    if (['loading', 'starting'].includes(s)) return 'bg-blue-100 text-blue-800'
    if (['idle', 'stopped'].includes(s)) return 'bg-yellow-100 text-yellow-800'
    return 'bg-gray-100 text-gray-600'
  }

  if (isLoading) return <p className="text-xs text-muted-foreground">Loading runtimes…</p>
  if (rows.length === 0) return <p className="text-sm text-muted-foreground py-4 text-center">No active runtimes.</p>

  return (
    <table className="w-full text-sm">
      <thead>
        <tr className="border-b text-xs text-muted-foreground">
          <th className="text-left pb-2">Runtime ID</th>
          <th className="text-left pb-2">State</th>
          <th className="text-left pb-2">Endpoint</th>
          <th className="text-left pb-2">Last used</th>
        </tr>
      </thead>
      <tbody>
        {rows.map(rt => (
          <tr key={rt.id} className="border-b last:border-0">
            <td className="py-2 font-mono text-xs">{rt.id.slice(0, 8)}…</td>
            <td className="py-2">
              <span className={`px-2 py-0.5 rounded-full text-xs font-medium ${stateColor(rt.state)}`}>
                {rt.state}
              </span>
            </td>
            <td className="py-2 font-mono text-xs">{rt.bind_host}:{rt.bind_port}</td>
            <td className="py-2 text-xs text-muted-foreground">
              {rt.last_used_at ? new Date(rt.last_used_at).toLocaleTimeString() : '—'}
            </td>
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

  const { data: project, isLoading, error } = useQuery({
    queryKey: ['project', id],
    queryFn:  () => api.projects.get(id),
    refetchInterval: 30_000,
  })

  if (isLoading) return <div className="text-muted-foreground text-sm p-8">Loading…</div>
  if (error || !project) return (
    <div className="p-8 space-y-4">
      <p className="text-red-600">Project not found.</p>
      <Button variant="ghost" onClick={() => router.push('/projects')}>
        <ArrowLeft className="w-4 h-4 mr-2" />Back to Projects
      </Button>
    </div>
  )

  return (
    <div className="space-y-6">
      {/* Breadcrumb + header */}
      <div>
        <Button variant="ghost" size="sm" className="mb-3 -ml-2 text-muted-foreground"
          onClick={() => router.push('/projects')}>
          <ArrowLeft className="w-4 h-4 mr-1" />Projects
        </Button>
        <div className="flex items-start justify-between gap-4 flex-wrap">
          <div>
            <div className="flex items-center gap-3 flex-wrap">
              <h1 className="text-2xl font-bold">{project.name}</h1>
              <PriorityBadge priority={project.priority} />
              <span className={`px-2 py-0.5 rounded-full text-xs font-medium ${
                project.status === 'active' ? 'bg-green-100 text-green-700' : 'bg-gray-100 text-gray-500'
              }`}>{project.status}</span>
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
              <div><span className="font-medium text-foreground">{(project.reserved_vram_mb / 1024).toFixed(0)} GB</span> reserved VRAM</div>
            )}
          </div>
        </div>
      </div>

      {/* Usage summary */}
      <Card>
        <CardHeader><CardTitle className="text-base flex items-center gap-2">
          <BarChart2 className="w-4 h-4" />Last 30 Days Usage
        </CardTitle></CardHeader>
        <CardContent><UsageSummary projectId={id} /></CardContent>
      </Card>

      {/* Controls grid */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
        <PriorityPanel projectId={id} current={project.priority} />
        <ReservationPanel projectId={id} current={{
          reserved_vram_mb:   project.reserved_vram_mb,
          reserved_cpu_cores: project.reserved_cpu_cores,
          reserved_memory_mb: project.reserved_memory_mb,
        }} />
      </div>

      <ProtectionPanel projectId={id} current={{
        always_running:   project.always_running,
        protected:        project.protected,
        minimum_replicas: project.minimum_replicas,
        admission_policy: project.admission_policy,
      }} />

      {/* Active runtimes */}
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
          <div><span className="font-medium text-foreground">Admission policy:</span> {project.admission_policy}</div>
          <div><span className="font-medium text-foreground">Created:</span> {new Date(project.created_at).toLocaleString()}</div>
          <div><span className="font-medium text-foreground">Updated:</span> {new Date(project.updated_at).toLocaleString()}</div>
        </CardContent>
      </Card>
    </div>
  )
}
