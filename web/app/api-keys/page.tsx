'use client'

import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { toast } from '@/components/ui/toaster'
import { KeyRound, Copy, Trash2 } from 'lucide-react'

export default function ApiKeysPage() {
  const qc = useQueryClient()
  const { data: teams } = useQuery({ queryKey: ['teams'], queryFn: () => api.teams.list() })
  const [selectedTeam, setSelectedTeam] = useState('')
  const [keyName, setKeyName] = useState('')
  const [newKey, setNewKey] = useState<string | null>(null)
  const [revokeConfirm, setRevokeConfirm] = useState<string | null>(null)

  const { data: keys, isLoading } = useQuery({
    queryKey: ['api-keys', selectedTeam],
    queryFn:  () => api.apiKeys.list(selectedTeam),
    enabled:  !!selectedTeam,
  })

  const create = useMutation({
    mutationFn: () => api.apiKeys.create(selectedTeam, keyName),
    onSuccess: (data) => {
      setNewKey(data.key)
      setKeyName('')
      qc.invalidateQueries({ queryKey: ['api-keys', selectedTeam] })
      toast({ title: 'API key created' })
    },
    onError: (e: any) => toast({ title: 'Error', description: e.message, variant: 'destructive' }),
  })

  const revoke = useMutation({
    mutationFn: (id: string) => api.apiKeys.revoke(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['api-keys', selectedTeam] })
      setRevokeConfirm(null)
      toast({ title: 'Key revoked' })
    },
    onError: (e: any) => {
      setRevokeConfirm(null)
      toast({ title: 'Revoke failed', description: e.message, variant: 'destructive' })
    },
  })

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold">API Keys</h1>
        <p className="text-sm text-muted-foreground mt-0.5">Create and revoke team API keys</p>
      </div>

      {/* Team selector */}
      <Card>
        <CardContent className="pt-4">
          <Label>Select Team</Label>
          <select
            className="w-full border rounded-md h-9 px-3 text-sm mt-1"
            value={selectedTeam}
            onChange={e => { setSelectedTeam(e.target.value); setNewKey(null) }}
          >
            <option value="">Choose a team…</option>
            {(teams?.data ?? []).map(t => (
              <option key={t.id} value={t.id}>{t.name}</option>
            ))}
          </select>
        </CardContent>
      </Card>

      {selectedTeam && (
        <>
          {/* Create new key */}
          <Card>
            <CardHeader><CardTitle className="text-base">Create New Key</CardTitle></CardHeader>
            <CardContent className="space-y-3">
              {newKey && (
                <div className="p-3 bg-green-50 border border-green-200 rounded-md">
                  <p className="text-sm font-semibold text-green-800 mb-1">
                    ✅ Key created — save it now, it won't be shown again
                  </p>
                  <div className="flex items-center gap-2">
                    <code className="text-xs break-all flex-1 bg-white p-2 rounded border">{newKey}</code>
                    <Button size="sm" variant="outline" onClick={() => {
                      navigator.clipboard.writeText(newKey)
                      toast({ title: 'Copied to clipboard' })
                    }}>
                      <Copy className="w-3.5 h-3.5" />
                    </Button>
                  </div>
                </div>
              )}
              <div className="flex gap-2">
                <Input
                  placeholder="Key name (e.g. my-app-prod)"
                  value={keyName}
                  onChange={e => setKeyName(e.target.value)}
                />
                <Button
                  onClick={() => create.mutate()}
                  disabled={!keyName || create.isPending}
                >
                  <KeyRound className="w-4 h-4 mr-1" />
                  {create.isPending ? 'Creating…' : 'Create'}
                </Button>
              </div>
            </CardContent>
          </Card>

          {/* Key list */}
          <Card>
            <CardHeader><CardTitle className="text-base">Existing Keys</CardTitle></CardHeader>
            <CardContent>
              {isLoading ? (
                <p className="text-sm text-muted-foreground">Loading…</p>
              ) : (
                <div className="space-y-2">
                  {(keys?.data ?? []).map(k => (
                    <div key={k.id} className="flex items-center justify-between border rounded-md px-3 py-2">
                      <div>
                        <p className="text-sm font-medium">{k.name}</p>
                        <p className="text-xs font-mono text-muted-foreground">{k.key_prefix}…</p>
                        {k.last_used_at && (
                          <p className="text-xs text-muted-foreground">
                            Last used: {new Date(k.last_used_at).toLocaleDateString()}
                          </p>
                        )}
                      </div>
                      <div className="flex items-center gap-2">
                        <span className={`text-xs px-2 py-0.5 rounded-full ${
                          k.active ? 'bg-green-100 text-green-700' : 'bg-red-100 text-red-700'
                        }`}>
                          {k.active ? 'active' : 'revoked'}
                        </span>
                        {k.active && (
                          revokeConfirm === k.id ? (
                            <div className="flex gap-1 items-center">
                              <span className="text-xs text-red-700 mr-1">Revoke?</span>
                              <Button size="sm" variant="destructive" className="h-6 text-xs"
                                disabled={revoke.isPending}
                                onClick={() => revoke.mutate(k.id)}>
                                {revoke.isPending ? '…' : 'Yes'}
                              </Button>
                              <Button size="sm" variant="outline" className="h-6 text-xs"
                                onClick={() => setRevokeConfirm(null)}>No</Button>
                            </div>
                          ) : (
                            <Button
                              variant="ghost" size="sm"
                              className="text-red-500 hover:text-red-700"
                              onClick={() => setRevokeConfirm(k.id)}
                            >
                              <Trash2 className="w-3.5 h-3.5" />
                            </Button>
                          )
                        )}
                      </div>
                    </div>
                  ))}
                  {(keys?.data ?? []).length === 0 && (
                    <p className="text-sm text-muted-foreground text-center py-4">No keys yet.</p>
                  )}
                </div>
              )}
            </CardContent>
          </Card>
        </>
      )}
    </div>
  )
}
