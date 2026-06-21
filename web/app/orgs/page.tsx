'use client'

import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'
import { toast } from '@/components/ui/toaster'
import { Plus, Trash2, Building2 } from 'lucide-react'

export default function OrgsPage() {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('')
  const [slug, setSlug] = useState('')

  const { data, isLoading } = useQuery({ queryKey: ['orgs'], queryFn: api.orgs.list })

  const create = useMutation({
    mutationFn: () => api.orgs.create({ name, slug }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['orgs'] })
      toast({ title: 'Organization created', description: name })
      setOpen(false); setName(''); setSlug('')
    },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  const del = useMutation({
    mutationFn: (id: string) => api.orgs.delete(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['orgs'] }),
  })

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold">Organizations</h1>
          <p className="text-sm text-muted-foreground mt-0.5">Top-level tenants</p>
        </div>
        <Dialog open={open} onOpenChange={setOpen}>
          <DialogTrigger asChild>
            <Button><Plus className="w-4 h-4 mr-1" />New Org</Button>
          </DialogTrigger>
          <DialogContent>
            <DialogHeader><DialogTitle>Create Organization</DialogTitle></DialogHeader>
            <form onSubmit={e => { e.preventDefault(); create.mutate() }} className="space-y-3">
              <div><Label>Name *</Label><Input value={name} onChange={e => setName(e.target.value)} placeholder="Acme Corp" required /></div>
              <div><Label>Slug *</Label><Input value={slug} onChange={e => setSlug(e.target.value)} placeholder="acme-corp" required /></div>
              <Button type="submit" disabled={create.isPending}>{create.isPending ? 'Creating…' : 'Create'}</Button>
            </form>
          </DialogContent>
        </Dialog>
      </div>

      {isLoading ? <p className="text-muted-foreground">Loading…</p> : (
        <div className="grid gap-3">
          {(data?.data ?? []).map(org => (
            <Card key={org.id}>
              <CardContent className="flex items-center justify-between pt-4 pb-4">
                <div className="flex items-center gap-3">
                  <Building2 className="w-5 h-5 text-blue-500" />
                  <div>
                    <p className="font-semibold">{org.name}</p>
                    <p className="text-xs text-muted-foreground font-mono">{org.slug}</p>
                  </div>
                  <span className={`text-xs px-2 py-0.5 rounded-full ${org.active ? 'bg-green-100 text-green-700' : 'bg-gray-100 text-gray-500'}`}>
                    {org.active ? 'active' : 'inactive'}
                  </span>
                </div>
                <Button variant="ghost" size="sm" className="text-red-500 hover:text-red-700"
                  onClick={() => { if (confirm(`Delete ${org.name}?`)) del.mutate(org.id) }}>
                  <Trash2 className="w-4 h-4" />
                </Button>
              </CardContent>
            </Card>
          ))}
          {(data?.data ?? []).length === 0 && (
            <Card><CardContent className="py-8 text-center text-muted-foreground">No organizations yet.</CardContent></Card>
          )}
        </div>
      )}
    </div>
  )
}
