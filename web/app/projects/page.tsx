'use client'

import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import Link from 'next/link'
import { api, type Project, type ProjectPriority, type ProjectStatus } from '@/lib/api'
import { PriorityBadge } from '@/components/projects/PriorityBadge'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'
import { toast } from '@/components/ui/toaster'
import { Plus, ChevronRight, FolderKanban, Shield, Zap } from 'lucide-react'

const PRIORITIES: ProjectPriority[] = ['CRITICAL', 'HIGH', 'NORMAL', 'LOW', 'BEST_EFFORT']
const STATUSES: ProjectStatus[]     = ['active', 'inactive', 'archived']

function StatusBadge({ status }: { status: ProjectStatus }) {
  const map: Record<ProjectStatus, string> = {
    active:   'bg-green-100 text-green-700',
    inactive: 'bg-yellow-100 text-yellow-700',
    archived: 'bg-gray-100 text-gray-500',
  }
  return (
    <span className={`px-2 py-0.5 rounded-full text-xs font-medium ${map[status] ?? map.active}`}>
      {status}
    </span>
  )
}

// ── Create project form ────────────────────────────────────────────────────────
function CreateProjectForm({ onDone }: { onDone: () => void }) {
  const qc = useQueryClient()
  const [form, setForm] = useState({
    organization_id: '', team_id: '', name: '', description: '',
    priority: 'NORMAL' as ProjectPriority,
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
      priority: form.priority,
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
          <Label>Priority</Label>
          <select className="w-full border rounded-md h-9 px-3 text-sm mt-1"
            value={form.priority} onChange={set('priority')}>
            {PRIORITIES.map(p => <option key={p} value={p}>{p}</option>)}
          </select>
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
  const [filterPriority, setFilterPriority] = useState('')
  const [filterStatus, setFilterStatus] = useState('active')

  const params: Record<string, string> = {}
  if (filterPriority) params.priority = filterPriority
  if (filterStatus)   params.status   = filterStatus

  const { data, isLoading, error } = useQuery({
    queryKey: ['projects', filterPriority, filterStatus],
    queryFn: () => api.projects.list(params),
    refetchInterval: 15_000,
  })

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
          <DialogContent>
            <DialogHeader><DialogTitle>Create Project</DialogTitle></DialogHeader>
            <CreateProjectForm onDone={() => setOpen(false)} />
          </DialogContent>
        </Dialog>
      </div>

      <div className="flex gap-3 items-center">
        <select className="border rounded-md h-9 px-3 text-sm"
          value={filterPriority} onChange={e => setFilterPriority(e.target.value)}>
          <option value="">All priorities</option>
          {PRIORITIES.map(p => <option key={p} value={p}>{p}</option>)}
        </select>
        <select className="border rounded-md h-9 px-3 text-sm"
          value={filterStatus} onChange={e => setFilterStatus(e.target.value)}>
          <option value="">All statuses</option>
          {STATUSES.map(s => <option key={s} value={s}>{s}</option>)}
        </select>
        {(filterPriority || filterStatus !== 'active') && (
          <Button variant="ghost" size="sm" onClick={() => { setFilterPriority(''); setFilterStatus('active') }}>
            Reset
          </Button>
        )}
      </div>

      {isLoading && <p className="text-muted-foreground text-sm">Loading…</p>}
      {error && <p className="text-red-600 text-sm">Failed to load projects.</p>}

      {!isLoading && projects.length === 0 && (
        <Card><CardContent className="py-12 text-center text-muted-foreground">
          No projects found. Create one to get started.
        </CardContent></Card>
      )}

      <div className="space-y-2">
        {projects.map((p: Project) => (
          <Link key={p.id} href={`/projects/${p.id}`}>
            <Card className="hover:shadow-md transition-shadow cursor-pointer">
              <CardContent className="py-4">
                <div className="flex items-center justify-between gap-4">
                  <div className="flex items-center gap-3 min-w-0">
                    <FolderKanban className="w-5 h-5 text-gray-400 shrink-0" />
                    <div className="min-w-0">
                      <div className="flex items-center gap-2 flex-wrap">
                        <span className="font-semibold">{p.name}</span>
                        <PriorityBadge priority={p.priority} />
                        <StatusBadge status={p.status} />
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
                      {p.description && (
                        <p className="text-xs text-muted-foreground truncate mt-0.5">{p.description}</p>
                      )}
                    </div>
                  </div>
                  <div className="flex items-center gap-6 shrink-0 text-sm">
                    <div className="text-right">
                      <div className="font-medium">{p.runtime_count}</div>
                      <div className="text-xs text-muted-foreground">active runtimes</div>
                    </div>
                    {p.reserved_vram_mb > 0 && (
                      <div className="text-right">
                        <div className="font-medium">{(p.reserved_vram_mb / 1024).toFixed(0)} GB</div>
                        <div className="text-xs text-muted-foreground">reserved VRAM</div>
                      </div>
                    )}
                    <ChevronRight className="w-4 h-4 text-gray-400" />
                  </div>
                </div>
              </CardContent>
            </Card>
          </Link>
        ))}
      </div>
    </div>
  )
}
