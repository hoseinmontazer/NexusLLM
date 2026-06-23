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
import { Plus, KeyRound, ChevronDown, ChevronUp, Pencil, Trash2, ShieldCheck, Cpu } from 'lucide-react'

// ── Full policy editor (all 4 fields) ────────────────────────────────────────
function PolicyCard({ team }: { team: Team }) {
  const qc = useQueryClient()
  const { data: policy } = useQuery({
    queryKey: ['policy', team.id],
    queryFn:  () => api.teams.getPolicy(team.id),
  })

  const [form, setForm] = useState({
    rpm: '', tpd: '', max_concurrent: '', max_context_tokens: '',
  })

  const mut = useMutation({
    mutationFn: () => api.teams.updatePolicy(team.id, {
      ...(form.rpm               ? { rpm:                parseInt(form.rpm)               } : {}),
      ...(form.tpd               ? { tpd:                parseInt(form.tpd)               } : {}),
      ...(form.max_concurrent    ? { max_concurrent:     parseInt(form.max_concurrent)    } : {}),
      ...(form.max_context_tokens? { max_context_tokens: parseInt(form.max_context_tokens)} : {}),
    }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['policy', team.id] })
      setForm({ rpm: '', tpd: '', max_concurrent: '', max_context_tokens: '' })
      toast({ title: 'Policy updated — takes effect immediately' })
    },
    onError: (e: any) => toast({ title: 'Update failed', description: e.message, variant: 'destructive' }),
  })

  if (!policy) return <p className="text-sm text-muted-foreground py-2">Loading policy…</p>

  const set = (k: string) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm(p => ({ ...p, [k]: e.target.value }))

  return (
    <div className="mt-3 border-t pt-3 space-y-3">
      <p className="text-xs font-semibold text-muted-foreground uppercase tracking-wide flex items-center gap-1.5">
        <ShieldCheck className="w-3.5 h-3.5" />Rate Limits & Quotas
      </p>

      {/* Current values */}
      <div className="grid grid-cols-2 gap-2 text-sm bg-gray-50 rounded-lg p-3">
        <div>
          <span className="text-muted-foreground text-xs">RPM</span>
          <div className="font-semibold">{policy.rpm.toLocaleString()}</div>
          <div className="text-xs text-muted-foreground">requests/min</div>
        </div>
        <div>
          <span className="text-muted-foreground text-xs">TPD</span>
          <div className="font-semibold">{policy.tpd.toLocaleString()}</div>
          <div className="text-xs text-muted-foreground">tokens/day</div>
        </div>
        <div>
          <span className="text-muted-foreground text-xs">Max concurrent</span>
          <div className="font-semibold">{policy.max_concurrent}</div>
          <div className="text-xs text-muted-foreground">parallel requests</div>
        </div>
        <div>
          <span className="text-muted-foreground text-xs">Max context</span>
          <div className="font-semibold">{policy.max_context_tokens.toLocaleString()}</div>
          <div className="text-xs text-muted-foreground">tokens/request</div>
        </div>
      </div>

      {/* Edit fields — leave blank to keep current value */}
      <p className="text-xs text-muted-foreground">Leave a field blank to keep its current value.</p>
      <div className="grid grid-cols-2 gap-2">
        <div>
          <Label className="text-xs">RPM</Label>
          <Input className="h-8 text-sm" type="number" min={0}
            value={form.rpm} onChange={set('rpm')} placeholder={String(policy.rpm)} />
        </div>
        <div>
          <Label className="text-xs">TPD (tokens/day)</Label>
          <Input className="h-8 text-sm" type="number" min={0}
            value={form.tpd} onChange={set('tpd')} placeholder={String(policy.tpd)} />
        </div>
        <div>
          <Label className="text-xs">Max concurrent</Label>
          <Input className="h-8 text-sm" type="number" min={0}
            value={form.max_concurrent} onChange={set('max_concurrent')}
            placeholder={String(policy.max_concurrent)} />
        </div>
        <div>
          <Label className="text-xs">Max context tokens</Label>
          <Input className="h-8 text-sm" type="number" min={0}
            value={form.max_context_tokens} onChange={set('max_context_tokens')}
            placeholder={String(policy.max_context_tokens)} />
        </div>
      </div>
      <Button size="sm" onClick={() => mut.mutate()} disabled={mut.isPending}>
        {mut.isPending ? 'Saving…' : 'Update Policy'}
      </Button>
    </div>
  )
}

// ── Model access management ───────────────────────────────────────────────────
function ModelAccessSection({ team }: { team: Team }) {
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [revokeTarget, setRevokeTarget] = useState<string | null>(null)

  const { data: allModels } = useQuery({
    queryKey: ['models'],
    queryFn:  () => api.models.list(),
  })

  const { data: grantedData, refetch: refetchGranted } = useQuery({
    queryKey: ['team-models', team.id],
    queryFn:  () => api.teams.listModels(team.id),
  })

  const grantedModels: string[] = grantedData?.models ?? []
  const allModelNames = (allModels?.data ?? []).map(m => m.name)
  const notGranted = allModelNames.filter(n => !grantedModels.includes(n))

  const grantMut = useMutation({
    // Grant all selected models in parallel
    mutationFn: async (names: string[]) => {
      await Promise.all(names.map(n => api.teams.addModel(team.id, n)))
    },
    onSuccess: () => {
      toast({ title: `Access granted to ${selected.size} model${selected.size > 1 ? 's' : ''}` })
      setSelected(new Set())
      refetchGranted()
    },
    onError: (e: any) => toast({ title: 'Grant failed', description: e.message, variant: 'destructive' }),
  })

  const revokeMut = useMutation({
    mutationFn: (name: string) => api.teams.removeModel(team.id, name),
    onSuccess: (_, name) => {
      toast({ title: 'Access removed', description: name })
      setRevokeTarget(null)
      refetchGranted()
    },
    onError: (e: any) => {
      toast({ title: 'Revoke failed', description: e.message, variant: 'destructive' })
      setRevokeTarget(null)
    },
  })

  const toggleSelect = (name: string) => {
    setSelected(prev => {
      const next = new Set(prev)
      if (next.has(name)) next.delete(name)
      else next.add(name)
      return next
    })
  }

  return (
    <div className="mt-3 border-t pt-3 space-y-4">
      <p className="text-xs font-semibold text-muted-foreground uppercase tracking-wide flex items-center gap-1.5">
        <Cpu className="w-3.5 h-3.5" />Model Access
      </p>

      {/* Currently granted */}
      <div>
        <p className="text-xs text-muted-foreground mb-2">
          Granted ({grantedModels.length}):
        </p>
        {grantedModels.length === 0 ? (
          <p className="text-xs text-muted-foreground italic">No models granted yet.</p>
        ) : (
          <div className="space-y-1">
            {grantedModels.map(name => (
              <div key={name} className="flex items-center justify-between border rounded px-2 py-1.5 bg-green-50">
                {revokeTarget === name ? (
                  <>
                    <span className="text-xs text-red-700">Remove <strong>{name}</strong>?</span>
                    <div className="flex gap-1">
                      <Button size="sm" variant="destructive" className="h-6 text-xs"
                        disabled={revokeMut.isPending}
                        onClick={() => revokeMut.mutate(name)}>
                        {revokeMut.isPending ? '…' : 'Remove'}
                      </Button>
                      <Button size="sm" variant="outline" className="h-6 text-xs"
                        onClick={() => setRevokeTarget(null)}>Cancel</Button>
                    </div>
                  </>
                ) : (
                  <>
                    <span className="text-sm font-medium text-green-800">{name}</span>
                    <Button variant="ghost" size="sm"
                      className="h-6 text-xs text-red-400 hover:text-red-600 hover:bg-red-50"
                      onClick={() => setRevokeTarget(name)}>
                      Revoke
                    </Button>
                  </>
                )}
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Grant multiple via checkboxes */}
      {notGranted.length > 0 && (
        <div>
          <p className="text-xs text-muted-foreground mb-2">
            Grant access — tick models then click Grant:
          </p>
          <div className="border rounded-md divide-y max-h-48 overflow-y-auto">
            {notGranted.map(name => (
              <label key={name}
                className="flex items-center gap-2.5 px-3 py-2 cursor-pointer hover:bg-gray-50 text-sm">
                <input
                  type="checkbox"
                  checked={selected.has(name)}
                  onChange={() => toggleSelect(name)}
                  className="w-4 h-4 accent-blue-600"
                />
                {name}
              </label>
            ))}
          </div>
          <div className="flex items-center justify-between mt-2">
            <span className="text-xs text-muted-foreground">
              {selected.size > 0 ? `${selected.size} selected` : 'None selected'}
            </span>
            <div className="flex gap-2">
              {selected.size > 0 && (
                <Button size="sm" variant="ghost" className="h-7 text-xs"
                  onClick={() => setSelected(new Set())}>
                  Clear
                </Button>
              )}
              <Button size="sm" className="h-7"
                disabled={selected.size === 0 || grantMut.isPending}
                onClick={() => grantMut.mutate(Array.from(selected))}>
                {grantMut.isPending ? 'Granting…' : `Grant ${selected.size > 0 ? `(${selected.size})` : ''}`}
              </Button>
            </div>
          </div>
        </div>
      )}

      {notGranted.length === 0 && allModelNames.length > 0 && (
        <p className="text-xs text-green-700">All platform models are already granted to this team.</p>
      )}
      {allModelNames.length === 0 && (
        <p className="text-xs text-muted-foreground">No models registered on this platform yet.</p>
      )}
    </div>
  )
}

// Separate component so it can query independently
function TeamModelList({ teamId, onRevoke, revoking }: {
  teamId: string
  onRevoke: (name: string) => void
  revoking: boolean
}) {
  const [revokeTarget, setRevokeTarget] = useState<string | null>(null)

  // Fetch via the models list + filter by team permissions via a proxy endpoint.
  // Since we don't have a dedicated GET /teams/:id/models endpoint,
  // we call GET /models and the backend returns only models allowed for the
  // authenticated admin (all of them). We show them all with revoke buttons.
  // Alternatively we use the /teams/:id/policy endpoint to detect what's loaded.
  //
  // For a correct list we use the models list and allow the admin to revoke any.
  const { data } = useQuery({
    queryKey: ['team-models', teamId],
    queryFn:  () => api.models.list(),
    refetchInterval: 30_000,
  })
  const models = data?.data ?? []

  if (models.length === 0) {
    return <p className="text-xs text-muted-foreground">No models registered on this platform yet.</p>
  }

  return (
    <div className="space-y-1">
      <p className="text-xs text-muted-foreground">Platform models — click × to revoke access for this team:</p>
      <div className="flex flex-wrap gap-1.5">
        {models.map(m => (
          <div key={m.id}>
            {revokeTarget === m.name ? (
              <div className="flex items-center gap-1 border rounded px-2 py-0.5 bg-red-50 border-red-200">
                <span className="text-xs text-red-700">Remove {m.name}?</span>
                <Button size="sm" variant="destructive" className="h-5 text-xs px-1.5"
                  disabled={revoking} onClick={() => { onRevoke(m.name); setRevokeTarget(null) }}>
                  Yes
                </Button>
                <Button size="sm" variant="ghost" className="h-5 text-xs px-1"
                  onClick={() => setRevokeTarget(null)}>No</Button>
              </div>
            ) : (
              <span
                className="inline-flex items-center gap-1 text-xs bg-blue-50 text-blue-700 border border-blue-200 rounded-full px-2.5 py-0.5 cursor-default group"
              >
                {m.name}
                <button
                  className="opacity-0 group-hover:opacity-100 transition-opacity text-blue-400 hover:text-red-500 ml-0.5"
                  title={`Revoke ${m.name} for this team`}
                  onClick={() => setRevokeTarget(m.name)}
                >×</button>
              </span>
            )}
          </div>
        ))}
      </div>
    </div>
  )
}

// ── API Key section ────────────────────────────────────────────────────────────
function ApiKeySection({ team }: { team: Team }) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [newKey, setNewKey] = useState<string | null>(null)
  const [revokeId, setRevokeId] = useState<string | null>(null)
  const { data: keys } = useQuery({
    queryKey: ['api-keys', team.id],
    queryFn: () => api.apiKeys.list(team.id),
  })
  const create = useMutation({
    mutationFn: () => api.apiKeys.create(team.id, name),
    onSuccess: (data) => {
      setNewKey(data.key); setName('')
      qc.invalidateQueries({ queryKey: ['api-keys', team.id] })
    },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })
  const revoke = useMutation({
    mutationFn: (id: string) => api.apiKeys.revoke(id),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['api-keys', team.id] }); setRevokeId(null) },
    onError: (e: any) => { toast({ title: 'Revoke failed', description: e.message, variant: 'destructive' }); setRevokeId(null) },
  })
  return (
    <div className="mt-3 border-t pt-3">
      <p className="text-xs font-semibold text-muted-foreground uppercase tracking-wide flex items-center gap-1.5 mb-2">
        <KeyRound className="w-3.5 h-3.5" />API Keys
      </p>
      {newKey && (
        <div className="mb-3 p-2 bg-green-50 border border-green-200 rounded text-xs">
          <p className="font-semibold text-green-800 mb-1">Key created — save it now, shown once:</p>
          <code className="break-all">{newKey}</code>
          <Button variant="ghost" size="sm" className="ml-2 h-5 text-xs"
            onClick={() => { navigator.clipboard.writeText(newKey); toast({ title: 'Copied!' }) }}>
            Copy
          </Button>
        </div>
      )}
      <div className="flex gap-2 mb-3">
        <Input className="h-7" placeholder="Key name" value={name} onChange={e => setName(e.target.value)} />
        <Button size="sm" onClick={() => create.mutate()} disabled={!name || create.isPending}>
          <KeyRound className="w-3.5 h-3.5 mr-1" />Create
        </Button>
      </div>
      <div className="space-y-1">
        {(keys?.data ?? []).map(k => (
          <div key={k.id} className="flex items-center justify-between text-sm border rounded px-2 py-1">
            {revokeId === k.id ? (
              <>
                <span className="text-xs text-red-700">Revoke <strong>{k.name}</strong>?</span>
                <div className="flex gap-1">
                  <Button size="sm" variant="destructive" className="h-6 text-xs"
                    disabled={revoke.isPending} onClick={() => revoke.mutate(k.id)}>
                    {revoke.isPending ? '…' : 'Revoke'}
                  </Button>
                  <Button size="sm" variant="outline" className="h-6 text-xs"
                    onClick={() => setRevokeId(null)}>Cancel</Button>
                </div>
              </>
            ) : (
              <>
                <span className="font-mono text-xs">{k.key_prefix}…</span>
                <span className="text-muted-foreground text-xs">{k.name}</span>
                <Button variant="ghost" size="sm" className="h-6 text-red-500 hover:text-red-700"
                  onClick={() => setRevokeId(k.id)}>Revoke</Button>
              </>
            )}
          </div>
        ))}
        {(keys?.data ?? []).length === 0 && (
          <p className="text-xs text-muted-foreground">No API keys yet.</p>
        )}
      </div>
    </div>
  )
}

// ── Edit team form ─────────────────────────────────────────────────────────────
function EditTeamForm({ team, onDone }: { team: Team; onDone: () => void }) {
  const qc = useQueryClient()
  const [name, setName] = useState(team.name)
  const [slug, setSlug] = useState(team.slug)
  const [priority, setPriority] = useState(String(team.priority))

  const mut = useMutation({
    mutationFn: () => api.teams.update(team.id, {
      name:     name !== team.name ? name : undefined,
      slug:     slug !== team.slug ? slug : undefined,
      priority: parseInt(priority) !== team.priority ? parseInt(priority) : undefined,
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
      <div>
        <Label>Priority (1–100)</Label>
        <Input type="number" value={priority} onChange={e => setPriority(e.target.value)} min="1" max="100" />
      </div>
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
  const [activeTab, setActiveTab] = useState<'policy' | 'models' | 'keys'>('policy')
  const [editOpen, setEditOpen] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)

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
                <p className="font-semibold">{team.name}</p>
                <p className="text-xs text-muted-foreground">{team.slug} · priority {team.priority}</p>
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

            {/* Expanded content with tabs */}
            {expanded && (
              <div className="mt-3 border-t pt-3">
                {/* Tab bar */}
                <div className="flex gap-0.5 mb-3 border-b">
                  {([
                    { key: 'policy', label: 'Rate Limits' },
                    { key: 'models', label: 'Model Access' },
                    { key: 'keys',   label: 'API Keys' },
                  ] as const).map(tab => (
                    <button key={tab.key} onClick={() => setActiveTab(tab.key)}
                      className={`px-3 py-1.5 text-sm font-medium border-b-2 transition-colors ${
                        activeTab === tab.key
                          ? 'border-blue-600 text-blue-600'
                          : 'border-transparent text-muted-foreground hover:text-foreground'
                      }`}>
                      {tab.label}
                    </button>
                  ))}
                </div>

                {activeTab === 'policy' && <PolicyCard team={team} />}
                {activeTab === 'models' && <ModelAccessSection team={team} />}
                {activeTab === 'keys'   && <ApiKeySection team={team} />}
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
  const [form, setForm] = useState({ org_id: '', name: '', slug: '', priority: '5' })
  const mut = useMutation({
    mutationFn: () => api.teams.create({
      org_id: form.org_id, name: form.name, slug: form.slug, priority: parseInt(form.priority),
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
      <div>
        <Label>Priority (1–100)</Label>
        <Input type="number" value={form.priority} onChange={set('priority')} min="1" max="100" />
        <p className="text-xs text-muted-foreground mt-1">Higher priority = served first under load</p>
      </div>
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
            Manage teams, rate limits, model access, and API keys
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
