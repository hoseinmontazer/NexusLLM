'use client'

import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type Team, type Policy } from '@/lib/api'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'
import { toast } from '@/components/ui/toaster'
import { Plus, KeyRound, ChevronDown, ChevronUp } from 'lucide-react'

function PolicyCard({ team }: { team: Team }) {
  const qc = useQueryClient()
  const { data: policy } = useQuery({
    queryKey: ['policy', team.id],
    queryFn:  () => api.teams.getPolicy(team.id),
  })
  const [rpm, setRpm]     = useState('')
  const [tpd, setTpd]     = useState('')
  const mut = useMutation({
    mutationFn: () => api.teams.updatePolicy(team.id, {
      ...(rpm ? { rpm: parseInt(rpm) } : {}),
      ...(tpd ? { tpd: parseInt(tpd) } : {}),
    }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['policy', team.id] }); toast({ title: 'Policy updated' }) },
  })

  if (!policy) return <p className="text-sm text-muted-foreground">Loading policy…</p>

  return (
    <div className="mt-3 border-t pt-3 space-y-3">
      <div className="grid grid-cols-2 gap-3 text-sm">
        <div><span className="text-muted-foreground">RPM limit:</span> <strong>{policy.rpm}</strong></div>
        <div><span className="text-muted-foreground">Tokens/day:</span> <strong>{policy.tpd.toLocaleString()}</strong></div>
        <div><span className="text-muted-foreground">Max concurrent:</span> <strong>{policy.max_concurrent}</strong></div>
        <div><span className="text-muted-foreground">Max context:</span> <strong>{policy.max_context_tokens.toLocaleString()}</strong></div>
      </div>
      <div className="flex gap-2 items-end">
        <div><Label className="text-xs">New RPM</Label><Input className="h-7 w-24" value={rpm} onChange={e => setRpm(e.target.value)} placeholder={String(policy.rpm)} /></div>
        <div><Label className="text-xs">New TPD</Label><Input className="h-7 w-32" value={tpd} onChange={e => setTpd(e.target.value)} placeholder={String(policy.tpd)} /></div>
        <Button size="sm" onClick={() => mut.mutate()} disabled={mut.isPending}>Save</Button>
      </div>
    </div>
  )
}

function ApiKeySection({ team }: { team: Team }) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [newKey, setNewKey] = useState<string | null>(null)

  const { data: keys } = useQuery({
    queryKey: ['api-keys', team.id],
    queryFn:  () => api.apiKeys.list(team.id),
  })
  const create = useMutation({
    mutationFn: () => api.apiKeys.create(team.id, name),
    onSuccess: (data) => {
      setNewKey(data.key)
      setName('')
      qc.invalidateQueries({ queryKey: ['api-keys', team.id] })
    },
  })
  const revoke = useMutation({
    mutationFn: (id: string) => api.apiKeys.revoke(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['api-keys', team.id] }),
  })

  return (
    <div className="mt-3 border-t pt-3">
      <p className="text-sm font-medium mb-2">API Keys</p>
      {newKey && (
        <div className="mb-3 p-2 bg-green-50 border border-green-200 rounded text-xs">
          <p className="font-semibold text-green-800 mb-1">Key created — save it now, shown once only:</p>
          <code className="break-all">{newKey}</code>
          <Button variant="ghost" size="sm" className="ml-2 h-5 text-xs" onClick={() => { navigator.clipboard.writeText(newKey); toast({ title: 'Copied!' }) }}>Copy</Button>
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
            <span className="font-mono">{k.key_prefix}…</span>
            <span className="text-muted-foreground">{k.name}</span>
            <Button variant="ghost" size="sm" className="h-6 text-red-500 hover:text-red-700"
              onClick={() => { if(confirm('Revoke this key?')) revoke.mutate(k.id) }}>
              Revoke
            </Button>
          </div>
        ))}
      </div>
    </div>
  )
}

function TeamCard({ team }: { team: Team }) {
  const [expanded, setExpanded] = useState(false)
  return (
    <Card>
      <CardContent className="pt-4">
        <div className="flex items-center justify-between">
          <div>
            <p className="font-semibold">{team.name}</p>
            <p className="text-xs text-muted-foreground">{team.slug} · priority {team.priority}</p>
          </div>
          <Button variant="ghost" size="sm" onClick={() => setExpanded(e => !e)}>
            {expanded ? <ChevronUp className="w-4 h-4" /> : <ChevronDown className="w-4 h-4" />}
          </Button>
        </div>
        {expanded && (
          <div className="space-y-2">
            <PolicyCard team={team} />
            <ApiKeySection team={team} />
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function CreateTeamForm({ onDone }: { onDone: () => void }) {
  const { data: orgs } = useQuery({ queryKey: ['orgs'], queryFn: api.orgs.list })
  const [form, setForm] = useState({ org_id: '', name: '', slug: '', priority: '5' })
  const mut = useMutation({ mutationFn: () => api.teams.create({
    org_id: form.org_id, name: form.name, slug: form.slug, priority: parseInt(form.priority),
  })})
  const set = (k: string) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm(p => ({ ...p, [k]: e.target.value }))

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    try { await mut.mutateAsync(); toast({ title: 'Team created' }); onDone() }
    catch(err: any) { toast({ title: 'Error', description: err.message, variant: 'destructive' }) }
  }

  return (
    <form onSubmit={submit} className="space-y-3">
      <div>
        <Label>Organization *</Label>
        <select className="w-full border rounded-md h-9 px-3 text-sm mt-1" value={form.org_id}
          onChange={e => setForm(p => ({ ...p, org_id: e.target.value }))} required>
          <option value="">Select org…</option>
          {(orgs?.data ?? []).map(o => <option key={o.id} value={o.id}>{o.name}</option>)}
        </select>
      </div>
      <div><Label>Team name *</Label><Input value={form.name} onChange={set('name')} required /></div>
      <div><Label>Slug *</Label><Input value={form.slug} onChange={set('slug')} placeholder="team-alpha" required /></div>
      <div><Label>Priority (1–100)</Label><Input type="number" value={form.priority} onChange={set('priority')} min="1" max="100" /></div>
      <Button type="submit" disabled={mut.isPending}>{mut.isPending ? 'Creating…' : 'Create Team'}</Button>
    </form>
  )
}

export default function TeamsPage() {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const { data, isLoading } = useQuery({ queryKey: ['teams'], queryFn: () => api.teams.list(), refetchInterval: 30_000 })

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div><h1 className="text-2xl font-bold">Teams</h1><p className="text-sm text-muted-foreground mt-0.5">Manage teams, policies and API keys</p></div>
        <Dialog open={open} onOpenChange={setOpen}>
          <DialogTrigger asChild><Button><Plus className="w-4 h-4 mr-1" />New Team</Button></DialogTrigger>
          <DialogContent>
            <DialogHeader><DialogTitle>Create Team</DialogTitle></DialogHeader>
            <CreateTeamForm onDone={() => { setOpen(false); qc.invalidateQueries({ queryKey: ['teams'] }) }} />
          </DialogContent>
        </Dialog>
      </div>
      {isLoading ? <p className="text-muted-foreground">Loading…</p> : (
        <div className="space-y-3">
          {(data?.data ?? []).map(t => <TeamCard key={t.id} team={t} />)}
          {(data?.data ?? []).length === 0 && <Card><CardContent className="py-8 text-center text-muted-foreground">No teams yet.</CardContent></Card>}
        </div>
      )}
    </div>
  )
}
