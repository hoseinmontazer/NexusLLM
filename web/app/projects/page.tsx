'use client'

import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import Link from 'next/link'
import { api, type Project, type ProjectStatus, type PriorityPreset } from '@/lib/api'
import { PriorityBadge, PriorityBar } from '@/components/projects/PriorityBadge'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'
import { toast } from '@/components/ui/toaster'
import { Plus, ChevronRight, FolderKanban, Shield, Zap } from 'lucide-react'

const STATUSES: ProjectStatus[] = ['active', 'inactive', 'archived']

function StatusBadge({ status }: { status: ProjectStatus }) {
  const map: Record<ProjectStatus, string> = {
    active:   'bg-green-100 text-green-700',
    inactive: 'bg-yellow-100 text-yellow-700',
    archived: 'bg-gray-100 text-gray-500',
  }
  return <span className={`px-2 py-0.5 rounded-full text-xs font-medium ${map[status] ?? map.active}`}>{status}</span>
}

// ── Priority weight input with preset picker ──────────────────────────────────
function PriorityInput({
  value, onChange, presets
}: {
  value: number
  onChange: (v: number) => void
  presets: PriorityPreset[]
}) {
  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2">
        <Input
          type="number" min={0} max={1000} value={value}
          onChange={e => onChange(Math.min(1000, Math.max(0, parseInt(e.target.value) || 0)))}
          className="w-24"
        />
        <PriorityBadge weight={value} showWeight />
      </div>
      <PriorityBar weight={value} />
      <div className="flex flex-wrap gap-1.5 pt-1">
        {presets.map(p => (
          <button
            key={p.weight}
            type="button"
            onClick={() => onChange(p.weight)}
            className={`text-xs px-2 py-0.5 rounded border transition-colors ${
              value === p.weight ? 'bg-blue-600 text-white border-blue-600' : 'hover:bg-gray-50 border-gray-200'
            }`}
          >
            {p.weight} · {p.label}
          </button>
        ))}
      </div>
    </div>
  )
}

// ── Create project form ────────────────────────────────────────────────────────
function CreateProjectForm({ onDone, presets }: { onDone: () => void; presets: PriorityPreset[] }) {
  const qc = useQueryClient()
  const [form, setForm] = useState({
    organization_id: '', team_id: '', name: '', description: '',
    priority_weight: 500, preemptible: true,
  })
  const { data: orgsData } = useQuery({ queryKey: ['orgs'], queryFn: api.orgs.list })
  const { data: teamsData } = useQuery({
    queryKey: ['teams', form.organization_id],
    queryFn: () => api.teams.list(form.organization_id || undefined),
    enabled: !!form.organization_id,
  })
  const mut = useMutation({
    mutationFn: () => api.projects.create({
      organization_id: form.organization_id,
      team_id: form.team_id,
      name: form.name,
      description: form.description || undefined,
      priority_weight: form.priority_weight,
      preemptible: form.preemptible,
    }),
    onSuccess: () => {
      toast({ title: 'Project created', description: form.name })
      qc.invalidateQueries({ queryKey: ['projects'] })
      onDone()
    },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })
  const set = (k: string) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm(p => ({ ...p, [k]: e.target.value }))

  return (
    <form onSubmit={e => { e.preventDefault(); mut.mutate() }} className="space-y-4">
      <div className="grid grid-cols-2 gap-3">
        <div className="col-span-2">
          <Label>Organization *</Label>
          <select className="w-full border rounded-md h-9 px-3 text-sm mt-1"
            value={form.organization_id} onChange={set('organization_id')} required>
            <option value="">Select organization…</option>
            {orgsData?.data?.map(o => <option key={o.id} value={o.id}>{o.name}</option>)}
          </select>
        </div>
        <div className="col-span-2">
          <Label>Team *</Label>
          <select className="w-full border rounded-md h-9 px-3 text-sm mt-1"
            value={form.team_id} onChange={set('team_id')} required disabled={!form.organization_id}>
            <option value="">Select team…</option>
            {teamsData?.data?.map(t => <option key={t.id} value={t.id}>{t.name}</option>)}
          </select>
        </div>
        <div className="col-span-2">
          <Label>Project name *</Label>
          <Input value={form.name} onChange={set('name')} placeholder="Customer Support Bot" required maxLength={200} />
        </div>
        <div className="col-span-2">
          <Label>Description</Label>
          <Input value={form.description} onChange={set('description')} placeholder="Optional description" maxLength={1000} />
        </div>
        <div className="col-span-2">
          <Label className="mb-2 block">Priority Weight (0–1000)</Label>
          <PriorityInput
            value={form.priority_weight}
            onChange={v => setForm(p => ({ ...p, priority_weight: v }))}
            presets={presets}
          />
        </div>
        <div className="col-span-2">
          <label className="flex items-center gap-2 cursor-pointer text-sm">
            <input type="checkbox" className="w-4 h-4"
              checked={form.preemptible}
              onChange={e => setForm(p => ({ ...p, preemptible: e.target.checked }))}
            />
            <span className="font-medium">Preemptible</span>
            <span className="text-muted-foreground text-xs">(can be evicted under resource pressure)</span>
          </label>
        </div>
      </div>
      <Button type="submit" disabled={mut.isPending} className="w-full">
        {mut.isPending ? 'Creating…' : 'Create Project'}
      </Button>
    </form>
  )
}

// ── Main page ─────────────────────────────────────────────────────────────────
export default function ProjectsPage() {
  const [open, setOpen] = useState(false)
  const [filterStatus, setFilterStatus] = useState('active')

  const params: Record<string, string> = {}
  if (filterStatus) params.status = filterStatus

  const { data, isLoading, error } = useQuery({
    queryKey: ['projects', filterStatus],
    queryFn: () => api.projects.list(params),
    refetchInterval: 15_000,
  })
  const { data: presetData } = useQuery({
    queryKey: ['priority-presets'],
    queryFn: api.scheduler.getPriorityPresets,
    staleTime: Infinity,
  })
  const presets = presetData?.presets ?? []
  const projects = data?.data ?? []

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-3">
          <FolderKanban className="w-7 h-7 text-blue-600" />
          <div>
            <h1 className="text-2xl font-bold">Projects</h1>
            <p className="text-sm text-muted-foreground">
              {data?.total ?? 0} project{(data?.total ?? 0) !== 1 ? 's' : ''} — SLA enforcement units
            </p>
          </div>
        </div>
        <Dialog open={open} onOpenChange={setOpen}>
          <DialogTrigger asChild>
            <Button><Plus className="w-4 h-4 mr-2" />New Project</Button>
          </DialogTrigger>
          <DialogContent className="max-w-lg">
            <DialogHeader><DialogTitle>Create Project</DialogTitle></DialogHeader>
            <CreateProjectForm onDone={() => setOpen(false)} presets={presets} />
          </DialogContent>
        </Dialog>
      </div>

      {/* Filters */}
      <div className="flex gap-3 items-center">
        <select className="border rounded-md h-8 px-2 text-sm"
          value={filterStatus} onChange={e => setFilterStatus(e.target.value)}>
          <option value="">All statuses</option>
          {STATUSES.map(s => <option key={s} value={s}>{s}</option>)}
        </select>
        {filterStatus !== 'active' && (
          <Button variant="ghost" size="sm" onClick={() => setFilterStatus('active')}>
            Reset
          </Button>
        )}
      </div>

      {isLoading && <p className="text-muted-foreground text-sm">Loading…</p>}
      {error && <p className="text-red-600 text-sm">Failed to load projects.</p>}
      {!isLoading && projects.length === 0 && (
        <Card><CardContent className="py-12 text-center text-muted-foreground">
          No projects found.
        </CardContent></Card>
      )}

      <div className="space-y-2">
        {projects.map((p: Project) => (
          <Link key={p.id} href={`/projects/${p.id}`}>
            <Card className="hover:shadow-md transition-shadow cursor-pointer">
              <CardContent className="py-3">
                <div className="flex items-center justify-between gap-4">
                  <div className="flex items-center gap-3 min-w-0 flex-1">
                    <FolderKanban className="w-5 h-5 text-gray-400 shrink-0" />
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-2 flex-wrap">
                        <span className="font-semibold">{p.name}</span>
                        <PriorityBadge weight={p.priority_weight} label={p.priority_label} showWeight />
                        <StatusBadge status={p.status} />
                        {!p.preemptible && (
                          <span className="text-xs text-purple-700 bg-purple-50 px-1.5 py-0.5 rounded border border-purple-200">non-preemptible</span>
                        )}
                        {p.protected && (
                          <span className="flex items-center gap-1 text-xs text-purple-700 bg-purple-50 px-1.5 py-0.5 rounded border border-purple-200">
                            <Shield className="w-3 h-3" /> protected
                          </span>
                        )}
                        {p.always_running && (
                          <span className="flex items-center gap-1 text-xs text-green-700 bg-green-50 px-1.5 py-0.5 rounded border border-green-200">
                            <Zap className="w-3 h-3" /> always-running
                          </span>
                        )}
                      </div>
                      <div className="flex items-center gap-2 mt-1">
                        <PriorityBar weight={p.priority_weight} className="w-20" />
                        {p.effective_priority !== p.priority_weight && (
                          <span className="text-xs text-muted-foreground">
                            effective: <span className="font-semibold text-foreground">{p.effective_priority}</span>
                          </span>
                        )}
                      </div>
                    </div>
                  </div>

                  {/* Metrics */}
                  <div className="hidden md:flex items-center gap-5 shrink-0 text-sm">
                    <div className="text-right">
                      <div className="font-medium tabular-nums">{p.runtime_count}</div>
                      <div className="text-xs text-muted-foreground">runtimes</div>
                    </div>
                    {(p as any).tokens_24h != null && (
                      <div className="text-right">
                        <div className="font-medium tabular-nums">{((p as any).tokens_24h as number).toLocaleString()}</div>
                        <div className="text-xs text-muted-foreground">tokens 24h</div>
                      </div>
                    )}
                    {(p as any).cost_usd_24h != null && (p as any).cost_usd_24h > 0 && (
                      <div className="text-right">
                        <div className="font-medium tabular-nums">${((p as any).cost_usd_24h as number).toFixed(3)}</div>
                        <div className="text-xs text-muted-foreground">cost 24h</div>
                      </div>
                    )}
                    <ChevronRight className="w-4 h-4 text-gray-400" />
                  </div>
                  <ChevronRight className="w-4 h-4 text-gray-400 md:hidden" />
                </div>
              </CardContent>
            </Card>
          </Link>
        ))}
      </div>
    </div>
  )
}
