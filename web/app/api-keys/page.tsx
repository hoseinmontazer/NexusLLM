'use client'

import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { toast } from '@/components/ui/toaster'
import { PriorityBadge } from '@/components/projects/PriorityBadge'
import { KeyRound, Copy, Trash2, FolderKanban, Users } from 'lucide-react'

// ─── types ────────────────────────────────────────────────────────────────────
interface ApiKeyRow {
  id: string
  team_id: string
  name: string
  key_prefix: string
  active: boolean
  last_used_at?: string
  expires_at?: string
  created_at: string
  project_id?: string
  project_name?: string
  project_priority_weight?: number
}

export default function ApiKeysPage() {
  const qc = useQueryClient()

  // Selectors
  const [selectedTeam, setSelectedTeam]       = useState('')
  const [selectedProject, setSelectedProject] = useState('')
  const [scope, setScope]                     = useState<'team' | 'project'>('team')
  const [keyName, setKeyName]                 = useState('')
  const [newKey, setNewKey]                   = useState<string | null>(null)
  const [revokeConfirm, setRevokeConfirm]     = useState<string | null>(null)

  const { data: teams }    = useQuery({ queryKey: ['teams'],   queryFn: () => api.teams.list() })
  const { data: projects } = useQuery({
    queryKey: ['projects-for-team', selectedTeam],
    queryFn:  () => api.projects.list({ team_id: selectedTeam, status: 'active' }),
    enabled:  !!selectedTeam,
  })

  const { data: keys, isLoading } = useQuery({
    queryKey: ['api-keys', selectedTeam],
    queryFn:  () => api.apiKeys.list(selectedTeam),
    enabled:  !!selectedTeam,
  })

  const create = useMutation({
    mutationFn: () => api.apiKeys.create(
      selectedTeam,
      keyName,
      undefined,
      scope === 'project' && selectedProject ? selectedProject : undefined,
    ),
    onSuccess: (data: any) => {
      setNewKey(data.key)
      setKeyName('')
      setSelectedProject('')
      qc.invalidateQueries({ queryKey: ['api-keys', selectedTeam] })
      toast({ title: 'API key created', description: scope === 'project' ? `Scoped to project: ${data.project_name}` : 'Team-scoped key' })
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
    onError: (e: any) => { toast({ title: 'Revoke failed', description: e.message, variant: 'destructive' }); setRevokeConfirm(null) },
  })

  const projectList = projects?.data ?? []
  const keyList: ApiKeyRow[] = (keys?.data ?? []) as ApiKeyRow[]
  const selectedProjectObj = projectList.find(p => p.id === selectedProject)

  const canCreate = !!keyName && !!selectedTeam && (scope === 'team' || !!selectedProject)

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold">API Keys</h1>
        <p className="text-sm text-muted-foreground mt-0.5">
          Keys can be scoped to a team or to a specific project for precise priority control
        </p>
      </div>

      {/* Team selector */}
      <Card>
        <CardContent className="pt-4">
          <Label>Team</Label>
          <select
            className="w-full border rounded-md h-9 px-3 text-sm mt-1"
            value={selectedTeam}
            onChange={e => { setSelectedTeam(e.target.value); setNewKey(null); setSelectedProject('') }}
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
            <CardHeader><CardTitle className="text-base flex items-center gap-2">
              <KeyRound className="w-4 h-4" />Create New Key
            </CardTitle></CardHeader>
            <CardContent className="space-y-4">
              {newKey && (
                <div className="p-3 bg-green-50 border border-green-200 rounded-md">
                  <p className="text-sm font-semibold text-green-800 mb-1">
                    ✅ Key created — save it now, it won't be shown again
                  </p>
                  <div className="flex items-center gap-2">
                    <code className="text-xs break-all flex-1 bg-white p-2 rounded border">{newKey}</code>
                    <Button size="sm" variant="outline" onClick={() => { navigator.clipboard.writeText(newKey); toast({ title: 'Copied' }) }}>
                      <Copy className="w-3.5 h-3.5" />
                    </Button>
                  </div>
                </div>
              )}

              {/* Scope selector */}
              <div>
                <Label className="text-xs mb-1 block">Key Scope</Label>
                <div className="flex gap-0 border rounded-md overflow-hidden text-sm">
                  <button
                    type="button"
                    onClick={() => { setScope('team'); setSelectedProject('') }}
                    className={`flex items-center gap-2 px-4 py-2 flex-1 justify-center transition-colors ${
                      scope === 'team' ? 'bg-gray-900 text-white' : 'bg-white text-muted-foreground hover:bg-gray-50'
                    }`}
                  >
                    <Users className="w-3.5 h-3.5" />Team
                  </button>
                  <button
                    type="button"
                    onClick={() => setScope('project')}
                    className={`flex items-center gap-2 px-4 py-2 flex-1 justify-center transition-colors ${
                      scope === 'project' ? 'bg-gray-900 text-white' : 'bg-white text-muted-foreground hover:bg-gray-50'
                    }`}
                    disabled={projectList.length === 0}
                  >
                    <FolderKanban className="w-3.5 h-3.5" />Project
                  </button>
                </div>
                {scope === 'team' && (
                  <p className="text-xs text-muted-foreground mt-1">
                    Key inherits the team's default priority. Use project scope for fine-grained scheduling.
                  </p>
                )}
              </div>

              {/* Project picker */}
              {scope === 'project' && (
                <div>
                  <Label className="text-xs">Project *</Label>
                  <select
                    className="w-full border rounded-md h-9 px-3 text-sm mt-1"
                    value={selectedProject}
                    onChange={e => setSelectedProject(e.target.value)}
                  >
                    <option value="">Select project…</option>
                    {projectList.map(p => (
                      <option key={p.id} value={p.id}>
                        {p.name} — priority {p.priority_weight} ({p.priority_label})
                      </option>
                    ))}
                  </select>
                  {selectedProjectObj && (
                    <div className="mt-2 flex items-center gap-2 text-xs text-muted-foreground">
                      <FolderKanban className="w-3.5 h-3.5" />
                      Requests will use priority
                      <PriorityBadge weight={selectedProjectObj.priority_weight} label={selectedProjectObj.priority_label} showWeight />
                    </div>
                  )}
                </div>
              )}

              <div className="flex gap-2">
                <Input
                  placeholder="Key name (e.g. chatbot-prod)"
                  value={keyName}
                  onChange={e => setKeyName(e.target.value)}
                />
                <Button onClick={() => create.mutate()} disabled={!canCreate || create.isPending}>
                  <KeyRound className="w-4 h-4 mr-1" />
                  {create.isPending ? 'Creating…' : 'Create'}
                </Button>
              </div>
            </CardContent>
          </Card>

          {/* Key list */}
          <Card>
            <CardHeader>
              <CardTitle className="text-base">Keys for this team ({keyList.length})</CardTitle>
            </CardHeader>
            <CardContent>
              {isLoading ? (
                <p className="text-sm text-muted-foreground">Loading…</p>
              ) : keyList.length === 0 ? (
                <p className="text-sm text-muted-foreground text-center py-4">No keys yet.</p>
              ) : (
                <div className="overflow-x-auto">
                  <table className="w-full text-sm">
                    <thead>
                      <tr className="border-b text-xs text-muted-foreground">
                        <th className="text-left pb-2 font-medium">Key Name</th>
                        <th className="text-left pb-2 font-medium">Prefix</th>
                        <th className="text-left pb-2 font-medium">Scope</th>
                        <th className="text-left pb-2 font-medium">Priority</th>
                        <th className="text-left pb-2 font-medium">Last Used</th>
                        <th className="text-left pb-2 font-medium">Status</th>
                        <th className="pb-2"></th>
                      </tr>
                    </thead>
                    <tbody>
                      {keyList.map(k => (
                        <tr key={k.id} className="border-b last:border-0">
                          <td className="py-2 font-medium">{k.name}</td>
                          <td className="py-2 font-mono text-xs text-muted-foreground">{k.key_prefix}…</td>
                          <td className="py-2">
                            {k.project_id ? (
                              <span className="flex items-center gap-1 text-xs text-blue-700">
                                <FolderKanban className="w-3 h-3" />
                                {k.project_name || k.project_id.slice(0, 8)}
                              </span>
                            ) : (
                              <span className="flex items-center gap-1 text-xs text-muted-foreground">
                                <Users className="w-3 h-3" />Team
                              </span>
                            )}
                          </td>
                          <td className="py-2">
                            {k.project_priority_weight != null && k.project_priority_weight > 0 ? (
                              <PriorityBadge weight={k.project_priority_weight} showWeight />
                            ) : (
                              <span className="text-xs text-muted-foreground">—</span>
                            )}
                          </td>
                          <td className="py-2 text-xs text-muted-foreground">
                            {k.last_used_at ? new Date(k.last_used_at).toLocaleDateString() : 'never'}
                          </td>
                          <td className="py-2">
                            <span className={`text-xs px-2 py-0.5 rounded-full ${k.active ? 'bg-green-100 text-green-700' : 'bg-red-100 text-red-700'}`}>
                              {k.active ? 'active' : 'revoked'}
                            </span>
                          </td>
                          <td className="py-2">
                            {k.active && (
                              revokeConfirm === k.id ? (
                                <div className="flex gap-1">
                                  <Button size="sm" variant="destructive" className="h-6 text-xs"
                                    disabled={revoke.isPending} onClick={() => revoke.mutate(k.id)}>
                                    {revoke.isPending ? '…' : 'Revoke'}
                                  </Button>
                                  <Button size="sm" variant="outline" className="h-6 text-xs"
                                    onClick={() => setRevokeConfirm(null)}>Cancel</Button>
                                </div>
                              ) : (
                                <Button variant="ghost" size="sm" className="text-red-500 hover:text-red-700 h-7"
                                  onClick={() => setRevokeConfirm(k.id)}>
                                  <Trash2 className="w-3.5 h-3.5" />
                                </Button>
                              )
                            )}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </CardContent>
          </Card>
        </>
      )}
    </div>
  )
}
