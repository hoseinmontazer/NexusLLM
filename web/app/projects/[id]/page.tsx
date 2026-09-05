'use client'

import { useState } from 'react'
import { useParams, useRouter } from 'next/navigation'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type AdmissionPolicy, type PriorityPreset, type ProjectPolicy, type CatalogProvider, type ProjectProviderAccess, type ProviderCredential, type Model } from '@/lib/api'
import { PriorityBadge, PriorityBar, EffectivePriorityCard, weightLabel } from '@/components/projects/PriorityBadge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { toast } from '@/components/ui/toaster'
import {
  ArrowLeft, Shield, Zap, Activity, AlertTriangle,
  Server, BarChart2, Clock, DollarSign, Layers, Gauge,
  Settings2, TrendingUp, Percent, Globe, CheckCircle2,
  Loader2, Trash2, Plus, ShieldCheck, Cpu, X, KeyRound,
} from 'lucide-react'

// ── Policy & Quota panel ──────────────────────────────────────────────────────
function PolicyPanel({ projectId }: { projectId: string }) {
  const qc = useQueryClient()

  const { data: policy, isLoading } = useQuery({
    queryKey: ['project-policy', projectId],
    queryFn: () => api.projectPolicy.getPolicy(projectId),
  })
  const { data: quota } = useQuery({
    queryKey: ['project-quota', projectId],
    queryFn: () => api.projectPolicy.getQuota(projectId),
    refetchInterval: 15_000,
  })

  const [form, setForm] = useState<Partial<ProjectPolicy>>({})
  const set = (k: keyof ProjectPolicy) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm(p => ({ ...p, [k]: e.target.value === '' ? undefined : Number(e.target.value) }))

  const mut = useMutation({
    mutationFn: () => api.projectPolicy.updatePolicy(projectId, form),
    onSuccess: () => {
      toast({ title: 'Policy updated — takes effect immediately' })
      qc.invalidateQueries({ queryKey: ['project-policy', projectId] })
      qc.invalidateQueries({ queryKey: ['project-quota', projectId] })
      setForm({})
    },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  if (isLoading || !policy) return <p className="text-sm text-muted-foreground">Loading policy…</p>

  const fmtTokens = (n: number) => n === 0 ? 'Unlimited' : n.toLocaleString()
  const pct = (used: number, budget: number) => budget > 0 ? Math.min(100, Math.round(used / budget * 100)) : 0

  return (
    <div className="space-y-6">
      {/* Live quota status */}
      {quota && (
        <Card>
          <CardHeader><CardTitle className="text-base flex items-center gap-2">
            <Activity className="w-4 h-4" />Live Quota Status
          </CardTitle></CardHeader>
          <CardContent>
            <div className="grid grid-cols-3 gap-4 text-sm">
              <div>
                <p className="text-xs text-muted-foreground mb-1">Requests in-flight</p>
                <p className="text-2xl font-bold tabular-nums">{quota.inflight}</p>
                <p className="text-xs text-muted-foreground">/ {quota.max_concurrent_limit || '∞'} max</p>
              </div>
              <div>
                <p className="text-xs text-muted-foreground mb-1">Tokens/min (current)</p>
                <p className="text-2xl font-bold tabular-nums">{quota.tpm_current.toLocaleString()}</p>
                <p className="text-xs text-muted-foreground">/ {fmtTokens(quota.tpm_limit)} limit</p>
              </div>
              <div>
                <p className="text-xs text-muted-foreground mb-1">Tokens today</p>
                <p className="text-2xl font-bold tabular-nums">{quota.daily_tokens_used.toLocaleString()}</p>
                {quota.daily_token_budget > 0 ? (
                  <>
                    <div className="mt-1 h-1.5 bg-gray-100 rounded-full overflow-hidden w-full">
                      <div className={`h-full rounded-full ${pct(quota.daily_tokens_used, quota.daily_token_budget) > 80 ? 'bg-red-500' : 'bg-blue-500'}`}
                        style={{ width: `${pct(quota.daily_tokens_used, quota.daily_token_budget)}%` }} />
                    </div>
                    <p className="text-xs text-muted-foreground mt-0.5">
                      {quota.daily_tokens_remaining?.toLocaleString() ?? '—'} remaining
                    </p>
                  </>
                ) : (
                  <p className="text-xs text-muted-foreground">Unlimited</p>
                )}
              </div>
            </div>
          </CardContent>
        </Card>
      )}

      {/* Policy editor */}
      <Card>
        <CardHeader><CardTitle className="text-base flex items-center gap-2">
          <Settings2 className="w-4 h-4" />Rate Limits &amp; Budgets
        </CardTitle></CardHeader>
        <CardContent className="space-y-4">
          <div className="grid grid-cols-2 gap-3">
            {([
              { key: 'rpm',                  label: 'RPM Limit',              desc: 'Requests per minute (0 = unlimited)', cur: policy.rpm },
              { key: 'tpm',                  label: 'TPM Limit',              desc: 'Tokens per minute (0 = unlimited)',   cur: policy.tpm },
              { key: 'max_concurrent',       label: 'Max Concurrent',         desc: 'Parallel requests (0 = unlimited)',   cur: policy.max_concurrent },
              { key: 'max_context_tokens',   label: 'Max Context Tokens',     desc: 'Per-request token limit (0 = unlimited)', cur: policy.max_context_tokens },
              { key: 'daily_token_budget',   label: 'Daily Token Budget',     desc: 'Total tokens/day (0 = unlimited)',    cur: policy.daily_token_budget },
              { key: 'monthly_token_budget', label: 'Monthly Token Budget',   desc: 'Total tokens/month (0 = unlimited)', cur: policy.monthly_token_budget },
              { key: 'daily_cost_budget',    label: 'Daily Cost Budget ($)',  desc: 'USD/day (0 = unlimited)',             cur: policy.daily_cost_budget },
              { key: 'monthly_cost_budget',  label: 'Monthly Cost Budget ($)',desc: 'USD/month (0 = unlimited)',           cur: policy.monthly_cost_budget },
            ] as const).map(({ key, label, desc, cur }) => (
              <div key={key}>
                <Label className="text-xs">{label}</Label>
                <Input
                  type="number" min={0} step={key.includes('cost') ? '0.01' : '1'}
                  placeholder={String(cur)}
                  value={form[key as keyof ProjectPolicy] ?? ''}
                  onChange={set(key as keyof ProjectPolicy)}
                  className="mt-1"
                />
                <p className="text-xs text-muted-foreground mt-0.5">Current: {fmtTokens(cur as number)} · {desc}</p>
              </div>
            ))}
          </div>
          <Button size="sm" onClick={() => mut.mutate()} disabled={mut.isPending || Object.keys(form).length === 0}>
            {mut.isPending ? 'Saving…' : 'Update Policy'}
          </Button>
          <p className="text-xs text-muted-foreground">Leave blank to keep current value. Changes push to gateway Redis immediately.</p>
        </CardContent>
      </Card>
    </div>
  )
}

// ── Usage analytics panel ─────────────────────────────────────────────────────
function UsageAnalyticsPanel({ projectId }: { projectId: string }) {
  const [range, setRange] = useState<'24h' | '7d' | '30d'>('7d')

  const rangeParams = () => {
    const to = new Date().toISOString()
    const days = range === '24h' ? 1 : range === '7d' ? 7 : 30
    const from = new Date(Date.now() - days * 86400 * 1000).toISOString()
    return { from, to }
  }

  const { from, to } = rangeParams()

  const { data: summary } = useQuery({
    queryKey: ['project-summary', projectId, range],
    queryFn: () => api.projectPolicy.getSummary(projectId, from, to),
    refetchInterval: 60_000,
  })
  const { data: dailyData } = useQuery({
    queryKey: ['project-daily', projectId, range],
    queryFn: () => api.projectPolicy.getDailyUsage(projectId, from, to),
    refetchInterval: 60_000,
  })

  const rows = dailyData?.data ?? []

  const fmt = (n: number) => n >= 1_000_000 ? (n/1_000_000).toFixed(2)+'M' :
                              n >= 1_000     ? (n/1_000).toFixed(1)+'K'    : String(n)

  return (
    <div className="space-y-5">
      {/* Range selector */}
      <div className="flex gap-1">
        {(['24h', '7d', '30d'] as const).map(r => (
          <button key={r} onClick={() => setRange(r)}
            className={`px-3 py-1.5 text-xs font-medium rounded transition-colors ${
              range === r ? 'bg-gray-900 text-white' : 'bg-white border text-muted-foreground hover:bg-gray-50'
            }`}>
            {r}
          </button>
        ))}
      </div>

      {/* Summary cards */}
      {summary && (
        <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
          {[
            { label: 'Requests',      value: summary.request_count.toLocaleString(),     icon: Activity },
            { label: 'Total Tokens',  value: fmt(summary.total_tokens),                  icon: Layers },
            { label: 'Cost (USD)',    value: '$'+summary.cost_usd.toFixed(4),            icon: DollarSign },
            { label: 'Avg Latency',  value: summary.avg_latency_ms.toFixed(0)+'ms',      icon: Clock },
            { label: 'Prompt Tokens',value: fmt(summary.prompt_tokens),                  icon: TrendingUp },
            { label: 'Completion',   value: fmt(summary.completion_tokens),              icon: TrendingUp },
            { label: 'Errors',       value: summary.error_count.toLocaleString(),        icon: AlertTriangle },
            { label: 'Error Rate',   value: summary.request_count > 0 ? (summary.error_count/summary.request_count*100).toFixed(1)+'%' : '0%', icon: Percent },
          ].map(s => (
            <div key={s.label} className="flex items-center gap-3 p-3 rounded-lg border bg-white">
              <s.icon className="w-4 h-4 text-muted-foreground shrink-0" />
              <div>
                <div className="text-xs text-muted-foreground">{s.label}</div>
                <div className="font-semibold text-sm tabular-nums">{s.value}</div>
              </div>
            </div>
          ))}
        </div>
      )}

      {/* Daily breakdown table */}
      {rows.length > 0 && (
        <Card>
          <CardHeader><CardTitle className="text-sm">Daily Breakdown</CardTitle></CardHeader>
          <CardContent className="overflow-x-auto">
            <table className="w-full text-xs">
              <thead>
                <tr className="border-b text-muted-foreground">
                  <th className="text-left pb-2 font-medium">Date</th>
                  <th className="text-left pb-2 font-medium">Model</th>
                  <th className="text-right pb-2 font-medium">Requests</th>
                  <th className="text-right pb-2 font-medium">Tokens</th>
                  <th className="text-right pb-2 font-medium">Cost</th>
                  <th className="text-right pb-2 font-medium">Latency</th>
                  <th className="text-right pb-2 font-medium">Errors</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r, i) => (
                  <tr key={i} className="border-b last:border-0 hover:bg-gray-50">
                    <td className="py-1.5">{r.day}</td>
                    <td className="py-1.5 font-mono">{r.model_name || '—'}</td>
                    <td className="py-1.5 text-right tabular-nums">{r.request_count.toLocaleString()}</td>
                    <td className="py-1.5 text-right tabular-nums">{fmt(r.total_tokens)}</td>
                    <td className="py-1.5 text-right tabular-nums font-mono">${r.cost_usd.toFixed(4)}</td>
                    <td className="py-1.5 text-right tabular-nums">{r.avg_latency_ms.toFixed(0)}ms</td>
                    <td className="py-1.5 text-right tabular-nums">
                      {r.error_count > 0
                        ? <span className="text-red-500">{r.error_count}</span>
                        : <span className="text-green-600">0</span>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </CardContent>
        </Card>
      )}
      {rows.length === 0 && !summary && (
        <p className="text-sm text-muted-foreground text-center py-6">No usage data for this period.</p>
      )}
    </div>
  )
}

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

// ── Model Access panel (Public/managed models) ───────────────────────────────
// Grants this PROJECT access to regular deployed models — narrower than, and
// independent from, Provider Access above (which governs external catalog
// models only). Effective access is team AND project (Option A): a model
// must be granted to both this project's team AND this project to be usable
// by a token scoped to this project. A project with no grants here inherits
// its team's full model access unchanged until the first grant/revoke here.
function ProjectModelAccessPanel({ projectId, teamId }: { projectId: string; teamId?: string | null }) {
  const qc = useQueryClient()
  const [modelInput, setModelInput] = useState('')

  const { data: modelsData, isLoading } = useQuery({
    queryKey: ['project-models', projectId],
    queryFn: () => api.projects.listModels(projectId),
  })
  const { data: teamModelsData } = useQuery({
    queryKey: ['team-models', teamId],
    queryFn: () => api.teams.listModels(teamId as string),
    enabled: !!teamId,
  })
  const { data: allModels } = useQuery({
    queryKey: ['models'],
    queryFn: () => api.models.list(),
  })

  const allModelList: Model[] = allModels?.data ?? []
  const grantedModels = modelsData?.models ?? []
  const grantedNames = new Set(grantedModels.map(g => g.name))
  const unsyncedCount = grantedModels.filter(g => !g.synced).length
  const teamGrantedModels = new Set(teamModelsData?.models ?? [])
  const isConfigured = grantedModels.length > 0
  const ungranted = allModelList.filter(m => !grantedNames.has(m.name))

  // Failed mutations must still invalidate/refetch — otherwise a grant that
  // partially applied (DB committed, Redis sync failed) leaves the cached
  // list stale until some unrelated refetch happens, hiding exactly the
  // divergence the `synced` flag exists to surface.
  const grant = useMutation({
    mutationFn: (name: string) => api.projects.addModel(projectId, name),
    onSuccess: (_, name) => {
      toast({ title: 'Access granted', description: `Project → ${name}` })
      qc.invalidateQueries({ queryKey: ['project-models', projectId] })
      setModelInput('')
    },
    onError: (e: any) => {
      toast({ title: 'Grant failed', description: e.message, variant: 'destructive' })
      qc.invalidateQueries({ queryKey: ['project-models', projectId] })
    },
  })

  const revoke = useMutation({
    mutationFn: (name: string) => api.projects.removeModel(projectId, name),
    onSuccess: (_, name) => {
      toast({ title: 'Access revoked', description: `Project ✕ ${name}` })
      qc.invalidateQueries({ queryKey: ['project-models', projectId] })
    },
    onError: (e: any) => {
      toast({ title: 'Revoke failed', description: e.message, variant: 'destructive' })
      qc.invalidateQueries({ queryKey: ['project-models', projectId] })
    },
  })

  return (
    <Card>
      <CardHeader><CardTitle className="text-base flex items-center gap-2">
        <ShieldCheck className="w-4 h-4" />Model Access
      </CardTitle></CardHeader>
      <CardContent className="space-y-3">
        <p className="text-xs text-muted-foreground">
          Narrows this project's team model access — a model must be granted to both
          the team <em>and</em> this project to be usable by a token scoped to this project.
        </p>

        {isLoading ? (
          <p className="text-xs text-muted-foreground">Loading…</p>
        ) : !isConfigured ? (
          <p className="text-xs text-blue-700 bg-blue-50 border border-blue-100 rounded px-2 py-1.5">
            No project-specific grants yet — this project currently inherits its team's
            full model access unchanged. Granting a model below switches this project
            into restricted mode.
          </p>
        ) : (
          <>
            {unsyncedCount > 0 && (
              <p className="text-xs text-amber-700 bg-amber-50 border border-amber-100 rounded px-2 py-1.5">
                ⚠️ {unsyncedCount} grant{unsyncedCount > 1 ? 's' : ''} not yet active — recorded but not
                confirmed in the live enforcement cache. This self-heals within a few minutes, or trigger
                an immediate repair via <code>POST /admin/v1/system/reconcile-permissions</code>.
              </p>
            )}
            <div className="flex flex-wrap gap-1.5">
              {grantedModels.map(g => (
                <span key={g.name}
                  className={`inline-flex items-center gap-1 text-xs border rounded-full pl-2 pr-1 py-0.5 ${
                    g.synced
                      ? 'bg-green-50 text-green-700 border-green-200'
                      : 'bg-amber-50 text-amber-700 border-amber-200'
                  }`}
                  title={g.synced ? undefined : 'Recorded but not yet confirmed in the live enforcement cache'}
                >
                  <Cpu className="w-2.5 h-2.5 shrink-0" />
                  {g.name}
                  {!g.synced && <span className="text-[10px]">⏳</span>}
                  <button
                    onClick={() => revoke.mutate(g.name)}
                    disabled={revoke.isPending}
                    className="ml-0.5 hover:text-red-600 transition-colors rounded-full p-0.5 hover:bg-red-50"
                    title={`Revoke ${g.name}`}
                  >
                    <X className="w-3 h-3" />
                  </button>
                </span>
              ))}
            </div>
          </>
        )}

        <div className="flex gap-2">
          <div className="flex-1 relative">
            <Input
              value={modelInput}
              onChange={e => setModelInput(e.target.value)}
              placeholder="model name…"
              className="text-sm h-8"
              list={`project-model-list-${projectId}`}
            />
            <datalist id={`project-model-list-${projectId}`}>
              {ungranted.map(m => <option key={m.name} value={m.name} />)}
            </datalist>
          </div>
          <Button
            size="sm"
            className="h-8 shrink-0"
            disabled={!modelInput.trim() || grant.isPending}
            onClick={() => modelInput.trim() && grant.mutate(modelInput.trim())}
          >
            <Plus className="w-3.5 h-3.5 mr-1" />Grant
          </Button>
        </div>

        {ungranted.length > 0 && teamId && (
          <p className="text-[10px] text-muted-foreground flex items-center gap-2">
            <span className="inline-flex items-center gap-1">
              <span className="w-2 h-2 rounded-full bg-gray-300 border border-gray-400 inline-block" />
              gray = not yet granted to this project's team — would be a no-op
            </span>
          </p>
        )}
        {ungranted.length > 0 && (
          <div className="flex flex-wrap gap-1">
            {ungranted.slice(0, 12).map(m => (
              <button
                key={m.name}
                onClick={() => grant.mutate(m.name)}
                disabled={grant.isPending}
                className={`text-[10px] px-2 py-0.5 rounded border border-dashed transition-colors ${
                  teamId && !teamGrantedModels.has(m.name)
                    ? 'border-gray-200 text-gray-400 hover:border-amber-400 hover:text-amber-700 hover:bg-amber-50'
                    : 'border-gray-300 text-muted-foreground hover:border-blue-400 hover:text-blue-700 hover:bg-blue-50'
                }`}
                title={
                  teamId && !teamGrantedModels.has(m.name)
                    ? 'Not yet granted to this project\'s team — granting here alone will not be enough'
                    : m.display_name !== m.name ? m.display_name : undefined
                }
              >
                + {m.name}
              </button>
            ))}
            {ungranted.length > 12 && (
              <span className="text-[10px] text-muted-foreground self-center">
                +{ungranted.length - 12} more — type in the field above
              </span>
            )}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

// ── Provider Access panel (catalog / hybrid mode) ────────────────────────────
// Lets admins grant this project access to one or more catalog/hybrid-mode
// providers. Each grant optionally restricts callable models via glob prefixes.

function ProviderAccessPanel({ projectId }: { projectId: string }) {
  const qc = useQueryClient()

  // Existing grants for this project
  const { data: grantsData, isLoading: grantsLoading } = useQuery({
    queryKey: ['project-provider-access', projectId],
    queryFn: () => api.providerAccess.list(projectId),
    refetchInterval: 30_000,
  })
  const grants: ProjectProviderAccess[] = grantsData?.data ?? []

  // All providers — filtered to catalog/hybrid when displaying grant candidates
  const { data: providersData } = useQuery({
    queryKey: ['providers'],
    queryFn: api.providers.list,
    staleTime: 60_000,
  })
  const allProviders: CatalogProvider[] = providersData?.data ?? []
  const virtualProviders = allProviders.filter(
    p => p.exposure_mode === 'catalog' || p.exposure_mode === 'hybrid',
  )
  // Providers not yet granted to this project
  const grantedIds = new Set(grants.map(g => g.provider_id))
  const availableToGrant = virtualProviders.filter(p => !grantedIds.has(p.id))

  // Grant form state
  const [showForm, setShowForm] = useState(false)
  const [selectedProvider, setSelectedProvider] = useState('')
  const [selectedCredential, setSelectedCredential] = useState('')
  const [allowedRaw, setAllowedRaw] = useState('')
  const [deniedRaw, setDeniedRaw] = useState('')

  // Edit prefix state — keyed by grant provider_id
  const [editingId, setEditingId] = useState<string | null>(null)
  const [editAllowed, setEditAllowed] = useState('')
  const [editDenied, setEditDenied] = useState('')

  // Credential picker for the currently-selected provider in the grant form.
  const { data: newGrantCredsData } = useQuery({
    queryKey: ['provider-credentials', selectedProvider],
    queryFn: () => api.providerCredentials.list(selectedProvider),
    enabled: !!selectedProvider,
  })
  const newGrantCredentials: ProviderCredential[] = newGrantCredsData?.data ?? []

  // Inline "change credential" state — keyed by grant provider_id, separate
  // from the prefix editor so either can be opened independently.
  const [credentialEditingId, setCredentialEditingId] = useState<string | null>(null)
  const [editCredential, setEditCredential] = useState('')
  const { data: editCredsData } = useQuery({
    queryKey: ['provider-credentials', credentialEditingId],
    queryFn: () => api.providerCredentials.list(credentialEditingId!),
    enabled: !!credentialEditingId,
  })
  const editCredentials: ProviderCredential[] = editCredsData?.data ?? []

  const splitPrefixes = (raw: string) =>
    raw.split('\n').map(s => s.trim()).filter(Boolean)

  // Grant
  const grantMut = useMutation({
    mutationFn: () =>
      api.providerAccess.grant(projectId, {
        provider_id: selectedProvider,
        credential_id: selectedCredential || undefined,
        allowed_prefixes: splitPrefixes(allowedRaw),
        denied_prefixes: splitPrefixes(deniedRaw),
      }),
    onSuccess: (r) => {
      toast({ title: `Access granted`, description: `Provider: ${r.provider_name}` })
      qc.invalidateQueries({ queryKey: ['project-provider-access', projectId] })
      setShowForm(false)
      setSelectedProvider('')
      setSelectedCredential('')
      setAllowedRaw('')
      setDeniedRaw('')
    },
    onError: (e: any) => toast({ title: 'Grant failed', description: e.message, variant: 'destructive' }),
  })

  // Update prefixes
  const updateMut = useMutation({
    mutationFn: ({ pid, allowed, denied }: { pid: string; allowed: string[]; denied: string[] }) =>
      api.providerAccess.update(projectId, pid, {
        allowed_prefixes: allowed,
        denied_prefixes: denied,
      }),
    onSuccess: () => {
      toast({ title: 'Prefix rules updated' })
      qc.invalidateQueries({ queryKey: ['project-provider-access', projectId] })
      setEditingId(null)
    },
    onError: (e: any) => toast({ title: 'Update failed', description: e.message, variant: 'destructive' }),
  })

  // Change (or clear) the pinned credential on an existing grant. Passing ''
  // clears the pin (credential_id: null) so the project falls back to the
  // provider's default credential.
  const updateCredentialMut = useMutation({
    mutationFn: ({ pid, credentialId }: { pid: string; credentialId: string }) =>
      api.providerAccess.update(projectId, pid, { credential_id: credentialId || null }),
    onSuccess: () => {
      toast({ title: 'Credential updated' })
      qc.invalidateQueries({ queryKey: ['project-provider-access', projectId] })
      setCredentialEditingId(null)
    },
    onError: (e: any) => toast({ title: 'Update failed', description: e.message, variant: 'destructive' }),
  })

  // Revoke
  const revokeMut = useMutation({
    mutationFn: (pid: string) => api.providerAccess.revoke(projectId, pid),
    onSuccess: () => {
      toast({ title: 'Access revoked' })
      qc.invalidateQueries({ queryKey: ['project-provider-access', projectId] })
    },
    onError: (e: any) => toast({ title: 'Revoke failed', description: e.message, variant: 'destructive' }),
  })

  const selectedProviderObj = allProviders.find(p => p.id === selectedProvider)
  const exposePrefix = selectedProviderObj?.catalog_expose_prefix || selectedProviderObj?.name || ''

  return (
    <div className="space-y-5">
      {/* Header explainer */}
      <div className="rounded-md bg-violet-50 border border-violet-200 px-4 py-3 text-xs text-violet-800 space-y-1">
        <p className="font-semibold flex items-center gap-1.5">
          <Globe className="w-3.5 h-3.5" />Provider Catalog Access
        </p>
        <p>
          Grant this project access to <strong>Catalog</strong> or <strong>Hybrid</strong> mode providers.
          Once granted, users calling the gateway with an API key scoped to this project can access any
          virtual model from the provider — subject to the project's rate limits and quota.
        </p>
        <p>
          Only providers with <strong>exposure_mode = catalog or hybrid</strong> are shown. To add a
          provider, change its mode in <strong>Providers → Overview</strong>.
        </p>
      </div>

      {/* No virtual providers at all */}
      {virtualProviders.length === 0 && !grantsLoading && (
        <div className="rounded-lg border bg-white p-8 text-center space-y-2">
          <Globe className="w-8 h-8 mx-auto text-muted-foreground opacity-30" />
          <p className="text-sm font-medium text-muted-foreground">No catalog/hybrid providers configured</p>
          <p className="text-xs text-muted-foreground">
            Go to <strong>Providers</strong>, open a provider, and set its Exposure Mode to
            <strong> Catalog</strong> or <strong>Hybrid</strong>.
          </p>
        </div>
      )}

      {/* Existing grants */}
      {(grants.length > 0 || grantsLoading) && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base flex items-center gap-2">
              <CheckCircle2 className="w-4 h-4 text-green-600" />
              Active Provider Grants
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            {grantsLoading && (
              <p className="text-sm text-muted-foreground flex items-center gap-2">
                <Loader2 className="w-3.5 h-3.5 animate-spin" />Loading grants…
              </p>
            )}
            {grants.map(g => {
              const isEditing = editingId === g.provider_id
              return (
                <div key={g.id} className={`rounded-lg border p-3 space-y-2 ${
                  g.enabled ? 'border-violet-200 bg-violet-50/40' : 'border-gray-200 bg-gray-50 opacity-60'
                }`}>
                  {/* Grant header */}
                  <div className="flex items-start justify-between gap-3">
                    <div className="flex-1 space-y-0.5">
                      <div className="flex items-center gap-2 flex-wrap">
                        <span className="font-semibold text-sm">{g.provider_name}</span>
                        <span className={`text-[10px] px-1.5 py-0.5 rounded-full border font-medium ${
                          g.exposure_mode === 'catalog'
                            ? 'bg-violet-100 text-violet-700 border-violet-200'
                            : 'bg-blue-100 text-blue-700 border-blue-200'
                        }`}>
                          {g.exposure_mode}
                        </span>
                        <span className={`text-[10px] px-1.5 py-0.5 rounded-full border font-medium ${
                          g.enabled
                            ? 'bg-green-100 text-green-700 border-green-200'
                            : 'bg-gray-100 text-gray-500 border-gray-200'
                        }`}>
                          {g.enabled ? 'active' : 'revoked'}
                        </span>
                      </div>
                      <p className="text-[10px] text-muted-foreground">
                        Granted {new Date(g.created_at).toLocaleDateString()}
                        {g.updated_at !== g.created_at && ` · Updated ${new Date(g.updated_at).toLocaleDateString()}`}
                      </p>
                      {/* Which upstream credential this project's calls to this
                          provider actually use — the answer to "which OpenRouter
                          token is this NexusLLM key connected to?" */}
                      <p className="text-xs flex items-center gap-1.5 mt-1">
                        <KeyRound className="w-3 h-3 text-muted-foreground" />
                        <span className="text-muted-foreground">Credential:</span>
                        {g.credential_name ? (
                          <code className="bg-white border border-amber-200 px-1.5 py-0.5 rounded text-amber-800 font-medium">
                            {g.credential_name}
                          </code>
                        ) : (
                          <span className="text-muted-foreground italic">provider default</span>
                        )}
                      </p>
                    </div>
                    <div className="flex gap-1 shrink-0">
                      <Button
                        size="sm"
                        variant="ghost"
                        className="h-7 px-2 text-xs"
                        onClick={() => {
                          const opening = credentialEditingId !== g.provider_id
                          setCredentialEditingId(opening ? g.provider_id : null)
                          setEditCredential(opening ? (g.credential_id ?? '') : '')
                        }}
                      >
                        {credentialEditingId === g.provider_id ? 'Cancel' : 'Change credential'}
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        className="h-7 px-2 text-xs"
                        onClick={() => {
                          if (isEditing) {
                            setEditingId(null)
                          } else {
                            setEditingId(g.provider_id)
                            setEditAllowed((g.allowed_prefixes ?? []).join('\n'))
                            setEditDenied((g.denied_prefixes ?? []).join('\n'))
                          }
                        }}
                      >
                        {isEditing ? 'Cancel' : 'Edit prefixes'}
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        className="h-7 px-2 text-xs text-red-600 hover:text-red-700 hover:bg-red-50"
                        disabled={revokeMut.isPending || !g.enabled}
                        onClick={() => revokeMut.mutate(g.provider_id)}
                      >
                        <Trash2 className="w-3 h-3" />
                      </Button>
                    </div>
                  </div>

                  {/* Inline credential editor */}
                  {credentialEditingId === g.provider_id && (
                    <div className="space-y-2 pt-1 border-t border-amber-200">
                      <Label className="text-[10px]">
                        Pin a specific credential <span className="text-muted-foreground font-normal">(or leave as "provider default")</span>
                      </Label>
                      <select
                        className="w-full border rounded-md h-9 px-3 text-sm"
                        value={editCredential}
                        onChange={e => setEditCredential(e.target.value)}
                      >
                        <option value="">— provider default —</option>
                        {editCredentials.map(c => (
                          <option key={c.id} value={c.id} disabled={!c.enabled}>
                            {c.name}{c.is_default ? ' (default)' : ''}{!c.enabled ? ' — disabled' : ''}
                          </option>
                        ))}
                      </select>
                      <Button
                        size="sm"
                        disabled={updateCredentialMut.isPending}
                        onClick={() => updateCredentialMut.mutate({ pid: g.provider_id, credentialId: editCredential })}
                      >
                        {updateCredentialMut.isPending ? (
                          <><Loader2 className="w-3 h-3 animate-spin mr-1" />Saving…</>
                        ) : 'Save credential'}
                      </Button>
                    </div>
                  )}

                  {/* Prefix summary — collapsed view */}
                  {!isEditing && (
                    <div className="text-xs space-y-1">
                      {(!g.allowed_prefixes?.length && !g.denied_prefixes?.length) ? (
                        <span className="text-green-700 flex items-center gap-1">
                          <CheckCircle2 className="w-3 h-3" />
                          All virtual models allowed (no prefix restrictions)
                        </span>
                      ) : (
                        <>
                          {g.allowed_prefixes?.length > 0 && (
                            <div className="flex flex-wrap items-center gap-1">
                              <span className="text-muted-foreground">Allow:</span>
                              {g.allowed_prefixes.map(p => (
                                <code key={p} className="bg-white border border-violet-200 px-1.5 py-0.5 rounded text-violet-700">{p}</code>
                              ))}
                            </div>
                          )}
                          {g.denied_prefixes?.length > 0 && (
                            <div className="flex flex-wrap items-center gap-1">
                              <span className="text-muted-foreground">Deny:</span>
                              {g.denied_prefixes.map(p => (
                                <code key={p} className="bg-white border border-red-200 px-1.5 py-0.5 rounded text-red-700">{p}</code>
                              ))}
                            </div>
                          )}
                        </>
                      )}
                    </div>
                  )}

                  {/* Inline prefix editor */}
                  {isEditing && (
                    <div className="space-y-2 pt-1 border-t border-violet-200">
                      <div className="grid grid-cols-2 gap-3">
                        <div>
                          <Label className="text-[10px]">
                            Allowed prefixes <span className="text-muted-foreground font-normal">(one glob per line; empty = allow all)</span>
                          </Label>
                          <textarea
                            rows={3}
                            value={editAllowed}
                            onChange={e => setEditAllowed(e.target.value)}
                            className="w-full mt-1 border rounded-md px-2 py-1.5 text-xs font-mono resize-none focus:outline-none focus:ring-1 focus:ring-blue-400"
                            placeholder={`${g.provider_name}/*`}
                          />
                        </div>
                        <div>
                          <Label className="text-[10px]">
                            Denied prefixes <span className="text-muted-foreground font-normal">(one glob per line)</span>
                          </Label>
                          <textarea
                            rows={3}
                            value={editDenied}
                            onChange={e => setEditDenied(e.target.value)}
                            className="w-full mt-1 border rounded-md px-2 py-1.5 text-xs font-mono resize-none focus:outline-none focus:ring-1 focus:ring-blue-400"
                            placeholder={`${g.provider_name}/openai/gpt-4-*`}
                          />
                        </div>
                      </div>
                      <Button
                        size="sm"
                        disabled={updateMut.isPending}
                        onClick={() =>
                          updateMut.mutate({
                            pid: g.provider_id,
                            allowed: splitPrefixes(editAllowed),
                            denied: splitPrefixes(editDenied),
                          })
                        }
                      >
                        {updateMut.isPending ? (
                          <><Loader2 className="w-3 h-3 animate-spin mr-1" />Saving…</>
                        ) : 'Save prefix rules'}
                      </Button>
                    </div>
                  )}
                </div>
              )
            })}
          </CardContent>
        </Card>
      )}

      {/* Grant new access */}
      {virtualProviders.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base flex items-center justify-between">
              <span className="flex items-center gap-2">
                <Plus className="w-4 h-4" />Grant Provider Access
              </span>
              {!showForm && availableToGrant.length > 0 && (
                <Button size="sm" onClick={() => setShowForm(true)}>
                  + Grant Access
                </Button>
              )}
            </CardTitle>
          </CardHeader>
          <CardContent>
            {availableToGrant.length === 0 && !showForm && (
              <p className="text-sm text-muted-foreground">
                All available catalog/hybrid providers have been granted to this project.
              </p>
            )}

            {showForm && (
              <div className="space-y-4">
                {/* Provider selector */}
                <div>
                  <Label className="text-xs">Provider</Label>
                  <select
                    className="w-full border rounded-md h-9 px-3 text-sm mt-1"
                    value={selectedProvider}
                    onChange={e => {
                      setSelectedProvider(e.target.value)
                      setAllowedRaw('')
                      setDeniedRaw('')
                    }}
                  >
                    <option value="">— select provider —</option>
                    {availableToGrant.map(p => (
                      <option key={p.id} value={p.id}>
                        {p.display_name} ({p.exposure_mode}) · {p.catalog_model_count} models
                      </option>
                    ))}
                  </select>
                </div>

                {selectedProvider && (
                  <>
                    <div>
                      <Label className="text-xs">
                        Credential <span className="text-muted-foreground font-normal">(optional — which upstream token this project uses)</span>
                      </Label>
                      <select
                        className="w-full border rounded-md h-9 px-3 text-sm mt-1"
                        value={selectedCredential}
                        onChange={e => setSelectedCredential(e.target.value)}
                      >
                        <option value="">— provider default —</option>
                        {newGrantCredentials.map(c => (
                          <option key={c.id} value={c.id} disabled={!c.enabled}>
                            {c.name}{c.is_default ? ' (default)' : ''}{!c.enabled ? ' — disabled' : ''}
                          </option>
                        ))}
                      </select>
                      {newGrantCredentials.length === 0 && (
                        <p className="text-[10px] text-muted-foreground mt-1">
                          No named credentials configured for this provider yet — every project will share the
                          provider's single legacy API key until you add some in <strong>Providers → Credentials</strong>.
                        </p>
                      )}
                    </div>

                    <div className="rounded-md bg-gray-50 border px-3 py-2.5 text-xs text-muted-foreground space-y-1">
                      <p className="font-medium text-foreground">
                        Virtual model names from this provider look like:
                      </p>
                      <code className="block bg-white border rounded px-2 py-1 font-mono text-xs">
                        {exposePrefix}/openai/gpt-5<br />
                        {exposePrefix}/anthropic/claude-opus-4<br />
                        {exposePrefix}/google/gemini-2.5-pro
                      </code>
                      <p>Leave prefix rules empty to allow <strong>all</strong> virtual models from this provider.</p>
                    </div>

                    <div className="grid grid-cols-2 gap-3">
                      <div>
                        <Label className="text-xs">
                          Allowed prefixes{' '}
                          <span className="text-muted-foreground font-normal">(one glob per line, empty&nbsp;=&nbsp;all)</span>
                        </Label>
                        <textarea
                          rows={4}
                          value={allowedRaw}
                          onChange={e => setAllowedRaw(e.target.value)}
                          className="w-full mt-1 border rounded-md px-2 py-1.5 text-xs font-mono resize-none focus:outline-none focus:ring-1 focus:ring-blue-400"
                          placeholder={`${exposePrefix}/openai/*\n${exposePrefix}/anthropic/*`}
                        />
                      </div>
                      <div>
                        <Label className="text-xs">
                          Denied prefixes{' '}
                          <span className="text-muted-foreground font-normal">(one glob per line)</span>
                        </Label>
                        <textarea
                          rows={4}
                          value={deniedRaw}
                          onChange={e => setDeniedRaw(e.target.value)}
                          className="w-full mt-1 border rounded-md px-2 py-1.5 text-xs font-mono resize-none focus:outline-none focus:ring-1 focus:ring-blue-400"
                          placeholder={`${exposePrefix}/openai/gpt-4-*`}
                        />
                      </div>
                    </div>

                    <div className="rounded-md bg-blue-50 border border-blue-200 px-3 py-2 text-[11px] text-blue-800 space-y-0.5">
                      <p><strong>Evaluation order:</strong> denied prefixes are checked first (deny wins).</p>
                      <p>Pattern <code className="bg-white px-0.5 rounded">*</code> matches within one path segment.
                        Use <code className="bg-white px-0.5 rounded">{exposePrefix}/*</code> to allow all models.</p>
                      <p><strong>Rate limits still apply:</strong> RPM, TPM, daily and monthly budgets are enforced regardless of which virtual model is called.</p>
                    </div>
                  </>
                )}

                <div className="flex gap-2">
                  <Button
                    size="sm"
                    disabled={grantMut.isPending || !selectedProvider}
                    onClick={() => grantMut.mutate()}
                  >
                    {grantMut.isPending ? (
                      <><Loader2 className="w-3 h-3 animate-spin mr-1" />Granting…</>
                    ) : (
                      <><Globe className="w-3 h-3 mr-1" />Grant Access</>
                    )}
                  </Button>
                  <Button size="sm" variant="outline" onClick={() => {
                    setShowForm(false)
                    setSelectedProvider('')
                    setAllowedRaw('')
                    setDeniedRaw('')
                  }}>
                    Cancel
                  </Button>
                </div>
              </div>
            )}
          </CardContent>
        </Card>
      )}

      {/* Rate limit reminder */}
      {grants.length > 0 && (
        <div className="rounded-md border bg-white px-4 py-3 text-xs text-muted-foreground space-y-1">
          <p className="font-medium text-foreground">Rate limits apply to all virtual models equally</p>
          <p>
            All calls to virtual models from any granted provider consume the same project budget
            (RPM, TPM, daily tokens, monthly tokens). Configure limits in the{' '}
            <strong>Policy &amp; Quotas</strong> tab.
          </p>
        </div>
      )}
    </div>
  )
}

// ── Main detail page ──────────────────────────────────────────────────────────
export default function ProjectDetailPage() {
  const params  = useParams<{ id: string }>()
  const router  = useRouter()
  const id      = params.id
  const [activeTab, setActiveTab] = useState<'usage' | 'policy' | 'config' | 'runtimes' | 'providers' | 'models'>('usage')

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

      {/* Tabbed navigation — Usage | Policy | Config | Runtimes */}
      <div>
        <div className="flex gap-0 border-b mb-5">
          {([
            { key: 'usage',     label: 'Usage Analytics' },
            { key: 'policy',    label: 'Policy & Quotas' },
            { key: 'config',    label: 'Configuration' },
            { key: 'runtimes',  label: 'Runtimes & Queue' },
            { key: 'providers', label: 'Provider Access' },
            { key: 'models',    label: 'Model Access' },
          ] as const).map(t => (
            <button key={t.key}
              onClick={() => setActiveTab(t.key)}
              className={`px-4 py-2.5 text-sm font-medium border-b-2 transition-colors ${
                activeTab === t.key
                  ? 'border-blue-600 text-blue-600'
                  : 'border-transparent text-muted-foreground hover:text-foreground'
              }`}
            >
              {t.label}
            </button>
          ))}
        </div>

        {activeTab === 'usage' && <UsageAnalyticsPanel projectId={id} />}

        {activeTab === 'policy' && <PolicyPanel projectId={id} />}

        {activeTab === 'config' && (
          <div className="space-y-4">
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
            <Card>
              <CardHeader><CardTitle className="text-base">Metadata</CardTitle></CardHeader>
              <CardContent className="text-sm grid grid-cols-2 gap-2 text-muted-foreground">
                <div><span className="font-medium text-foreground">ID:</span> <span className="font-mono text-xs">{project.id}</span></div>
                <div><span className="font-medium text-foreground">Team:</span> {project.team_id || <span className="italic text-muted-foreground text-xs">No team (org-direct)</span>}</div>
                <div><span className="font-medium text-foreground">Org:</span> {project.organization_id}</div>
                <div><span className="font-medium text-foreground">Admission:</span> {project.admission_policy}</div>
                <div><span className="font-medium text-foreground">Created:</span> {new Date(project.created_at).toLocaleString()}</div>
                <div><span className="font-medium text-foreground">Updated:</span> {new Date(project.updated_at).toLocaleString()}</div>
              </CardContent>
            </Card>
          </div>
        )}

        {activeTab === 'runtimes' && (
          <div className="space-y-4">
            <Card>
              <CardHeader><CardTitle className="text-base flex items-center gap-2">
                <Server className="w-4 h-4" />Active Runtimes
              </CardTitle></CardHeader>
              <CardContent><RuntimesTable projectId={id} /></CardContent>
            </Card>
            <Card>
              <CardHeader><CardTitle className="text-base flex items-center gap-2">
                <Clock className="w-4 h-4" />Deployment Queue
              </CardTitle></CardHeader>
              <CardContent><QueuePanel projectId={id} /></CardContent>
            </Card>
            <Card>
              <CardHeader><CardTitle className="text-base flex items-center gap-2">
                <AlertTriangle className="w-4 h-4" />Preemption History
              </CardTitle></CardHeader>
              <CardContent><PreemptionHistory projectId={id} /></CardContent>
            </Card>
          </div>
        )}

        {activeTab === 'providers' && <ProviderAccessPanel projectId={id} />}
        {activeTab === 'models' && <ProjectModelAccessPanel projectId={id} teamId={project.team_id} />}
      </div>
    </div>
  )
}
