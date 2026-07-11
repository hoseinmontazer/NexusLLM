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
import { KeyRound, Copy, Trash2, FolderKanban, Building2 } from 'lucide-react'

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

  // Project-first: select org → project → create key
  const [selectedOrg, setSelectedOrg]         = useState('')
  const [selectedTeam, setSelectedTeam]       = useState('')
  const [selectedProject, setSelectedProject] = useState('')
  const [keyName, setKeyName]                 = useState('')
  const [newKey, setNewKey]                   = useState<string | null>(null)
  const [revokeConfirm, setRevokeConfirm]     = useState<string | null>(null)
  const [expiresAt, setExpiresAt]             = useState('')

  const { data: orgsData }  = useQuery({ queryKey: ['orgs'],  queryFn: () => api.orgs.list() })
  const { data: teamsData } = useQuery({
    queryKey: ['teams', selectedOrg],
    queryFn:  () => api.teams.list(selectedOrg || undefined),
    enabled: !!selectedOrg,
  })
  const { data: projectsData } = useQuery({
    queryKey: ['projects-for-apikey', selectedOrg, selectedTeam],
    queryFn: () => api.projects.list({
      org_id: selectedOrg || undefined,
      team_id: selectedTeam || undefined,
      status: 'active',
    }),
    enabled: !!selectedOrg,
  })

  // List keys scoped to selected team (backend still routes via team)
  const { data: keys, isLoading } = useQuery({
    queryKey: ['api-keys', selectedTeam],
    queryFn:  () => api.apiKeys.list(selectedTeam),
    enabled:  !!selectedTeam,
  })

  const create = useMutation({
    mutationFn: () => api.apiKeys.create(
      selectedTeam,
      keyName,
      expiresAt || undefined,
      selectedProject || undefined,
    ),
    onSuccess: (data: any) => {
      setNewKey(data.key)
      setKeyName('')
      setExpiresAt('')
      qc.invalidateQueries({ queryKey: ['api-keys', selectedTeam] })
      const scopeDesc = data.project_name
        ? `Scoped to project: ${data.project_name} (priority ${data.project_priority_weight})`
        : 'Team-scoped key (inherits team defaults)'
      toast({ title: 'API key created', description: scopeDesc })
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

  const projectList       = projectsData?.data ?? []
  const keyList: ApiKeyRow[] = (keys?.data ?? []) as ApiKeyRow[]
  const selectedProjectObj  = projectList.find(p => p.id === selectedProject)

  const canCreate = !!keyName && !!selectedTeam

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold">API Keys</h1>
        <p className="text-sm text-muted-foreground mt-0.5">
          Keys are issued per project — each key inherits its project&apos;s priority, rate limits and quota.
        </p>
      </div>

      {/* Notice */}
      <div className="bg-blue-50 border border-blue-200 rounded-lg p-3 text-xs text-blue-800">
        <p className="font-semibold mb-0.5">Projects are the execution unit</p>
        <p>Scope each key to a Project to enforce that project&apos;s rate limits, token quotas and scheduling priority. Keys without a project scope fall back to team-level defaults.</p>
      </div>

      {/* Step 1: Select Org */}
      <Card>
        <CardContent className="pt-4 space-y-3">
          <div>
            <Label className="flex items-center gap-1.5"><Building2 className="w-3.5 h-3.5" />Organization *</Label>
            <select
              className="w-full border rounded-md h-9 px-3 text-sm mt-1"
              value={selectedOrg}
              onChange={e => { setSelectedOrg(e.target.value); setSelectedTeam(''); setSelectedProject(''); setNewKey(null) }}
            >
              <option value="">Choose an organization…</option>
              {(orgsData?.data ?? []).map(o => (
                <option key={o.id} value={o.id}>{o.name}</option>
              ))}
            </select>
          </div>

          {selectedOrg && (
            <div>
              <Label>Team * <span className="text-muted-foreground font-normal">(for key ownership)</span></Label>
              <select
                className="w-full border rounded-md h-9 px-3 text-sm mt-1"
                value={selectedTeam}
                onChange={e => { setSelectedTeam(e.target.value); setSelectedProject(''); setNewKey(null) }}
              >
                <option value="">Choose a team…</option>
                {(teamsData?.data ?? []).map(t => (
                  <option key={t.id} value={t.id}>{t.name}</option>
                ))}
              </select>
              <p className="text-xs text-muted-foreground mt-1">Teams own API keys for RBAC purposes. Actual execution settings come from the Project.</p>
            </div>
          )}
        </CardContent>
      </Card>

      {selectedTeam && (
        <>
          {/* Create new key */}
          <Card>
            <CardHeader><CardTitle className="text-base flex items-center gap-2">
              <KeyRound className="w-4 h-4" />Create New API Key
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

              {/* Project scope — primary selection */}
              <div>
                <Label className="flex items-center gap-1.5 mb-1">
                  <FolderKanban className="w-3.5 h-3.5" />
                  Project Scope <span className="text-muted-foreground font-normal text-xs">(recommended — inherits project priority &amp; quota)</span>
                </Label>
                <select
                  className="w-full border rounded-md h-9 px-3 text-sm"
                  value={selectedProject}
                  onChange={e => setSelectedProject(e.target.value)}
                >
                  <option value="">No project scope (team defaults)</option>
                  {projectList.map(p => (
                    <option key={p.id} value={p.id}>
                      {p.name} — priority {p.priority_weight} ({p.priority_label})
                    </option>
                  ))}
                </select>
                {selectedProjectObj ? (
                  <div className="mt-2 flex items-center gap-2 text-xs text-muted-foreground">
                    <FolderKanban className="w-3.5 h-3.5" />
                    Requests will use priority
                    <PriorityBadge weight={selectedProjectObj.priority_weight} label={selectedProjectObj.priority_label} showWeight />
                    {' '}· rate limits from project policy
                  </div>
                ) : (
                  <p className="text-xs text-muted-foreground mt-1">
                    Without a project scope, requests will use team-level defaults. Assign to a project for precise scheduling.
                  </p>
                )}
              </div>

              <div className="grid grid-cols-2 gap-3">
                <div>
                  <Label>Key Name *</Label>
                  <Input
                    placeholder="e.g. chatbot-prod"
                    value={keyName}
                    onChange={e => setKeyName(e.target.value)}
                    className="mt-1"
                  />
                </div>
                <div>
                  <Label>Expiration <span className="text-muted-foreground font-normal">(optional)</span></Label>
                  <Input
                    type="date"
                    value={expiresAt}
                    onChange={e => setExpiresAt(e.target.value)}
                    className="mt-1"
                  />
                </div>
              </div>

              <Button onClick={() => create.mutate()} disabled={!canCreate || create.isPending}>
                <KeyRound className="w-4 h-4 mr-1" />
                {create.isPending ? 'Creating…' : 'Create API Key'}
              </Button>
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
                        <th className="text-left pb-2 font-medium">Project Scope</th>
                        <th className="text-left pb-2 font-medium">Priority</th>
                        <th className="text-left pb-2 font-medium">Last Used</th>
                        <th className="text-left pb-2 font-medium">Expires</th>
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
                              <span className="text-xs text-muted-foreground italic">Team default</span>
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
                          <td className="py-2 text-xs text-muted-foreground">
                            {k.expires_at ? new Date(k.expires_at).toLocaleDateString() : '—'}
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
