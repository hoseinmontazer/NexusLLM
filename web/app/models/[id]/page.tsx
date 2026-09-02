'use client'

// Model detail page — Runtime status · HA Replicas · Lazy config
// Accessed from /models → click a model row.

import { useState } from 'react'
import { useParams, useRouter } from 'next/navigation'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type ReplicaStatus, type RecoveryLogEntry, type PlacementPolicy, type LazyConfig, type Model } from '@/lib/api'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { toast } from '@/components/ui/toaster'
import {
  ArrowLeft, Activity, ShieldCheck, ShieldAlert, ShieldX,
  Settings, RefreshCw, Server, AlertTriangle, Clock, FileCode2,
  Brain, Zap, Globe, KeyRound, Eye, EyeOff
} from 'lucide-react'
// ─────────────────────────────────────────────────────────────────────────────
// HA helpers
// ─────────────────────────────────────────────────────────────────────────────
function HABadge({ status }: { status: string }) {
  const map = {
    healthy:     { cls: 'bg-green-100 text-green-800',  icon: ShieldCheck },
    starting:    { cls: 'bg-blue-100 text-blue-800',    icon: ShieldCheck },
    degraded:    { cls: 'bg-yellow-100 text-yellow-800', icon: ShieldAlert },
    unavailable: { cls: 'bg-red-100 text-red-800',       icon: ShieldX },
  }
  const s = map[status as keyof typeof map] ?? map.unavailable
  return (
    <span className={`inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs font-semibold ${s.cls}`}>
      <s.icon className="w-3 h-3" />{status}
    </span>
  )
}

function ReplicaBar({ s }: { s: ReplicaStatus }) {
  return (
    <div className="flex items-center gap-2 text-xs">
      <div className="flex gap-0.5">
        {Array.from({ length: s.desired_replicas }).map((_, i) => {
          const active = i < s.active_replicas
          const starting = !active && i < s.active_replicas + s.starting_replicas
          const lost = !active && !starting && i < s.active_replicas + s.starting_replicas + s.lost_replicas
          return (
            <div key={i} title={active ? 'active' : starting ? 'starting' : lost ? 'lost' : 'missing'}
              className={`w-3 h-3 rounded-sm ${active ? 'bg-green-500' : starting ? 'bg-blue-400' : lost ? 'bg-red-400' : 'bg-gray-200'}`} />
          )
        })}
      </div>
      <span className="text-muted-foreground">
        {s.active_replicas}/{s.desired_replicas}
        {s.starting_replicas > 0 && <span className="text-blue-600 ml-1">+{s.starting_replicas}</span>}
        {s.lost_replicas > 0 && <span className="text-red-600 ml-1">·{s.lost_replicas} lost</span>}
      </span>
    </div>
  )
}

// ─────────────────────────────────────────────────────────────────────────────
// HA TAB
// ─────────────────────────────────────────────────────────────────────────────
function HATab({ modelId }: { modelId: string }) {
  const qc = useQueryClient()
  const [showConfig, setShowConfig] = useState(false)

  const { data, isLoading } = useQuery({
    queryKey: ['ha-model', modelId],
    queryFn: () => api.ha.getModelStatus(modelId),
    refetchInterval: 10_000,
  })
  const { data: logData } = useQuery({
    queryKey: ['ha-recovery-log', modelId],
    queryFn: () => api.ha.getModelRecoveryLog(modelId, { limit: 20 }),
    refetchInterval: 30_000,
  })

  const status = data?.status
  const replicas = data?.replicas ?? []
  const log: RecoveryLogEntry[] = logData?.data ?? []

  const stateColor = (s: string) => ({
    ready: 'bg-green-100 text-green-800', active: 'bg-green-100 text-green-800',
    warm: 'bg-green-100 text-green-800', idle: 'bg-yellow-100 text-yellow-800',
    loading_model: 'bg-blue-100 text-blue-800', starting: 'bg-blue-100 text-blue-800',
    recovering: 'bg-blue-100 text-blue-800', lost: 'bg-red-100 text-red-800',
    failed: 'bg-red-100 text-red-800',
  }[s] ?? 'bg-gray-100 text-gray-600')

  const triggerCls = (t: string) => ({
    node_offline: 'bg-red-50 text-red-700', health_fail: 'bg-orange-50 text-orange-700',
    reconcile: 'bg-blue-50 text-blue-700', manual: 'bg-purple-50 text-purple-700',
  }[t] ?? 'bg-gray-50 text-gray-600')

  if (isLoading) return <p className="text-sm text-muted-foreground py-4">Loading HA status…</p>

  return (
    <div className="space-y-5">
      {/* Status summary */}
      {status && (
        <div className="flex items-center gap-4 p-4 rounded-lg border bg-gray-50">
          <HABadge status={status.ha_status} />
          <div className="flex-1">
            <ReplicaBar s={status} />
          </div>
          <div className="text-xs text-muted-foreground">
            {status.node_count} node{status.node_count !== 1 ? 's' : ''}
            · policy: <span className="font-medium">{status.placement_policy}</span>
          </div>
          <Button size="sm" variant="outline" onClick={() => setShowConfig(true)}>
            <Settings className="w-3.5 h-3.5 mr-1" />Configure
          </Button>
          <Button size="sm" variant="ghost" onClick={() => qc.invalidateQueries({ queryKey: ['ha-model', modelId] })}>
            <RefreshCw className="w-3.5 h-3.5" />
          </Button>
        </div>
      )}

      {/* Live replica instances */}
      {replicas.length > 0 && (
        <div>
          <p className="text-xs font-medium text-muted-foreground mb-2">REPLICA INSTANCES</p>
          <div className="space-y-2">
            {replicas.map((r: any) => (
              <div key={r.runtime_id} className="flex items-center gap-3 p-3 rounded-lg border bg-white text-sm">
                <Server className="w-4 h-4 text-muted-foreground shrink-0" />
                <div className="flex-1 min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="font-medium">{r.node_hostname}</span>
                    <span className={`px-1.5 py-0.5 rounded-full text-xs font-medium ${stateColor(r.state)}`}>{r.state}</span>
                  </div>
                  <p className="text-xs text-muted-foreground mt-0.5 font-mono">{r.bind_host}:{r.bind_port}</p>
                </div>
                <span className="text-xs text-muted-foreground font-mono">{r.runtime_id.slice(0, 8)}…</span>
              </div>
            ))}
          </div>
        </div>
      )}

      {!status && !isLoading && (
        <div className="py-6 text-center text-muted-foreground">
          <ShieldCheck className="w-8 h-8 mx-auto mb-2 opacity-30" />
          <p className="text-sm">No replica spec configured yet.</p>
          <Button size="sm" className="mt-3" onClick={() => setShowConfig(true)}>
            <Settings className="w-3.5 h-3.5 mr-1" />Set up HA replicas
          </Button>
        </div>
      )}

      {/* Recovery log */}
      {log.length > 0 && (
        <div>
          <p className="text-xs font-medium text-muted-foreground mb-2">RECOVERY LOG</p>
          <table className="w-full text-xs">
            <thead><tr className="border-b text-muted-foreground">
              <th className="text-left pb-2">Trigger</th>
              <th className="text-left pb-2">Status</th>
              <th className="text-left pb-2">Reason</th>
              <th className="text-left pb-2">Time</th>
            </tr></thead>
            <tbody>
              {log.map(e => (
                <tr key={e.id} className="border-b last:border-0">
                  <td className="py-2"><span className={`px-2 py-0.5 rounded text-xs ${triggerCls(e.trigger)}`}>{e.trigger}</span></td>
                  <td className="py-2">
                    <span className={`px-2 py-0.5 rounded-full text-xs font-medium ${
                      e.status === 'success' ? 'bg-green-100 text-green-800' : e.status === 'failed' ? 'bg-red-100 text-red-800' : 'bg-blue-100 text-blue-800'
                    }`}>{e.status}</span>
                  </td>
                  <td className="py-2 text-muted-foreground max-w-xs truncate" title={e.reason}>{e.reason}</td>
                  <td className="py-2 text-muted-foreground whitespace-nowrap">{new Date(e.created_at).toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* Configure dialog */}
      <Dialog open={showConfig} onOpenChange={setShowConfig}>
        <HAConfigDialog modelId={modelId} current={status ?? null} onClose={() => { setShowConfig(false); qc.invalidateQueries({ queryKey: ['ha-model', modelId] }) }} />
      </Dialog>
    </div>
  )
}

function HAConfigDialog({ modelId, current, onClose }: { modelId: string; current: ReplicaStatus | null; onClose: () => void }) {
  const [desired, setDesired] = useState(String(current?.desired_replicas ?? 1))
  const [minAvail, setMinAvail] = useState(String(current?.min_available ?? 1))
  const [policy, setPolicy] = useState<PlacementPolicy>((current?.placement_policy as PlacementPolicy) ?? 'spread')
  const [autoRecover, setAutoRecover] = useState(current?.auto_recover ?? true)
  const [delay, setDelay] = useState('30')

  const mut = useMutation({
    mutationFn: () => api.ha.setReplicaSpec(modelId, {
      desired_replicas: parseInt(desired) || 1,
      min_available:    parseInt(minAvail) || 1,
      placement_policy: policy,
      auto_recover:     autoRecover,
      recovery_delay_s: parseInt(delay) || 30,
    }),
    onSuccess: () => { toast({ title: 'Replica spec saved' }); onClose() },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  return (
    <DialogContent>
      <DialogHeader><DialogTitle>HA Replica Configuration</DialogTitle></DialogHeader>
      <div className="space-y-4">
        <div className="grid grid-cols-2 gap-3">
          <div>
            <Label className="text-xs">Desired replicas</Label>
            <Input type="number" min={0} max={32} value={desired} onChange={e => setDesired(e.target.value)} className="mt-1" />
          </div>
          <div>
            <Label className="text-xs">Min available (SLA floor)</Label>
            <Input type="number" min={0} max={32} value={minAvail} onChange={e => setMinAvail(e.target.value)} className="mt-1" />
          </div>
        </div>
        <div>
          <Label className="text-xs">Placement policy</Label>
          <select className="w-full border rounded-md h-9 px-3 text-sm mt-1" value={policy}
            onChange={e => setPolicy(e.target.value as PlacementPolicy)}>
            <option value="spread">Spread — prefer different nodes (best for HA)</option>
            <option value="pack">Pack — prefer same node (saves resources)</option>
            <option value="anti_affinity">Anti-affinity — never two replicas on same node</option>
          </select>
        </div>
        <div>
          <Label className="text-xs">Recovery delay (seconds)</Label>
          <Input type="number" min={0} max={300} value={delay} onChange={e => setDelay(e.target.value)} className="mt-1 w-32" />
        </div>
        <label className="flex items-center gap-2 cursor-pointer text-sm">
          <input type="checkbox" className="w-4 h-4" checked={autoRecover} onChange={e => setAutoRecover(e.target.checked)} />
          <span>Auto-recover lost replicas</span>
        </label>
        <Button onClick={() => mut.mutate()} disabled={mut.isPending} className="w-full">
          {mut.isPending ? 'Saving…' : 'Save'}
        </Button>
      </div>
    </DialogContent>
  )
}

// ─────────────────────────────────────────────────────────────────────────────
// PLACEMENT TAB — change where a deployed model runs
// ─────────────────────────────────────────────────────────────────────────────
function PlacementTab({ modelId }: { modelId: string }) {
  const qc = useQueryClient()

  const { data: cfg, isLoading: cfgLoading } = useQuery({
    queryKey: ['model-lazy-config', modelId],
    queryFn: () => api.models.getLazyConfig(modelId),
  })
  const { data: nodesData } = useQuery({
    queryKey: ['nodes'],
    queryFn: api.nodes.list,
  })
  const nodes = nodesData?.data ?? []

  const [mode, setMode] = useState<'auto' | 'specific_node' | 'node_group' | 'label_selector'>('auto')
  const [nodeId, setNodeId] = useState('')
  const [nodeGroupId, setNodeGroupId] = useState('')
  const [labelPairs, setLabelPairs] = useState<{k: string; v: string}[]>([{ k: '', v: '' }])
  const [execMode, setExecMode] = useState('auto')
  const [ngpuLayers, setNgpuLayers] = useState('-1')
  const [initialized, setInitialized] = useState(false)

  // Initialize form from existing config once loaded
  if (cfg && !initialized) {
    setInitialized(true)
    setExecMode(cfg.execution_mode ?? 'auto')
    setNgpuLayers(String(cfg.n_gpu_layers ?? -1))
    if (cfg.node_id) {
      setMode('specific_node')
      setNodeId(cfg.node_id)
    }
  }

  const save = useMutation({
    mutationFn: () => {
      const nodeSelector: Record<string, string> = {}
      if (mode === 'label_selector') {
        labelPairs.forEach(p => { if (p.k && p.v) nodeSelector[p.k] = p.v })
      }
      return api.models.setLazyConfig(modelId, {
        n_gpu_layers:   parseInt(ngpuLayers) ?? -1,
        idle_timeout_secs: cfg?.idle_timeout_secs ?? undefined,
        ctx_size:       cfg?.ctx_size ?? 4096,
        node_id:        mode === 'specific_node' ? nodeId : undefined,
        execution_mode: execMode,
      })
    },
    onSuccess: () => {
      toast({ title: 'Placement updated', description: 'Takes effect on next restart' })
      qc.invalidateQueries({ queryKey: ['model-lazy-config', modelId] })
    },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  if (cfgLoading) return <p className="text-sm text-muted-foreground py-4">Loading…</p>

  return (
    <div className="space-y-5">
      <div className="p-3 bg-blue-50 border border-blue-100 rounded text-xs text-blue-700">
        Changes take effect on the next container start. Use <strong>Restart</strong> on the Endpoints tab to apply immediately.
      </div>

      {/* Placement mode */}
      <div>
        <p className="text-xs font-semibold text-muted-foreground uppercase mb-2">Placement Mode</p>
        <div className="grid grid-cols-2 gap-2">
          {([
            { v: 'auto',           label: 'Automatic',      desc: 'Scheduler picks best node' },
            { v: 'specific_node',  label: 'Specific Node',  desc: 'Pin to a named node' },
            { v: 'node_group',     label: 'Node Group',     desc: 'Any node in a group' },
            { v: 'label_selector', label: 'Label Selector', desc: 'Match by labels' },
          ] as const).map(opt => (
            <label key={opt.v}
              className={`flex items-start gap-2 border rounded-md p-2.5 cursor-pointer ${
                mode === opt.v ? 'border-blue-500 bg-blue-50' : 'hover:bg-gray-50'
              }`}>
              <input type="radio" name="pm" value={opt.v} checked={mode === opt.v}
                onChange={() => setMode(opt.v)} className="mt-0.5" />
              <div>
                <p className="text-xs font-medium">{opt.label}</p>
                <p className="text-xs text-muted-foreground">{opt.desc}</p>
              </div>
            </label>
          ))}
        </div>

        <div className="mt-3">
          {mode === 'specific_node' && (
            <div>
              <Label className="text-xs">Target node</Label>
              <select className="w-full border rounded-md h-9 px-3 text-sm mt-1"
                value={nodeId} onChange={e => setNodeId(e.target.value)}>
                <option value="">— select node —</option>
                {nodes.map(n => (
                  <option key={n.id} value={n.id}>
                    {n.hostname || n.id.slice(0, 8)}
                    {' '}({n.status}{n.cordoned ? ', cordoned' : ''}
                    {(n.total_vram_mb ?? 0) > 0 ? `, ${Math.round((n.total_vram_mb ?? 0)/1024)}GB VRAM` : ''})
                  </option>
                ))}
              </select>
              {nodeId && (() => {
                const n = nodes.find(x => x.id === nodeId)
                if (!n) return null
                return (
                  <div className="mt-2 p-2 bg-gray-50 rounded text-xs text-muted-foreground flex gap-3">
                    <span>{n.total_cpu} CPUs</span>
                    <span>{Math.round(n.total_ram_mb / 1024)}GB RAM</span>
                    {(n.total_vram_mb ?? 0) > 0 && <span>{Math.round((n.total_vram_mb ?? 0)/1024)}GB VRAM</span>}
                    <span className={n.status === 'online' ? 'text-green-600' : 'text-red-500'}>{n.status}</span>
                  </div>
                )
              })()}
            </div>
          )}
          {mode === 'node_group' && (
            <div>
              <Label className="text-xs">Group ID</Label>
              <Input value={nodeGroupId} onChange={e => setNodeGroupId(e.target.value)}
                placeholder="h200-cluster" className="mt-1" />
              <p className="text-xs text-muted-foreground mt-1">
                Nodes need label <code className="bg-gray-100 px-1 rounded">node_group=&lt;id&gt;</code>
              </p>
            </div>
          )}
          {mode === 'label_selector' && (
            <div className="space-y-2">
              <Label className="text-xs">Label selector (ALL must match)</Label>
              {labelPairs.map((pair, i) => (
                <div key={i} className="flex gap-2">
                  <Input value={pair.k} placeholder="key" className="text-sm"
                    onChange={e => setLabelPairs(p => p.map((x, idx) => idx === i ? { ...x, k: e.target.value } : x))} />
                  <span className="self-center text-muted-foreground">=</span>
                  <Input value={pair.v} placeholder="value" className="text-sm"
                    onChange={e => setLabelPairs(p => p.map((x, idx) => idx === i ? { ...x, v: e.target.value } : x))} />
                  <Button type="button" variant="ghost" size="sm" className="text-red-400 px-2"
                    onClick={() => setLabelPairs(p => p.filter((_, idx) => idx !== i))}>×</Button>
                </div>
              ))}
              <Button type="button" variant="outline" size="sm"
                onClick={() => setLabelPairs(p => [...p, { k: '', v: '' }])}>+ Add label</Button>
            </div>
          )}
        </div>
      </div>

      {/* Execution mode */}
      <div>
        <p className="text-xs font-semibold text-muted-foreground uppercase mb-2">Execution Mode</p>
        <div className="grid grid-cols-3 gap-2">
          {([
            { v: 'auto', label: 'Auto', desc: 'Use GPU if available' },
            { v: 'gpu',  label: 'GPU',  desc: 'Require GPU' },
            { v: 'cpu',  label: 'CPU',  desc: 'CPU only' },
          ] as const).map(opt => (
            <label key={opt.v}
              className={`flex items-start gap-2 border rounded-md p-2.5 cursor-pointer ${
                execMode === opt.v ? 'border-blue-500 bg-blue-50' : 'hover:bg-gray-50'
              }`}>
              <input type="radio" name="em" value={opt.v} checked={execMode === opt.v}
                onChange={() => setExecMode(opt.v)} className="mt-0.5" />
              <div>
                <p className="text-xs font-medium">{opt.label}</p>
                <p className="text-xs text-muted-foreground">{opt.desc}</p>
              </div>
            </label>
          ))}
        </div>
        <div className="mt-3 w-40">
          <Label className="text-xs">GPU layers (-1 = all)</Label>
          <Input type="number" value={ngpuLayers}
            onChange={e => setNgpuLayers(e.target.value)} className="mt-1" />
        </div>
      </div>

      <Button onClick={() => save.mutate()} disabled={save.isPending} className="w-full">
        {save.isPending ? 'Saving…' : 'Save Placement'}
      </Button>
    </div>
  )
}
// ─────────────────────────────────────────────────────────────────────────────
// LAZY CONFIG TAB — model source, runtime tuning, idle behaviour
// ─────────────────────────────────────────────────────────────────────────────
function LazyConfigTab({ modelId }: { modelId: string }) {
  const qc = useQueryClient()

  const { data: cfg, isLoading } = useQuery({
    queryKey: ['model-lazy-config', modelId],
    queryFn: () => api.models.getLazyConfig(modelId),
  })

  // Local form state — initialised once cfg loads
  const [initialized, setInitialized] = useState(false)
  const [ggufPath,    setGgufPath]    = useState('')
  const [hfRepo,      setHfRepo]      = useState('')
  const [hfFile,      setHfFile]      = useState('')
  const [hfToken,     setHfToken]     = useState('')
  const [ctxSize,     setCtxSize]     = useState('4096')
  const [ngpuLayers,  setNgpuLayers]  = useState('-1')
  const [cpuThreads,  setCpuThreads]  = useState('')
  const [memLimit,    setMemLimit]    = useState('')
  const [volume,      setVolume]      = useState('')
  const [idleTimeout, setIdleTimeout] = useState('')
  const [extraArgs,   setExtraArgs]   = useState('')  // space-separated string

  if (cfg && !initialized) {
    setInitialized(true)
    setGgufPath(cfg.gguf_path    ?? '')
    setHfRepo(cfg.hf_repo        ?? '')
    setHfFile(cfg.hf_file        ?? '')
    setCtxSize(String(cfg.ctx_size    ?? 4096))
    setNgpuLayers(String(cfg.n_gpu_layers ?? -1))
    setCpuThreads(cfg.cpu_threads != null ? String(cfg.cpu_threads) : '')
    setMemLimit(cfg.memory_limit  ?? '')
    setVolume(cfg.models_volume   ?? '')
    setIdleTimeout(cfg.idle_timeout_secs != null ? String(cfg.idle_timeout_secs) : '')
    // extra_args may be null, a raw JSON string, an actual array, or an empty
    // object '{}' (from pre-026 rows that got DEFAULT '{}' instead of '[]').
    const rawArgs = cfg.extra_args
    if (Array.isArray(rawArgs)) {
      setExtraArgs(rawArgs.join(' '))
    } else if (typeof rawArgs === 'string') {
      try { setExtraArgs((JSON.parse(rawArgs) as string[]).join(' ')) } catch { setExtraArgs('') }
    } else {
      // null, undefined, or {} — treat as empty
      setExtraArgs('')
    }
  }

  const save = useMutation({
    mutationFn: () => api.models.setLazyConfig(modelId, {
      gguf_path:         ggufPath    || undefined,
      hf_repo:           hfRepo      || undefined,
      hf_file:           hfFile      || undefined,
      hf_token:          hfToken     || undefined,
      ctx_size:          parseInt(ctxSize)    || 4096,
      n_gpu_layers:      parseInt(ngpuLayers) ?? -1,
      cpu_threads:       cpuThreads ? parseInt(cpuThreads) : undefined,
      memory_limit:      memLimit    || undefined,
      models_volume:     volume      || undefined,
      idle_timeout_secs: idleTimeout ? parseInt(idleTimeout) : undefined,
      extra_args:        extraArgs.trim() ? extraArgs.trim().split(/\s+/) : [],
    } as Partial<LazyConfig>),
    onSuccess: () => {
      toast({ title: 'Lazy config saved', description: 'Takes effect on next container start' })
      qc.invalidateQueries({ queryKey: ['model-lazy-config', modelId] })
    },
    onError: (e: any) => toast({ title: 'Save failed', description: e.message, variant: 'destructive' }),
  })

  if (isLoading) return <p className="text-sm text-muted-foreground py-4">Loading…</p>

  const hasConfig = cfg && (cfg.gguf_path || cfg.hf_repo)

  return (
    <div className="space-y-5">
      {/* Status banner */}
      {hasConfig ? (
        <div className="flex items-start gap-3 p-3 rounded-lg border bg-green-50 border-green-100">
          <FileCode2 className="w-4 h-4 text-green-600 mt-0.5 shrink-0" />
          <div className="text-xs text-green-800">
            <p className="font-semibold mb-0.5">Lazy config active</p>
            {cfg.gguf_path
              ? <p className="font-mono">{cfg.gguf_path}</p>
              : <p className="font-mono">{cfg.hf_repo} / {cfg.hf_file}</p>
            }
          </div>
        </div>
      ) : (
        <div className="p-3 bg-yellow-50 border border-yellow-100 rounded text-xs text-yellow-800">
          No lazy config set. The model will not be started automatically on demand until a model source is configured.
        </div>
      )}

      {/* Model source */}
      <div>
        <p className="text-xs font-semibold text-muted-foreground uppercase mb-3">Model Source</p>
        <p className="text-xs text-muted-foreground mb-3">
          Local GGUF path takes precedence. If empty, HF repo + file are used. Set HF token for gated repos.
        </p>
        <div className="space-y-3">
          <div>
            <Label className="text-xs">Local GGUF path</Label>
            <Input value={ggufPath} onChange={e => setGgufPath(e.target.value)}
              placeholder="/models/gemma-2-2b-it-Q4_K_M.gguf"
              className="mt-1 font-mono text-xs" />
            <p className="text-xs text-muted-foreground mt-0.5">Container path inside the models volume</p>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <Label className="text-xs">HuggingFace repo</Label>
              <Input value={hfRepo} onChange={e => setHfRepo(e.target.value)}
                placeholder="bartowski/gemma-2-2b-it-GGUF"
                className="mt-1 font-mono text-xs" />
            </div>
            <div>
              <Label className="text-xs">GGUF file</Label>
              <Input value={hfFile} onChange={e => setHfFile(e.target.value)}
                placeholder="gemma-2-2b-it-Q4_K_M.gguf"
                className="mt-1 font-mono text-xs" />
            </div>
          </div>
          <div>
            <Label className="text-xs">HF token <span className="font-normal text-muted-foreground">(gated repos)</span></Label>
            <Input type="password" value={hfToken} onChange={e => setHfToken(e.target.value)}
              placeholder="hf_…" className="mt-1" />
          </div>
        </div>
      </div>

      {/* Runtime tuning */}
      <div>
        <p className="text-xs font-semibold text-muted-foreground uppercase mb-3">Runtime Tuning</p>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <Label className="text-xs">Context size</Label>
            <Input type="number" value={ctxSize} onChange={e => setCtxSize(e.target.value)}
              className="mt-1" />
          </div>
          <div>
            <Label className="text-xs">GPU layers <span className="font-normal text-muted-foreground">(-1 = all)</span></Label>
            <Input type="number" value={ngpuLayers} onChange={e => setNgpuLayers(e.target.value)}
              className="mt-1" />
          </div>
          <div>
            <Label className="text-xs">CPU threads <span className="font-normal text-muted-foreground">(0 = auto)</span></Label>
            <Input type="number" value={cpuThreads} onChange={e => setCpuThreads(e.target.value)}
              placeholder="auto" className="mt-1" />
          </div>
          <div>
            <Label className="text-xs">Memory limit</Label>
            <Input value={memLimit} onChange={e => setMemLimit(e.target.value)}
              placeholder="8g" className="mt-1" />
            <p className="text-xs text-muted-foreground mt-0.5">Docker --memory (e.g. 8g, 16384m)</p>
          </div>
        </div>
      </div>

      {/* Volume & idle */}
      <div>
        <p className="text-xs font-semibold text-muted-foreground uppercase mb-3">Storage & Idle Policy</p>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <Label className="text-xs">Models volume</Label>
            <Input value={volume} onChange={e => setVolume(e.target.value)}
              placeholder="llamacpp_models" className="mt-1 font-mono text-xs" />
            <p className="text-xs text-muted-foreground mt-0.5">Named volume or absolute host path</p>
          </div>
          <div>
            <Label className="text-xs">Idle timeout — <span className="font-semibold text-orange-600">in seconds</span></Label>
            <Input type="number" value={idleTimeout} onChange={e => setIdleTimeout(e.target.value)}
              placeholder="0 = cluster default" className="mt-1" />
            <p className="text-xs text-muted-foreground mt-0.5">
              Seconds of inactivity before container stops. 0 = cluster default (15 min).
              Examples: <span className="font-mono">600</span> = 10 min, <span className="font-mono">3600</span> = 1 hr.
            </p>
          </div>
        </div>
      </div>

      {/* Extra args */}
      <div>
        <p className="text-xs font-semibold text-muted-foreground uppercase mb-3">Extra Arguments</p>
        <Label className="text-xs">Additional flags <span className="font-normal text-muted-foreground">(space-separated)</span></Label>
        <Input value={extraArgs} onChange={e => setExtraArgs(e.target.value)}
          placeholder="-thk 0 --no-warmup"
          className="mt-1 font-mono text-xs" />
        <p className="text-xs text-muted-foreground mt-0.5">
          Appended to the backend command after all structured flags
        </p>
      </div>

      <Button onClick={() => save.mutate()} disabled={save.isPending} className="w-full">
        {save.isPending ? 'Saving…' : 'Save Lazy Config'}
      </Button>

      {/* Extra args hint */}
      <div className="p-3 bg-gray-50 border rounded text-xs text-muted-foreground space-y-1">
        <p className="font-medium text-gray-700">Extra args examples</p>
        <p><code className="bg-gray-100 px-1 rounded">-thk 0</code> — disable Qwen3 thinking mode (fixes empty responses)</p>
        <p><code className="bg-gray-100 px-1 rounded">--no-warmup</code> — skip warmup inference on startup (faster start)</p>
        <p><code className="bg-gray-100 px-1 rounded">--rope-scale 2</code> — extend context via RoPE scaling</p>
      </div>
    </div>
  )
}

// ─────────────────────────────────────────────────────────────────────────────
// PRICING TAB — $-per-token rate used to compute usage.cost on responses
// ─────────────────────────────────────────────────────────────────────────────
function PricingTab({ modelId }: { modelId: string }) {
  const qc = useQueryClient()

  const { data: pricing, isLoading, isError } = useQuery({
    queryKey: ['model-pricing', modelId],
    queryFn: () => api.models.getPricing(modelId),
    retry: false,
  })

  const [initialized, setInitialized]   = useState(false)
  const [inputRate, setInputRate]       = useState('0')
  const [outputRate, setOutputRate]     = useState('0')
  const [cachedRate, setCachedRate]     = useState('0')
  const [currency, setCurrency]         = useState('USD')

  if (pricing && !initialized) {
    setInitialized(true)
    setInputRate(pricing.input_per_token)
    setOutputRate(pricing.output_per_token)
    setCachedRate(pricing.cached_per_token)
    setCurrency(pricing.currency)
  }

  const save = useMutation({
    mutationFn: () => api.models.setPricing(modelId, {
      input_per_token: parseFloat(inputRate) || 0,
      output_per_token: parseFloat(outputRate) || 0,
      cached_per_token: parseFloat(cachedRate) || 0,
      currency,
    }),
    onSuccess: () => {
      toast({ title: 'Pricing saved', description: 'usage.cost will now appear on this model’s responses' })
      qc.invalidateQueries({ queryKey: ['model-pricing', modelId] })
    },
    onError: (e: any) => toast({ title: 'Save failed', description: e.message, variant: 'destructive' }),
  })

  if (isLoading) return <p className="text-sm text-muted-foreground py-4">Loading…</p>

  return (
    <div className="space-y-5">
      {pricing ? (
        <div className="p-3 bg-green-50 border border-green-100 rounded text-xs text-green-800">
          Pricing configured — responses for this model include a computed <span className="font-mono">usage.cost</span>.
          Active since {new Date(pricing.effective_from).toLocaleString()}.
        </div>
      ) : isError ? (
        <div className="p-3 bg-yellow-50 border border-yellow-100 rounded text-xs text-yellow-800">
          No pricing configured. Responses for this model won&apos;t include <span className="font-mono">usage.cost</span> until a rate is set below.
          Cloud-provider models (OpenRouter, etc.) don&apos;t need this — they already report their own real cost.
        </div>
      ) : null}

      <div>
        <p className="text-xs font-semibold text-muted-foreground uppercase mb-3">Per-Token Rate</p>
        <div className="grid grid-cols-3 gap-3">
          <div>
            <Label className="text-xs">Input $/token</Label>
            <Input type="number" step="0.0000000001" min="0" value={inputRate}
              onChange={e => setInputRate(e.target.value)} className="mt-1 font-mono text-xs" />
          </div>
          <div>
            <Label className="text-xs">Output $/token</Label>
            <Input type="number" step="0.0000000001" min="0" value={outputRate}
              onChange={e => setOutputRate(e.target.value)} className="mt-1 font-mono text-xs" />
          </div>
          <div>
            <Label className="text-xs">Cached input $/token</Label>
            <Input type="number" step="0.0000000001" min="0" value={cachedRate}
              onChange={e => setCachedRate(e.target.value)} className="mt-1 font-mono text-xs" />
          </div>
        </div>
        <p className="text-xs text-muted-foreground mt-2">
          Example: $0.50 per 1M tokens = <span className="font-mono">0.0000005</span>.
          Saving replaces the active rate — the previous one is kept (versioned) for past usage records.
        </p>
      </div>

      <div>
        <Label className="text-xs">Currency</Label>
        <Input value={currency} onChange={e => setCurrency(e.target.value.toUpperCase())}
          maxLength={3} className="mt-1 w-24 font-mono text-xs" />
      </div>

      <Button onClick={() => save.mutate()} disabled={save.isPending} className="w-full">
        {save.isPending ? 'Saving…' : 'Save Pricing'}
      </Button>
    </div>
  )
}

// ─────────────────────────────────────────────────────────────────────────────
// THINKING TAB — reasoning mode capability flags & deployment defaults
// ─────────────────────────────────────────────────────────────────────────────
function ThinkingTab({ modelId }: { modelId: string }) {
  const qc = useQueryClient()

  // Fetch the model to get current thinking flags
  const { data: models } = useQuery({
    queryKey: ['models', 'active'],
    queryFn: () => api.models.list('active'),
  })
  const model = models?.data.find(m => m.id === modelId)

  const [supportsThinking,  setSupportsThinking]  = useState<boolean | null>(null)
  const [thinkingEnabled,   setThinkingEnabled]   = useState<boolean | null>(null)
  const [minThinkingTokens, setMinThinkingTokens] = useState<string | null>(null)
  const [initialized, setInitialized] = useState(false)

  if (model && !initialized) {
    setInitialized(true)
    setSupportsThinking(model.supports_thinking ?? false)
    setThinkingEnabled(model.thinking_enabled ?? false)
    setMinThinkingTokens(String(model.min_thinking_tokens ?? 500))
  }

  const save = useMutation({
    mutationFn: () => api.models.setThinkingMode(modelId, {
      supports_thinking:   supportsThinking ?? false,
      thinking_enabled:    thinkingEnabled  ?? false,
      min_thinking_tokens: parseInt(minThinkingTokens ?? '500') || 500,
    }),
    onSuccess: () => {
      toast({ title: 'Thinking mode updated' })
      qc.invalidateQueries({ queryKey: ['models'] })
    },
    onError: (e: any) => toast({ title: 'Save failed', description: e.message, variant: 'destructive' }),
  })

  const isThinking = supportsThinking ?? model?.supports_thinking ?? false
  const isEnabled  = thinkingEnabled  ?? model?.thinking_enabled  ?? false

  return (
    <div className="space-y-5">
      {/* Mode badge */}
      <div className="flex items-center gap-3 p-4 rounded-lg border bg-gray-50">
        {isThinking ? (
          isEnabled ? (
            <span className="inline-flex items-center gap-1.5 px-3 py-1 rounded-full text-sm font-semibold bg-purple-100 text-purple-800">
              <Brain className="w-4 h-4" />🧠 Reasoning Mode
            </span>
          ) : (
            <span className="inline-flex items-center gap-1.5 px-3 py-1 rounded-full text-sm font-semibold bg-yellow-100 text-yellow-800">
              <Zap className="w-4 h-4" />⚡ Fast Mode (thinking disabled by default)
            </span>
          )
        ) : (
          <span className="inline-flex items-center gap-1.5 px-3 py-1 rounded-full text-sm font-semibold bg-gray-100 text-gray-600">
            <Zap className="w-4 h-4" />Standard (no thinking capability)
          </span>
        )}
      </div>

      {/* Capability flag */}
      <div className="space-y-3">
        <p className="text-xs font-semibold text-muted-foreground uppercase">Capability</p>
        <label className="flex items-start gap-3 p-3 rounded-lg border cursor-pointer hover:bg-gray-50">
          <input
            type="checkbox"
            className="mt-0.5 w-4 h-4"
            checked={supportsThinking ?? false}
            onChange={e => {
              setSupportsThinking(e.target.checked)
              if (!e.target.checked) setThinkingEnabled(false)
            }}
          />
          <div>
            <p className="text-sm font-medium">This model supports thinking/reasoning</p>
            <p className="text-xs text-muted-foreground mt-0.5">
              Enables thinking mode control for Qwen3, DeepSeek-R1, and similar reasoning models.
              When off, all thinking features are disabled and no <code className="bg-gray-100 px-1 rounded">&lt;think&gt;</code> injection occurs.
            </p>
          </div>
        </label>
      </div>

      {/* Deployment default — only shown when supports_thinking is on */}
      {(supportsThinking ?? false) && (
        <div className="space-y-3">
          <p className="text-xs font-semibold text-muted-foreground uppercase">Deployment Default</p>

          <div className="grid grid-cols-2 gap-3">
            <label
              className={`flex items-start gap-3 p-3 rounded-lg border cursor-pointer transition-colors ${
                isEnabled ? 'border-purple-400 bg-purple-50' : 'hover:bg-gray-50'
              }`}
            >
              <input
                type="radio"
                name="think_mode"
                className="mt-0.5"
                checked={isEnabled}
                onChange={() => setThinkingEnabled(true)}
              />
              <div>
                <p className="text-sm font-medium flex items-center gap-1">🧠 Reasoning</p>
                <p className="text-xs text-muted-foreground mt-0.5">
                  Thinking on by default. Full chain-of-thought before answering.
                  Produces the best results but uses more tokens.
                </p>
              </div>
            </label>

            <label
              className={`flex items-start gap-3 p-3 rounded-lg border cursor-pointer transition-colors ${
                !isEnabled ? 'border-yellow-400 bg-yellow-50' : 'hover:bg-gray-50'
              }`}
            >
              <input
                type="radio"
                name="think_mode"
                className="mt-0.5"
                checked={!isEnabled}
                onChange={() => setThinkingEnabled(false)}
              />
              <div>
                <p className="text-sm font-medium flex items-center gap-1">⚡ Fast Mode</p>
                <p className="text-xs text-muted-foreground mt-0.5">
                  Thinking disabled by default. Responds immediately.
                  Clients can still enable thinking per-request.
                </p>
              </div>
            </label>
          </div>

          <div className="w-64">
            <Label className="text-xs">
              Auto-disable threshold <span className="font-normal text-muted-foreground">(tokens)</span>
            </Label>
            <Input
              type="number"
              value={minThinkingTokens ?? '500'}
              onChange={e => setMinThinkingTokens(e.target.value)}
              className="mt-1"
            />
            <p className="text-xs text-muted-foreground mt-0.5">
              When <code className="bg-gray-100 px-1 rounded">max_tokens</code> is below this, thinking is
              automatically disabled to prevent empty responses.
            </p>
          </div>
        </div>
      )}

      {/* Per-request override info */}
      {(supportsThinking ?? false) && (
        <div className="p-3 rounded-lg bg-blue-50 border border-blue-100 text-xs text-blue-800 space-y-1.5">
          <p className="font-semibold">Per-request override</p>
          <p>Clients can always override the deployment default:</p>
          <pre className="bg-white rounded p-2 text-[10px] overflow-x-auto">{`// Force thinking on for this request
{"model":"${modelId}","thinking":{"type":"enabled","budget_tokens":2048},...}

// Force fast mode for this request
{"model":"${modelId}","thinking":{"type":"disabled"},...}`}</pre>
          <p className="text-blue-700">
            The gateway auto-disables thinking if <code className="bg-blue-100 px-0.5 rounded">max_tokens</code> is
            below the threshold, and retries with thinking off if the response is empty.
          </p>
        </div>
      )}

      <Button onClick={() => save.mutate()} disabled={save.isPending} className="w-full">
        {save.isPending ? 'Saving…' : 'Save Thinking Mode'}
      </Button>
    </div>
  )
}

function RuntimesTab({ modelId }: { modelId: string }) {
  const { data, isLoading } = useQuery({
    queryKey: ['model-runtime-status', modelId],
    queryFn: () => api.models.getRuntimeStatus(modelId),
    refetchInterval: 8_000,
  })
  const runtimes = data?.runtimes ?? []
  const stateColor = (s: string) => ({
    ready: 'bg-green-100 text-green-800', active: 'bg-green-100 text-green-800',
    warm: 'bg-green-100 text-green-800', idle: 'bg-yellow-100 text-yellow-800',
    loading_model: 'bg-blue-100 text-blue-800', starting: 'bg-blue-100 text-blue-800',
    failed: 'bg-red-100 text-red-800', lost: 'bg-red-100 text-red-800',
  }[s] ?? 'bg-gray-100 text-gray-600')

  if (isLoading) return <p className="text-sm text-muted-foreground py-4">Loading runtimes…</p>
  if (runtimes.length === 0) return <p className="text-sm text-muted-foreground py-4 text-center">No active runtimes.</p>

  return (
    <div className="space-y-2">
      {runtimes.map((r: any) => (
        <div key={r.runtime_id} className="flex items-center gap-3 p-3 rounded-lg border bg-white text-sm">
          <Server className="w-4 h-4 text-muted-foreground" />
          <div className="flex-1">
            <div className="flex items-center gap-2">
              <span className="font-medium">{r.hostname ?? r.node_id?.slice(0, 8)}</span>
              <span className={`px-1.5 py-0.5 rounded-full text-xs ${stateColor(r.state)}`}>{r.state}</span>
            </div>
            <p className="text-xs text-muted-foreground font-mono">{r.bind_host}:{r.bind_port}</p>
          </div>
          {r.last_used_at && (
            <span className="text-xs text-muted-foreground">{new Date(r.last_used_at).toLocaleTimeString()}</span>
          )}
        </div>
      ))}
    </div>
  )
}

// ─────────────────────────────────────────────────────────────────────────────
// CAPABILITIES TAB — view and edit which API endpoints this model supports
// ─────────────────────────────────────────────────────────────────────────────

// Mapping of capability identifier → human label + endpoint + color
const CAPABILITY_META: Record<string, { label: string; endpoint: string; color: string }> = {
  chat:             { label: 'Chat Completions',  endpoint: 'POST /v1/chat/completions',    color: 'bg-blue-100 text-blue-700 border-blue-200' },
  completion:       { label: 'Text Completions',  endpoint: 'POST /v1/completions',         color: 'bg-sky-100 text-sky-700 border-sky-200' },
  responses:        { label: 'Responses',         endpoint: 'POST /v1/responses',           color: 'bg-indigo-100 text-indigo-700 border-indigo-200' },
  embedding:        { label: 'Embeddings',        endpoint: 'POST /v1/embeddings',          color: 'bg-purple-100 text-purple-700 border-purple-200' },
  rerank:           { label: 'Rerank',            endpoint: 'POST /v1/rerank',              color: 'bg-violet-100 text-violet-700 border-violet-200' },
  transcription:    { label: 'Audio Transcription', endpoint: 'POST /v1/audio/transcriptions', color: 'bg-teal-100 text-teal-700 border-teal-200' },
  speech:           { label: 'Audio Speech',      endpoint: 'POST /v1/audio/speech',       color: 'bg-cyan-100 text-cyan-700 border-cyan-200' },
  image_generation: { label: 'Image Generation',  endpoint: 'POST /v1/images/generations', color: 'bg-rose-100 text-rose-700 border-rose-200' },
  moderation:       { label: 'Moderation',        endpoint: 'POST /v1/moderations',        color: 'bg-orange-100 text-orange-700 border-orange-200' },
  ocr:              { label: 'OCR',               endpoint: 'POST /v1/ocr',                color: 'bg-amber-100 text-amber-700 border-amber-200' },
  vision:           { label: 'Vision (multimodal)', endpoint: 'POST /v1/chat/completions', color: 'bg-pink-100 text-pink-700 border-pink-200' },
}

const ALL_CAPABILITIES = Object.keys(CAPABILITY_META)

function CapabilitiesTab({ modelId }: { modelId: string }) {
  const qc = useQueryClient()

  // Load the model to see current capabilities
  const { data: models, isLoading } = useQuery({
    queryKey: ['models', 'active'],
    queryFn: () => api.models.list('active'),
  })
  const model = models?.data.find(m => m.id === modelId)

  const [selected, setSelected] = useState<string[]>([])
  const [initialized, setInitialized] = useState(false)

  // Seed form from model data once loaded
  if (model && !initialized) {
    setInitialized(true)
    setSelected(model.capabilities ?? [])
  }

  const toggle = (cap: string) => {
    setSelected(prev =>
      prev.includes(cap) ? prev.filter(c => c !== cap) : [...prev, cap]
    )
  }

  const save = useMutation({
    mutationFn: () => api.models.setCapabilities(modelId, selected),
    onSuccess: (r) => {
      toast({
        title: 'Capabilities updated',
        description: `${r.capabilities.length} capability${r.capabilities.length !== 1 ? 'ies' : ''} saved — gateway enforces immediately`,
      })
      qc.invalidateQueries({ queryKey: ['models'] })
    },
    onError: (e: any) => toast({ title: 'Save failed', description: e.message, variant: 'destructive' }),
  })

  if (isLoading) return <p className="text-sm text-muted-foreground py-4">Loading…</p>

  const changed = JSON.stringify([...selected].sort()) !== JSON.stringify([...(model?.capabilities ?? [])].sort())

  return (
    <div className="space-y-5">
      {/* Explainer */}
      <div className="p-3 rounded-lg bg-blue-50 border border-blue-100 text-xs text-blue-800 space-y-1">
        <p className="font-semibold">Gateway Capability Enforcement</p>
        <p>
          The gateway validates every inference request against this list before routing it to the backend.
          Requests to unsupported endpoints receive HTTP 400 with a structured error — the backend never sees them.
        </p>
        <p>
          Changes take effect immediately — no restart required.
        </p>
      </div>

      {/* Current state pill row */}
      {model?.capabilities && model.capabilities.length > 0 && (
        <div>
          <p className="text-xs font-semibold text-muted-foreground uppercase mb-2">Current Capabilities</p>
          <div className="flex flex-wrap gap-1.5">
            {model.capabilities.map(cap => {
              const meta = CAPABILITY_META[cap]
              return (
                <span
                  key={cap}
                  className={`inline-flex items-center gap-1 text-[11px] px-2.5 py-1 rounded-full font-medium border ${
                    meta?.color ?? 'bg-gray-100 text-gray-700 border-gray-200'
                  }`}
                >
                  ✓ {meta?.label ?? cap}
                </span>
              )
            })}
          </div>
        </div>
      )}

      {/* Edit checklist */}
      <div>
        <p className="text-xs font-semibold text-muted-foreground uppercase mb-3">Edit Capabilities</p>
        <div className="grid grid-cols-1 gap-2">
          {ALL_CAPABILITIES.map(cap => {
            const meta = CAPABILITY_META[cap]
            const on = selected.includes(cap)
            return (
              <label
                key={cap}
                className={`flex items-center gap-3 border rounded-lg px-3 py-2.5 cursor-pointer transition-all ${
                  on
                    ? `${meta.color} border-current shadow-sm`
                    : 'hover:border-gray-300 hover:bg-gray-50'
                }`}
              >
                <input
                  type="checkbox"
                  checked={on}
                  onChange={() => toggle(cap)}
                  className="h-4 w-4 shrink-0"
                />
                <div className="flex-1 min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="text-sm font-semibold">{meta.label}</span>
                    <code className="text-[10px] bg-white/60 px-1.5 py-0.5 rounded border border-current/20">
                      {cap}
                    </code>
                  </div>
                  <p className="text-xs opacity-75 mt-0.5">{meta.endpoint}</p>
                </div>
              </label>
            )
          })}
        </div>
      </div>

      {/* Error format reminder */}
      <details className="border rounded-md text-xs">
        <summary className="px-3 py-2 cursor-pointer select-none text-muted-foreground font-medium hover:text-foreground">
          Error response format when capability check fails
        </summary>
        <pre className="px-3 pb-3 pt-2 text-[11px] bg-gray-50 rounded-b-md overflow-x-auto text-gray-700">{`HTTP 400
{
  "error": {
    "type": "invalid_model",
    "message": "Model '...' does not support Chat Completions.",
    "required_capability": "chat",
    "model_capabilities": ["transcription"]
  }
}`}</pre>
      </details>

      {changed && (
        <div className="flex items-center gap-2 p-2.5 rounded-lg bg-amber-50 border border-amber-200 text-xs text-amber-800">
          <AlertTriangle className="w-3.5 h-3.5 shrink-0" />
          Unsaved changes — save to update gateway enforcement
        </div>
      )}

      <Button
        onClick={() => save.mutate()}
        disabled={save.isPending || !changed}
        className="w-full"
      >
        {save.isPending ? 'Saving…' : `Save Capabilities (${selected.length} selected)`}
      </Button>
    </div>
  )
}

// ─────────────────────────────────────────────────────────────────────────────
// MAIN MODEL DETAIL PAGE
// ─────────────────────────────────────────────────────────────────────────────
type DetailTab = 'runtimes' | 'ha' | 'placement' | 'lazy_config' | 'pricing' | 'thinking' | 'capabilities' | 'health' | 'upstream'

export default function ModelDetailPage() {
  const { id } = useParams<{ id: string }>()
  const router = useRouter()
  const qc = useQueryClient()
  const [tab, setTab] = useState<DetailTab>('runtimes')

  const { data: health, isLoading } = useQuery({
    queryKey: ['model-health', id],
    queryFn: () => api.models.health(id),
    refetchInterval: 15_000,
  })

  const tabs: { key: DetailTab; label: string }[] = [
    { key: 'runtimes',     label: 'Runtimes' },
    { key: 'ha',           label: 'HA Replicas' },
    { key: 'placement',    label: 'Placement' },
    { key: 'lazy_config',  label: 'Lazy Config' },
    { key: 'pricing',      label: 'Pricing' },
    { key: 'thinking',     label: '🧠 Thinking' },
    { key: 'capabilities', label: '🔌 Capabilities' },
    { key: 'upstream',     label: '🌐 Upstream' },
    { key: 'health',       label: 'Endpoints' },
  ]

  if (isLoading) return <div className="text-muted-foreground text-sm p-8">Loading…</div>

  return (
    <div className="space-y-5">
      {/* Breadcrumb */}
      <div>
        <Button variant="ghost" size="sm" className="-ml-2 text-muted-foreground"
          onClick={() => router.push('/models')}>
          <ArrowLeft className="w-4 h-4 mr-1" />Models
        </Button>
        <h1 className="text-xl font-bold mt-1">{id}</h1>
      </div>

      {/* Tab bar */}
      <div className="flex gap-0 border-b">
        {tabs.map(t => (
          <button key={t.key} onClick={() => setTab(t.key)}
            className={`px-4 py-2.5 text-sm font-medium border-b-2 transition-colors ${
              tab === t.key ? 'border-blue-600 text-blue-600' : 'border-transparent text-muted-foreground hover:text-foreground'
            }`}>
            {t.label}
          </button>
        ))}
      </div>

      {/* Tab content */}
      <Card>
        <CardContent className="pt-4">
          {tab === 'runtimes'     && <RuntimesTab modelId={id} />}
          {tab === 'ha'           && <HATab modelId={id} />}
          {tab === 'placement'    && <PlacementTab modelId={id} />}
          {tab === 'lazy_config'  && <LazyConfigTab modelId={id} />}
          {tab === 'pricing'      && <PricingTab modelId={id} />}
          {tab === 'thinking'     && <ThinkingTab modelId={id} />}
          {tab === 'capabilities' && <CapabilitiesTab modelId={id} />}
          {tab === 'upstream'     && <UpstreamTab modelId={id} />}
          {tab === 'health'       && <EndpointsTab modelId={id} />}
        </CardContent>
      </Card>
    </div>
  )
}

// ─────────────────────────────────────────────────────────────────────────────
// UPSTREAM TAB — API key, base URL, and proxy for cloud / external models
// ─────────────────────────────────────────────────────────────────────────────

function UpstreamTab({ modelId }: { modelId: string }) {
  const qc = useQueryClient()

  // Load the endpoint row to prefill current values.
  // We use the health endpoint — it returns endpoint rows which carry the
  // upstream fields when the model has them.
  const { data: healthData, isLoading } = useQuery({
    queryKey: ['model-health', modelId],
    queryFn: () => api.models.health(modelId),
  })
  const ep = (healthData?.endpoints ?? [])[0] as any

  const [apiKey,  setApiKey]  = useState('')
  const [baseUrl, setBaseUrl] = useState('')
  const [proxy,   setProxy]   = useState('')
  const [modelName, setModelName] = useState('')
  const [showKey, setShowKey] = useState(false)
  const [initialized, setInitialized] = useState(false)

  if (ep && !initialized) {
    setInitialized(true)
    setBaseUrl(ep.upstream_base_url   ?? '')
    setProxy(ep.upstream_proxy        ?? '')
    setModelName(ep.upstream_model_name ?? '')
    // Never prefill the API key — force a deliberate re-entry for security.
  }

  const save = useMutation({
    mutationFn: () => api.models.updateUpstream(modelId, {
      upstream_api_key:    apiKey    !== '' ? apiKey    : undefined,
      upstream_base_url:   baseUrl,
      upstream_proxy:      proxy,
      upstream_model_name: modelName,
    }),
    onSuccess: () => {
      toast({ title: 'Upstream config saved' })
      qc.invalidateQueries({ queryKey: ['model-health', modelId] })
      setInitialized(false) // re-read from refreshed data
    },
    onError: (e: any) => toast({ title: 'Save failed', description: e.message, variant: 'destructive' }),
  })

  const clearProxy = useMutation({
    mutationFn: () => api.models.updateUpstream(modelId, { upstream_proxy: '' }),
    onSuccess: () => {
      setProxy('')
      toast({ title: 'Proxy removed — using direct connection' })
      qc.invalidateQueries({ queryKey: ['model-health', modelId] })
    },
    onError: (e: any) => toast({ title: 'Failed', description: e.message, variant: 'destructive' }),
  })

  if (isLoading) return <p className="text-sm text-muted-foreground py-4">Loading…</p>

  const hasProxy     = (ep?.upstream_proxy      ?? '') !== ''
  const hasBaseUrl   = (ep?.upstream_base_url   ?? '') !== ''
  const hasModelName = (ep?.upstream_model_name ?? '') !== ''
  const hasApiKey    = ep?.upstream_api_key_set === true || hasBaseUrl

  return (
    <div className="space-y-5">

      {/* Status summary */}
      <div className="grid grid-cols-4 gap-3">
        <div className={`flex items-center gap-2 p-3 rounded-lg border text-xs ${hasBaseUrl ? 'bg-blue-50 border-blue-200' : 'bg-gray-50'}`}>
          <Globe className={`w-4 h-4 shrink-0 ${hasBaseUrl ? 'text-blue-600' : 'text-gray-400'}`} />
          <div className="min-w-0">
            <p className="font-semibold text-gray-700">Base URL</p>
            <p className={`truncate ${hasBaseUrl ? 'text-blue-700 font-mono' : 'text-muted-foreground'}`}>
              {hasBaseUrl ? (ep?.upstream_base_url ?? '') : 'not set'}
            </p>
          </div>
        </div>
        <div className={`flex items-center gap-2 p-3 rounded-lg border text-xs ${hasModelName ? 'bg-violet-50 border-violet-200' : 'bg-gray-50'}`}>
          <Globe className={`w-4 h-4 shrink-0 ${hasModelName ? 'text-violet-600' : 'text-gray-400'}`} />
          <div className="min-w-0">
            <p className="font-semibold text-gray-700">Model ID</p>
            <p className={`truncate ${hasModelName ? 'text-violet-700 font-mono' : 'text-muted-foreground'}`}>
              {hasModelName ? (ep?.upstream_model_name ?? '') : 'uses NexusLLM name'}
            </p>
          </div>
        </div>
        <div className={`flex items-center gap-2 p-3 rounded-lg border text-xs ${hasProxy ? 'bg-amber-50 border-amber-200' : 'bg-gray-50'}`}>
          <Globe className={`w-4 h-4 shrink-0 ${hasProxy ? 'text-amber-600' : 'text-gray-400'}`} />
          <div className="min-w-0">
            <p className="font-semibold text-gray-700">Proxy</p>
            <p className={`truncate ${hasProxy ? 'text-amber-700 font-mono' : 'text-muted-foreground'}`}>
              {hasProxy ? (ep?.upstream_proxy ?? '') : 'direct connection'}
            </p>
          </div>
        </div>
        <div className={`flex items-center gap-2 p-3 rounded-lg border text-xs ${hasApiKey ? 'bg-green-50 border-green-200' : 'bg-gray-50'}`}>
          <KeyRound className={`w-4 h-4 shrink-0 ${hasApiKey ? 'text-green-600' : 'text-gray-400'}`} />
          <div className="min-w-0">
            <p className="font-semibold text-gray-700">API Key</p>
            <p className={hasApiKey ? 'text-green-700' : 'text-muted-foreground'}>
              {hasApiKey ? '●●●● stored' : 'not set'}
            </p>
          </div>
        </div>
      </div>

      {/* Info banner */}
      <div className="p-3 rounded-lg bg-blue-50 border border-blue-100 text-xs text-blue-800 space-y-1">
        <p className="font-semibold">Cloud / External Model Routing</p>
        <p>
          <strong>Base URL</strong> — overrides host:port for cloud endpoints (e.g. <code className="bg-blue-100 px-1 rounded">https://openrouter.ai/api/v1</code>).
        </p>
        <p>
          <strong>Upstream model name</strong> — the model ID sent to the provider in <code className="bg-blue-100 px-1 rounded">req.model</code>.
          Required when your NexusLLM name differs from the provider's (e.g. <code className="bg-blue-100 px-1 rounded">meta-llama/llama-3.1-405b-instruct</code> for OpenRouter).
        </p>
        <p>
          <strong>API Key</strong> — injected as <code className="bg-blue-100 px-1 rounded">Authorization: Bearer</code> on every upstream request. Never shown to clients.
        </p>
        <p>
          <strong>Proxy</strong> — routes outbound requests through an HTTP or SOCKS5 proxy.
          Leave empty for a direct connection. Only this model's traffic is affected.
        </p>
      </div>

      {/* Edit form */}
      <div className="space-y-4">
        <div>
          <Label className="text-xs">Upstream base URL</Label>
          <Input value={baseUrl} onChange={e => setBaseUrl(e.target.value)}
            placeholder="https://openrouter.ai/api/v1"
            className="mt-1 font-mono text-xs" />
          <p className="text-xs text-muted-foreground mt-0.5">
            The gateway appends standard paths — e.g. <code className="bg-gray-100 px-1 rounded">/chat/completions</code>
          </p>
        </div>

        <div>
          <Label className="text-xs">API Key <span className="font-normal text-muted-foreground">(leave blank to keep current key)</span></Label>
          <div className="relative mt-1">
            <Input
              type={showKey ? 'text' : 'password'}
              value={apiKey}
              onChange={e => setApiKey(e.target.value)}
              placeholder="sk-or-…  /  sk-…  /  AIza…"
              className="pr-9 font-mono text-xs"
            />
            <button
              type="button"
              onClick={() => setShowKey(v => !v)}
              className="absolute right-2 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
            >
              {showKey ? <EyeOff className="w-4 h-4" /> : <Eye className="w-4 h-4" />}
            </button>
          </div>
          <p className="text-xs text-muted-foreground mt-0.5">
            Stored encrypted in DB. Entering a new value replaces the existing key.
          </p>
        </div>

        <div>
          <Label className="text-xs">Upstream model name <span className="font-normal text-muted-foreground">(optional)</span></Label>
          <Input value={modelName} onChange={e => setModelName(e.target.value)}
            placeholder="meta-llama/llama-3.1-405b-instruct"
            className="mt-1 font-mono text-xs" />
          <p className="text-xs text-muted-foreground mt-0.5">
            Sent as <code className="bg-gray-100 px-1 rounded">req.model</code> to the upstream provider.
            Empty = forward the NexusLLM model name unchanged.
          </p>
        </div>

        <div>
          <Label className="text-xs">Proxy URL <span className="font-normal text-muted-foreground">(optional)</span></Label>
          <div className="flex gap-2 mt-1">
            <Input value={proxy} onChange={e => setProxy(e.target.value)}
              placeholder="http://squid.corp:3128  or  socks5://proxy:1080"
              className="flex-1 font-mono text-xs" />
            {hasProxy && (
              <Button type="button" variant="outline" size="sm"
                disabled={clearProxy.isPending}
                onClick={() => clearProxy.mutate()}
                className="shrink-0 text-red-600 hover:text-red-700 hover:border-red-300">
                Remove proxy
              </Button>
            )}
          </div>
          <p className="text-xs text-muted-foreground mt-0.5">
            Empty = direct connection. Supports <code className="bg-gray-100 px-1 rounded">http://</code> and <code className="bg-gray-100 px-1 rounded">socks5://</code>.
          </p>
        </div>
      </div>

      <Button onClick={() => save.mutate()} disabled={save.isPending} className="w-full">
        {save.isPending ? 'Saving…' : 'Save Upstream Config'}
      </Button>

      <div className="p-3 bg-gray-50 border rounded text-xs text-muted-foreground space-y-1">
        <p className="font-medium text-gray-700">Proxy examples</p>
        <p><code className="bg-gray-100 px-1 rounded">http://squid.corp:3128</code> — corporate HTTP proxy</p>
        <p><code className="bg-gray-100 px-1 rounded">socks5://127.0.0.1:1080</code> — local SOCKS5 tunnel (SSH forward)</p>
        <p><code className="bg-gray-100 px-1 rounded">http://user:pass@proxy:3128</code> — proxy with auth</p>
      </div>
    </div>
  )
}

function EndpointsTab({ modelId }: { modelId: string }) {
  const qc = useQueryClient()
  const { data, isLoading } = useQuery({
    queryKey: ['model-health', modelId],
    queryFn: () => api.models.health(modelId),
    refetchInterval: 8_000,
  })
  const endpoints = data?.endpoints ?? []
  const hc = (s: string) => s === 'healthy' ? 'bg-green-100 text-green-800' : s === 'degraded' ? 'bg-yellow-100 text-yellow-800' : 'bg-red-100 text-red-800'

  if (isLoading) return <p className="text-sm text-muted-foreground py-4">Loading…</p>
  if (endpoints.length === 0) return <p className="text-sm text-muted-foreground py-4 text-center">No endpoints.</p>

  return (
    <div className="space-y-2">
      <div className="flex justify-end mb-2">
        <Button size="sm" variant="outline"
          onClick={() => api.models.resetHealth(modelId).then(() => qc.invalidateQueries({ queryKey: ['model-health', modelId] }))}>
          <RefreshCw className="w-3.5 h-3.5 mr-1" />Reset Health
        </Button>
      </div>
      {endpoints.map((ep: any) => (
        <div key={ep.id} className="flex items-center gap-3 p-3 rounded-lg border text-sm">
          <div className="flex-1">
            <div className="flex items-center gap-2">
              <span className="font-mono">{ep.host}:{ep.port}</span>
              <span className={`px-1.5 py-0.5 rounded-full text-xs ${hc(ep.health_status)}`}>{ep.health_status}</span>
              <span className="text-xs text-muted-foreground">{ep.lifecycle_state}</span>
            </div>
            {ep.response_time_ms && (
              <p className="text-xs text-muted-foreground mt-0.5">{ep.response_time_ms}ms avg</p>
            )}
          </div>
          {ep.consecutive_failures > 0 && (
            <span className="text-xs text-red-600 flex items-center gap-0.5">
              <AlertTriangle className="w-3 h-3" />{ep.consecutive_failures} failures
            </span>
          )}
        </div>
      ))}
    </div>
  )
}
