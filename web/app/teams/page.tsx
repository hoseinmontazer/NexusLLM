'use client'

import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type Team } from '@/lib/api'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'
import { toast } from '@/components/ui/toaster'
import { Plus, ChevronDown, ChevronUp, Pencil, Trash2, FolderKanban, X, ShieldCheck } from 'lucide-react'
import Link from 'next/link'

// ── Model access grant panel ──────────────────────────────────────────────────
function ModelAccessPanel({ team }: { team: Team }) {
  const qc = useQueryClient()
  const [modelInput, setModelInput] = useState('')

  const { data: modelsData, isLoading } = useQuery({
    queryKey: ['team-models', team.id],
    queryFn: () => api.teams.listModels(team.id),
  })

  // All registered models for the autocomplete list
  const { data: allModels } = useQuery({
    queryKey: ['models'],
    queryFn: () => api.models.list(),
  })
  const modelNames = (allModels?.data ?? []).map(m => m.name)
  const grantedModels = modelsData?.models ?? []
  const ungrantedModels = modelNames.filter(n => !grantedModels.includes(n))

  const grant = useMutation({
    mutationFn: (name: string) => api.teams.addModel(team.id, name),
    onSuccess: (_,name) => {
      toast({ title: `Access granted`, description: `${team.name} → ${name}` })
      qc.invalidateQueries({ queryKey: ['team-models', team.id] })
      setModelInput('')
    },
    onError: (e: any) => toast({ title: 'Grant failed', description: e.message, variant: 'destructive' }),
  })

  const revoke = useMutation({
    mutationFn: (name: string) => api.teams.removeModel(team.id, name),
    onSuccess: (_,name) => {
      toast({ title: `Access revoked`, description: `${team.name} ✕ ${name}` })
      qc.invalidateQueries({ queryKey: ['team-models', team.id] })
    },
    onError: (e: any) => toast({ title: 'Revoke failed', description: e.message, variant: 'destructive' }),
  })

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-1.5 text-xs font-semibold text-muted-foreground uppercase tracking-wide">
        <ShieldCheck className="w-3.5 h-3.5" />Model Access
        <span className="font-normal normal-case text-muted-foreground ml-1">
          — gateway ACL, required before any inference request
        </span>
      </div>

      {/* Granted list */}
      {isLoading ? (
        <p className="text-xs text-muted-foreground">Loading…</p>
      ) : grantedModels.length === 0 ? (
        <p className="text-xs text-amber-700 bg-amber-50 border border-amber-100 rounded px-2 py-1.5">
          ⚠️ No models granted — all inference requests from this team will be rejected with <code>model_not_allowed</code>.
        </p>
      ) : (
        <div className="flex flex-wrap gap-1.5">
          {grantedModels.map(name => (
            <span key={name}
              className="inline-flex items-center gap-1 text-xs bg-green-50 text-green-700 border border-green-200 rounded-full pl-2.5 pr-1 py-0.5">
              {name}
              <button
                onClick={() => revoke.mutate(name)}
                disabled={revoke.isPending}
                className="ml-0.5 hover:text-red-600 transition-colors rounded-full p-0.5 hover:bg-red-50"
                title={`Revoke ${name}`}
              >
                <X className="w-3 h-3" />
              </button>
            </span>
          ))}
        </div>
      )}

      {/* Grant new model */}
      <div className="flex gap-2">
        <div className="flex-1 relative">
          <Input
            value={modelInput}
            onChange={e => setModelInput(e.target.value)}
            placeholder="model name…"
            className="text-sm h-8"
            list={`model-list-${team.id}`}
          />
          <datalist id={`model-list-${team.id}`}>
            {ungrantedModels.map(n => <option key={n} value={n} />)}
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

      {/* Quick-grant all */}
      {ungrantedModels.length > 0 && (
        <div className="flex flex-wrap gap-1 pt-1">
          <span className="text-xs text-muted-foreground self-center mr-1">Quick grant:</span>
          {ungrantedModels.slice(0, 6).map(name => (
            <button
              key={name}
              onClick={() => grant.mutate(name)}
              disabled={grant.isPending}
              className="text-[10px] px-2 py-0.5 rounded border border-dashed border-gray-300 text-muted-foreground hover:border-green-400 hover:text-green-700 hover:bg-green-50 transition-colors"
            >
              + {name}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

// ── Edit team form ─────────────────────────────────────────────────────────────
function EditTeamForm({ team, onDone }: { team: Team; onDone: () => void }) {
  const qc = useQueryClient()
  const [name, setName] = useState(team.name)
  const [slug, setSlug] = useState(team.slug)

  const mut = useMutation({
    mutationFn: () => api.teams.update(team.id, {
      name: name !== team.name ? name : undefined,
      slug: slug !== team.slug ? slug : undefined,
    }),
    onSuccess: () => {
      toast({ title: 'Team updated' })
      qc.invalidateQueries({ queryKey: ['teams'] })
      onDone()
    },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  return (
    <form onSubmit={e => { e.preventDefault(); mut.mutate() }} className="space-y-3">
      <div><Label>Name</Label><Input value={name} onChange={e => setName(e.target.value)} required /></div>
      <div><Label>Slug</Label><Input value={slug} onChange={e => setSlug(e.target.value)} required /></div>
      <div className="flex gap-2">
        <Button type="submit" disabled={mut.isPending}>{mut.isPending ? 'Saving…' : 'Save Changes'}</Button>
        <Button type="button" variant="outline" onClick={onDone}>Cancel</Button>
      </div>
    </form>
  )
}

// ── Team card ──────────────────────────────────────────────────────────────────
function TeamCard({ team }: { team: Team }) {
  const qc = useQueryClient()
  const [expanded, setExpanded] = useState(false)
  const [editOpen, setEditOpen] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)

  // Load projects for this team (RBAC assignment display)
  const { data: projectsData } = useQuery({
    queryKey: ['projects-for-team', team.id],
    queryFn: () => api.projects.list({ team_id: team.id }),
    enabled: expanded,
  })
  const teamProjects = projectsData?.data ?? []

  const deleteMut = useMutation({
    mutationFn: () => api.teams.delete(team.id),
    onSuccess: () => {
      toast({ title: 'Team deleted', description: team.name })
      qc.invalidateQueries({ queryKey: ['teams'] })
      setConfirmDelete(false)
    },
    onError: (e: any) => {
      toast({ title: 'Delete failed', description: e.message, variant: 'destructive' })
      setConfirmDelete(false)
    },
  })

  return (
    <Card>
      <CardContent className="pt-4">
        {editOpen ? (
          <div>
            <p className="font-semibold mb-3">Editing: {team.name}</p>
            <EditTeamForm team={team} onDone={() => setEditOpen(false)} />
          </div>
        ) : confirmDelete ? (
          <div className="flex items-center justify-between gap-4">
            <span className="text-sm text-red-700">
              Delete <strong>{team.name}</strong>? This cannot be undone.
            </span>
            <div className="flex gap-2 shrink-0">
              <Button size="sm" variant="destructive" disabled={deleteMut.isPending}
                onClick={() => deleteMut.mutate()}>
                {deleteMut.isPending ? 'Deleting…' : 'Yes, delete'}
              </Button>
              <Button size="sm" variant="outline" onClick={() => setConfirmDelete(false)}>Cancel</Button>
            </div>
          </div>
        ) : (
          <>
            {/* Header row */}
            <div className="flex items-center justify-between">
              <div>
                <div className="flex items-center gap-2">
                  <p className="font-semibold">{team.name}</p>
                  <span className="text-xs bg-gray-100 text-gray-600 px-2 py-0.5 rounded-full">RBAC only</span>
                </div>
                <p className="text-xs text-muted-foreground mt-0.5">{team.slug}</p>
              </div>
              <div className="flex items-center gap-1">
                <Button variant="ghost" size="sm" onClick={() => setEditOpen(true)} title="Edit">
                  <Pencil className="w-3.5 h-3.5" />
                </Button>
                <Button variant="ghost" size="sm"
                  className="text-red-400 hover:text-red-600"
                  onClick={() => setConfirmDelete(true)} title="Delete">
                  <Trash2 className="w-3.5 h-3.5" />
                </Button>
                <Button variant="ghost" size="sm" onClick={() => setExpanded(e => !e)}>
                  {expanded ? <ChevronUp className="w-4 h-4" /> : <ChevronDown className="w-4 h-4" />}
                </Button>
              </div>
            </div>

            {/* Expanded details */}
            {expanded && (
              <div className="mt-3 border-t pt-3 space-y-4">
                {/* Model access — grant/revoke */}
                <ModelAccessPanel team={team} />

                {/* Project assignments (read-only view) */}
                <div>
                  <p className="text-xs font-semibold text-muted-foreground uppercase tracking-wide flex items-center gap-1.5 mb-2">
                    <FolderKanban className="w-3.5 h-3.5" />Project Assignments
                  </p>
                  <p className="text-xs text-muted-foreground mb-2">
                    Projects using this team for RBAC grouping.
                  </p>
                  {teamProjects.length === 0 ? (
                    <p className="text-xs text-muted-foreground italic">No projects assigned yet.</p>
                  ) : (
                    <div className="flex flex-wrap gap-1.5">
                      {teamProjects.map(p => (
                        <Link key={p.id} href={`/projects/${p.id}`}
                          className="inline-flex items-center gap-1 text-xs bg-blue-50 text-blue-700 border border-blue-200 rounded-full px-2.5 py-0.5 hover:bg-blue-100 transition-colors">
                          <FolderKanban className="w-3 h-3" />
                          {p.name}
                          <span className="text-blue-400 text-[10px]">(w:{p.priority_weight})</span>
                        </Link>
                      ))}
                    </div>
                  )}
                  <Link href="/projects" className="text-xs text-blue-600 hover:underline mt-1.5 inline-block">
                    Manage projects →
                  </Link>
                </div>
              </div>
            )}
          </>
        )}
      </CardContent>
    </Card>
  )
}

// ── Create team form ───────────────────────────────────────────────────────────
function CreateTeamForm({ onDone }: { onDone: () => void }) {
  const { data: orgs } = useQuery({ queryKey: ['orgs'], queryFn: api.orgs.list })
  const [form, setForm] = useState({ org_id: '', name: '', slug: '' })
  const mut = useMutation({
    mutationFn: () => api.teams.create({
      org_id: form.org_id, name: form.name, slug: form.slug,
    }),
    onSuccess: () => { toast({ title: 'Team created', description: form.name }); onDone() },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })
  const set = (k: string) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm(p => ({ ...p, [k]: e.target.value }))

  return (
    <form onSubmit={e => { e.preventDefault(); mut.mutate() }} className="space-y-3">
      <div>
        <Label>Organization *</Label>
        <select className="w-full border rounded-md h-9 px-3 text-sm mt-1"
          value={form.org_id} onChange={set('org_id')} required>
          <option value="">Select org…</option>
          {(orgs?.data ?? []).map(o => <option key={o.id} value={o.id}>{o.name}</option>)}
        </select>
      </div>
      <div><Label>Team name *</Label><Input value={form.name} onChange={set('name')} required /></div>
      <div><Label>Slug *</Label><Input value={form.slug} onChange={set('slug')} placeholder="my-team" required /></div>
      <p className="text-xs text-muted-foreground">
        Teams are for RBAC and membership grouping only. Rate limits, quotas and priority are configured per Project.
      </p>
      <Button type="submit" disabled={mut.isPending} className="w-full">
        {mut.isPending ? 'Creating…' : 'Create Team'}
      </Button>
    </form>
  )
}

// ── Main page ──────────────────────────────────────────────────────────────────
export default function TeamsPage() {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const { data, isLoading } = useQuery({
    queryKey: ['teams'],
    queryFn:  () => api.teams.list(),
    refetchInterval: 30_000,
  })

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold">Teams</h1>
          <p className="text-sm text-muted-foreground mt-0.5">
            Membership and RBAC grouping — rate limits and quotas belong to Projects
          </p>
        </div>
        <Dialog open={open} onOpenChange={setOpen}>
          <DialogTrigger asChild>
            <Button><Plus className="w-4 h-4 mr-1" />New Team</Button>
          </DialogTrigger>
          <DialogContent>
            <DialogHeader><DialogTitle>Create Team</DialogTitle></DialogHeader>
            <CreateTeamForm onDone={() => { setOpen(false); qc.invalidateQueries({ queryKey: ['teams'] }) }} />
          </DialogContent>
        </Dialog>
      </div>

      {/* Notice */}
      <div className="bg-blue-50 border border-blue-200 rounded-lg p-4 text-sm text-blue-800">
        <p className="font-semibold mb-1">Teams are RBAC/membership only (migration 031)</p>
        <p>All execution concepts (priority, rate limits, quotas, scheduling, usage, billing) have moved to <strong>Projects</strong>. Teams now manage members, roles and model access permissions.</p>
        <Link href="/projects" className="text-blue-700 underline mt-1 inline-block">
          Manage execution settings in Projects →
        </Link>
      </div>

      {isLoading ? <p className="text-muted-foreground">Loading…</p> : (
        <div className="space-y-3">
          {(data?.data ?? []).map(t => <TeamCard key={t.id} team={t} />)}
          {(data?.data ?? []).length === 0 && (
            <Card>
              <CardContent className="py-8 text-center text-muted-foreground">
                No teams yet. Create one to get started.
              </CardContent>
            </Card>
          )}
        </div>
      )}
    </div>
  )
}
