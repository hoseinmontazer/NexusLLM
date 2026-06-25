'use client'

// High Availability — cluster-wide replica status, recovery log, and per-model spec editor.

import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type ReplicaStatus, type RecoveryLogEntry, type PlacementPolicy } from '@/lib/api'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'
import { toast } from '@/components/ui/toaster'
import {
  Shield, RefreshCw, CheckCircle2, AlertTriangle, XCircle,
  ChevronDown, ChevronRight, Settings2, Clock, Activity,
} from 'lucide-react'

// ─── helpers ──────────────────────────────────────────────────────────────────

function HABadge({ status }: { status: string }) {
  if (status === 'healthy')
    return <span className="flex items-center gap-1 text-xs font-semibold text-green-700 bg-green-50 border border-green-200 rounded-full px-2 py-0.5"><CheckCircle2 className="w-3 h-3" />Healthy</span>
  if (status === 'degraded')
    return <span className="flex items-center gap-1 text-xs font-semibold text-yellow-700 bg-yellow-50 border border-yellow-200 rounded-full px-2 py-0.5"><AlertTriangle className="w-3 h-3" />Degraded</span>
  return <span className="flex items-center gap-1 text-xs font-semibold text-red-600 bg-red-50 border border-red-200 rounded-full px-2 py-0.5"><XCircle className="w-3 h-3" />Unavailable</span>
}

function RuntimeStateBadge({ state }: { state: string }) {
  const map: Record<string, string> = {
    active:        'bg-green-100 text-green-800',
    ready:         'bg-green-100 text-green-800',
    warm:          'bg-green-100 text-green-800',
    idle:          'bg-blue-100 text-blue-700',
    loading_model: 'bg-blue-100 text-blue-700',
    waiting_ready: 'bg-blue-100 text-blue-700',
    starting:      'bg-blue-100 text-blue-700',
    pending:       'bg-gray-100 text-gray-600',
    downloading:   'bg-purple-100 text-purple-700',
    validating:    'bg-purple-100 text-purple-700',
    recovering:    'bg-yellow-100 text-yellow-700',
    lost:          'bg-red-100 text-red-700',
    failed:        'bg-red-100 text-red-700',
    unhealthy:     'bg-red-100 text-red-700',
    stopping:      'bg-orange-100 text-orange-700',
    stopped:       'bg-gray-100 text-gray-500',
    archived:      'bg-gray-100 text-gray-400',
  }
  return (
    <span className={`text-xs px-1.5 py-0.5 rounded font-medium ${map[state] ?? 'bg-gray-100 text-gray-600'}`}>
      {state}
    </span>
  )
}

function ago(ts: string) {
  const ms = Date.now() - new Date(ts).getTime()
  const s = Math.round(ms / 1000)
  if (s < 60)   return `${s}s ago`
  const m = Math.round(s / 60)
  if (m < 60)   return `${m}m ago`
  const h = Math.round(m / 60)
  if (h < 24)   return `${h}h ago`
  return `${Math.round(h / 24)}d ago`
}

// ─── Replica spec editor ──────────────────────────────────────────────────────

function ReplicaSpecEditor({ modelId, modelName, current, onClose }: {
  modelId: string; modelName: string
  current: ReplicaStatus; onClose: () => void
}) {
  const qc = useQueryClient()
  const [form, setForm] = useState({
    desired_replicas: String(current.desired_replicas),
    min_available:    String(current.min_available),
    placement_policy: current.placement_policy as PlacementPolicy,
    auto_recover:     current.auto_recover,
  })

  const mut = useMutation({
    mutationFn: () => api.ha.setReplicaSpec(modelId, {
      desired_replicas: parseInt(form.desired_replicas) || 1,
      min_available:    parseInt(form.min_available)    || 1,
      placement_policy: form.placement_policy,
      auto_recover:     form.auto_recover,
    }),
    onSuccess: () => {
      toast({ title: 'Replica spec updated', description: modelName })
      qc.invalidateQueries({ queryKey: ['ha-status'] })
      onClose()
    },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-3">
        <div>
          <Label className="text-xs">Desired replicas</Label>
          <Input type="number" min={0} max={32} value={form.desired_replicas}
            onChange={e => setForm(p => ({ ...p, desired_replicas: e.target.value }))} className="mt-1" />
          <p className="text-xs text-muted-foreground mt-0.5">How many runtimes should run at all times</p>
        </div>
        <div>
          <Label className="text-xs">Min available</Label>
          <Input type="number" min={0} max={32} value={form.min_available}
            onChange={e => setForm(p => ({ ...p, min_available: e.target.value }))} className="mt-1" />
          <p className="text-xs text-muted-foreground mt-0.5">Alert threshold — below this = degraded</p>
        </div>
        <div>
          <Label className="text-xs">Placement policy</Label>
          <select
            className="w-full border rounded-md h-9 px-3 text-sm mt-1"
            value={form.placement_policy}
            onChange={e => setForm(p => ({ ...p, placement_policy: e.target.value as PlacementPolicy }))}>
            <option value="spread">spread — prefer different nodes (HA)</option>
            <option value="pack">pack — prefer same node (resource efficiency)</option>
            <option value="anti_affinity">anti_affinity — never two replicas on same node</option>
          </select>
        </div>
        <div className="flex items-center gap-3 mt-5">
          <label className="flex items-center gap-2 text-sm cursor-pointer">
            <input
              type="checkbox"
              checked={form.auto_recover}
              onChange={e => setForm(p => ({ ...p, auto_recover: e.target.checked }))}
              className="w-4 h-4 accent-blue-600"
            />
            <span>Auto-recover lost replicas</span>
          </label>
        </div>
      </div>

      <div className="flex gap-2 pt-2">
        <Button onClick={() => mut.mutate()} disabled={mut.isPending} className="flex-1">
          {mut.isPending ? 'Saving…' : 'Save Replica Spec'}
        </Button>
        <Button variant="outline" onClick={onClose}>Cancel</Button>
      </div>
    </div>
  )
}

// ─── Per-model replica detail row ─────────────────────────────────────────────

function ModelReplicaRow({ model }: { model: ReplicaStatus }) {
  const [expanded, setExpanded] = useState(false)
  const [editOpen, setEditOpen] = useState(false)

  const { data: detail } = useQuery({
    queryKey: ['ha-model', model.model_id],
    queryFn:  () => api.ha.getModelStatus(model.model_id),
    enabled:  expanded,
    refetchInterval: 10_000,
  })

  const replicas = detail?.replicas ?? []

  return (
    <div className="border rounded-lg bg-white">
      {/* Header row */}
      <div className="flex items-center justify-between p-3 gap-3">
        <div className="flex items-center gap-3 min-w-0">
          <Button variant="ghost" size="sm" className="p-0 h-6 w-6 shrink-0"
            onClick={() => setExpanded(e => !e)}>
            {expanded ? <ChevronDown className="w-4 h-4" /> : <ChevronRight className="w-4 h-4" />}
          </Button>
          <div className="min-w-0">
            <p className="font-semibold truncate">{model.model_name}</p>
            <p className="text-xs text-muted-foreground mt-0.5">
              {model.placement_policy} · {model.node_count} node{model.node_count !== 1 ? 's' : ''}
            </p>
          </div>
        </div>

        <div className="flex items-center gap-4 shrink-0">
          {/* Replica counts */}
          <div className="hidden sm:flex items-center gap-3 text-sm">
            <div className="text-center">
              <p className="font-semibold tabular-nums text-green-600">{model.active_replicas}</p>
              <p className="text-xs text-muted-foreground">active</p>
            </div>
            {model.starting_replicas > 0 && (
              <div className="text-center">
                <p className="font-semibold tabular-nums text-blue-500">{model.starting_replicas}</p>
                <p className="text-xs text-muted-foreground">starting</p>
              </div>
            )}
            {model.lost_replicas > 0 && (
              <div className="text-center">
                <p className="font-semibold tabular-nums text-red-500">{model.lost_replicas}</p>
                <p className="text-xs text-muted-foreground">lost</p>
              </div>
            )}
            <div className="text-center">
              <p className="font-semibold tabular-nums text-muted-foreground">{model.desired_replicas}</p>
              <p className="text-xs text-muted-foreground">desired</p>
            </div>
          </div>

          <HABadge status={model.ha_status} />

          <Dialog open={editOpen} onOpenChange={setEditOpen}>
            <DialogTrigger asChild>
              <Button size="sm" variant="outline">
                <Settings2 className="w-3.5 h-3.5 mr-1" />Configure
              </Button>
            </DialogTrigger>
            <DialogContent>
              <DialogHeader><DialogTitle>Replica Spec — {model.model_name}</DialogTitle></DialogHeader>
              <ReplicaSpecEditor
                modelId={model.model_id}
                modelName={model.model_name}
                current={model}
                onClose={() => setEditOpen(false)}
              />
            </DialogContent>
          </Dialog>
        </div>
      </div>

      {/* Expanded: replica list */}
      {expanded && (
        <div className="border-t px-3 pb-3 pt-2">
          {replicas.length === 0 ? (
            <p className="text-xs text-muted-foreground py-2">No runtimes found.</p>
          ) : (
            <table className="w-full text-xs">
              <thead>
                <tr className="text-muted-foreground border-b">
                  <th className="text-left pb-1.5 font-medium">Node</th>
                  <th className="text-left pb-1.5 font-medium">State</th>
                  <th className="text-left pb-1.5 font-medium">Updated</th>
                </tr>
              </thead>
              <tbody>
                {replicas.map((r: any) => (
                  <tr key={r.runtime_id} className="border-b last:border-0">
                    <td className="py-1.5 font-medium">{r.node_hostname}</td>
                    <td className="py-1.5"><RuntimeStateBadge state={r.state} /></td>
                    <td className="py-1.5 text-muted-foreground">{ago(r.updated_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
    </div>
  )
}

// ─── Recovery log ──────────────────────────────────────────────────────────────

function RecoveryLog() {
  const { data, isLoading } = useQuery({
    queryKey: ['recovery-log'],
    queryFn:  () => api.ha.getRecoveryLog({ limit: 50 }),
    refetchInterval: 15_000,
  })
  const entries: RecoveryLogEntry[] = data?.data ?? []

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center gap-2">
          <Clock className="w-4 h-4 text-muted-foreground" />
          <CardTitle className="text-base">Recovery Log</CardTitle>
          <span className="text-xs text-muted-foreground ml-auto">last 50 events</span>
        </div>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <p className="text-sm text-muted-foreground">Loading…</p>
        ) : entries.length === 0 ? (
          <p className="text-sm text-muted-foreground text-center py-6">No recovery events — cluster is healthy.</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-xs">
              <thead>
                <tr className="border-b text-muted-foreground">
                  <th className="text-left pb-2 font-medium">When</th>
                  <th className="text-left pb-2 font-medium">Model</th>
                  <th className="text-left pb-2 font-medium">Trigger</th>
                  <th className="text-left pb-2 font-medium">Result</th>
                </tr>
              </thead>
              <tbody>
                {entries.map(e => (
                  <tr key={e.id} className="border-b last:border-0">
                    <td className="py-1.5 text-muted-foreground whitespace-nowrap">{ago(e.created_at)}</td>
                    <td className="py-1.5 font-medium">{e.model_name}</td>
                    <td className="py-1.5">
                      <span className="bg-gray-100 text-gray-700 rounded px-1.5">{e.trigger}</span>
                    </td>
                    <td className="py-1.5">
                      <span className={`rounded px-1.5 font-medium ${
                        e.status === 'success'  ? 'bg-green-100 text-green-700' :
                        e.status === 'failed'   ? 'bg-red-100 text-red-700' :
                        e.status === 'skipped'  ? 'bg-gray-100 text-gray-500' :
                        'bg-blue-100 text-blue-700'
                      }`}>{e.status}</span>
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

export default function HAPage() {
  const qc = useQueryClient()
  const { data, isLoading } = useQuery({
    queryKey: ['ha-status'],
    queryFn:  api.ha.getClusterStatus,
    refetchInterval: 10_000,
  })

  const models: ReplicaStatus[]  = data?.models ?? []
  const healthy   = data?.healthy    ?? 0
  const degraded  = data?.degraded   ?? 0
  const unavail   = data?.unavailable ?? 0

  return (
    <div className="space-y-6">
      {/* Page header */}
      <div className="flex items-center justify-between flex-wrap gap-2">
        <div className="flex items-center gap-3">
          <Shield className="w-7 h-7 text-blue-600" />
          <div>
            <h1 className="text-2xl font-bold">High Availability</h1>
            <p className="text-sm text-muted-foreground">Replica management · Failover · Recovery</p>
          </div>
        </div>
        <Button variant="outline" size="sm" onClick={() => qc.invalidateQueries({ queryKey: ['ha-status'] })}>
          <RefreshCw className="w-3.5 h-3.5 mr-1" />Refresh
        </Button>
      </div>

      {/* Summary stats */}
      <div className="grid grid-cols-3 gap-4">
        <Card className="border-green-200 bg-green-50/50">
          <CardContent className="pt-4 pb-4 text-center">
            <p className="text-3xl font-bold text-green-700 tabular-nums">{healthy}</p>
            <p className="text-sm text-green-600 flex items-center justify-center gap-1 mt-1">
              <CheckCircle2 className="w-4 h-4" />Healthy models
            </p>
          </CardContent>
        </Card>
        <Card className="border-yellow-200 bg-yellow-50/50">
          <CardContent className="pt-4 pb-4 text-center">
            <p className="text-3xl font-bold text-yellow-700 tabular-nums">{degraded}</p>
            <p className="text-sm text-yellow-600 flex items-center justify-center gap-1 mt-1">
              <AlertTriangle className="w-4 h-4" />Degraded models
            </p>
          </CardContent>
        </Card>
        <Card className="border-red-200 bg-red-50/50">
          <CardContent className="pt-4 pb-4 text-center">
            <p className="text-3xl font-bold text-red-600 tabular-nums">{unavail}</p>
            <p className="text-sm text-red-500 flex items-center justify-center gap-1 mt-1">
              <XCircle className="w-4 h-4" />Unavailable models
            </p>
          </CardContent>
        </Card>
      </div>

      {/* Reconciler status */}
      {data && (
        <div className="text-xs text-muted-foreground flex items-center gap-4 px-1">
          <span className="flex items-center gap-1">
            <Activity className="w-3.5 h-3.5" />
            Last reconciler sweep: {data.reconciler_last_sweep
              ? new Date(data.reconciler_last_sweep).toLocaleString()
              : 'never'}
          </span>
          <span>Total recoveries triggered: {data.recoveries_triggered}</span>
        </div>
      )}

      {/* Model list */}
      <div className="space-y-3">
        {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}

        {!isLoading && models.length === 0 && (
          <Card>
            <CardContent className="py-12 text-center text-muted-foreground space-y-2">
              <Shield className="w-8 h-8 mx-auto opacity-30" />
              <p className="font-medium">No models with HA specs configured.</p>
              <p className="text-sm">Deploy a model and configure its replica count here.</p>
            </CardContent>
          </Card>
        )}

        {/* Unavailable first, then degraded, then healthy */}
        {['unavailable', 'degraded', 'healthy'].map(status =>
          models
            .filter(m => m.ha_status === status)
            .map(m => <ModelReplicaRow key={m.model_id} model={m} />)
        )}
      </div>

      {/* Recovery log */}
      <RecoveryLog />
    </div>
  )
}
