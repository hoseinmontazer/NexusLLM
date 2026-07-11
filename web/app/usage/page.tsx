'use client'

import { useState } from 'react'
import { useQuery, useMutation } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { toast } from '@/components/ui/toaster'
import { BarChart3, RefreshCw, Zap, FolderKanban, Building2, TrendingUp, DollarSign, Clock, AlertTriangle } from 'lucide-react'
import { PriorityBadge } from '@/components/projects/PriorityBadge'

function fmt(n: number) { return n.toLocaleString() }
function fmtTokens(n: number) {
  return n >= 1_000_000 ? (n / 1_000_000).toFixed(2) + 'M' :
         n >= 1_000     ? (n / 1_000).toFixed(1) + 'K' : String(n)
}

// ── Per-project summary card ───────────────────────────────────────────────────
function ProjectUsageCard({ project, from, to }: { project: any; from: string; to: string }) {
  const { data: summary } = useQuery({
    queryKey: ['project-usage-summary', project.id, from, to],
    queryFn: () => api.projectPolicy.getSummary(project.id, from ? new Date(from).toISOString() : undefined, to ? new Date(to).toISOString() : undefined),
    staleTime: 60_000,
  })

  return (
    <Card className="hover:shadow-md transition-shadow">
      <CardContent className="pt-4 pb-3">
        <div className="flex items-start justify-between gap-2 mb-3">
          <div>
            <div className="flex items-center gap-2 flex-wrap">
              <span className="font-semibold text-sm">{project.name}</span>
              <PriorityBadge weight={project.priority_weight} label={project.priority_label} showWeight />
            </div>
            <p className="text-xs text-muted-foreground mt-0.5">{project.status}</p>
          </div>
          <FolderKanban className="w-4 h-4 text-muted-foreground shrink-0 mt-0.5" />
        </div>
        {summary ? (
          <div className="grid grid-cols-2 gap-2 text-xs">
            <div className="flex items-center gap-1.5">
              <BarChart3 className="w-3.5 h-3.5 text-muted-foreground" />
              <div>
                <div className="text-muted-foreground">Requests</div>
                <div className="font-semibold">{fmt(summary.request_count)}</div>
              </div>
            </div>
            <div className="flex items-center gap-1.5">
              <TrendingUp className="w-3.5 h-3.5 text-muted-foreground" />
              <div>
                <div className="text-muted-foreground">Tokens</div>
                <div className="font-semibold">{fmtTokens(summary.total_tokens)}</div>
              </div>
            </div>
            <div className="flex items-center gap-1.5">
              <DollarSign className="w-3.5 h-3.5 text-muted-foreground" />
              <div>
                <div className="text-muted-foreground">Cost</div>
                <div className="font-semibold">${summary.cost_usd.toFixed(4)}</div>
              </div>
            </div>
            <div className="flex items-center gap-1.5">
              <Clock className="w-3.5 h-3.5 text-muted-foreground" />
              <div>
                <div className="text-muted-foreground">Avg Latency</div>
                <div className="font-semibold">{summary.avg_latency_ms.toFixed(0)}ms</div>
              </div>
            </div>
            {summary.error_count > 0 && (
              <div className="col-span-2 flex items-center gap-1.5 text-red-600">
                <AlertTriangle className="w-3.5 h-3.5" />
                <span className="font-semibold">{fmt(summary.error_count)} errors</span>
                <span className="text-muted-foreground">
                  ({summary.request_count > 0 ? (summary.error_count / summary.request_count * 100).toFixed(1) : '0'}% error rate)
                </span>
              </div>
            )}
          </div>
        ) : (
          <p className="text-xs text-muted-foreground">Loading…</p>
        )}
      </CardContent>
    </Card>
  )
}

export default function UsagePage() {
  const [viewMode, setViewMode] = useState<'overview' | 'project' | 'org'>('overview')
  const [selectedProjectId, setSelectedProjectId] = useState('')
  const [selectedOrgId, setSelectedOrgId]         = useState('')
  const [from, setFrom] = useState(() => {
    const d = new Date(); d.setDate(d.getDate() - 30)
    return d.toISOString().split('T')[0]
  })
  const [to, setTo] = useState(() => new Date().toISOString().split('T')[0])

  const { data: projectsData, refetch: refetchProjects } = useQuery({
    queryKey: ['projects-usage-list', selectedOrgId],
    queryFn: () => api.projects.list({
      org_id: selectedOrgId || undefined,
      status: 'active',
    }),
    refetchInterval: 60_000,
  })
  const { data: orgsData } = useQuery({ queryKey: ['orgs'], queryFn: api.orgs.list })

  // Per-project detailed view
  const { data: projectDetail, isLoading: detailLoading, refetch: refetchDetail } = useQuery({
    queryKey: ['project-daily-usage', selectedProjectId, from, to],
    queryFn: () => api.projectPolicy.getDailyUsage(selectedProjectId, from ? new Date(from).toISOString() : undefined, to ? new Date(to).toISOString() : undefined),
    enabled: !!selectedProjectId && viewMode === 'project',
    refetchInterval: 60_000,
  })

  // Org usage (billing rollup)
  const { data: orgUsageData, isLoading: orgLoading, refetch: refetchOrg } = useQuery({
    queryKey: ['org-daily-usage', selectedOrgId, from, to],
    queryFn: () => api.usage.orgMonthlySpend(selectedOrgId),
    enabled: !!selectedOrgId && viewMode === 'org',
    refetchInterval: 60_000,
  })

  const aggregate = useMutation({
    mutationFn: () => fetch('/api/admin/usage/aggregate', { method: 'POST' }).then(r => r.json()),
    onSuccess: () => { toast({ title: 'Aggregation triggered' }); refetchProjects() },
  })

  const projects = projectsData?.data ?? []

  // Overview: all projects sorted by requests_24h
  const sortedProjects = [...projects].sort((a: any, b: any) => (b.requests_24h ?? 0) - (a.requests_24h ?? 0))

  const dailyRows = projectDetail?.data ?? []
  const selectedProject = projects.find((p: any) => p.id === selectedProjectId)

  // Totals from daily breakdown
  const totalReqs     = dailyRows.reduce((s, r) => s + r.request_count, 0)
  const totalTokens   = dailyRows.reduce((s, r) => s + r.total_tokens, 0)
  const totalCost     = dailyRows.reduce((s, r) => s + r.cost_usd, 0)
  const totalErrors   = dailyRows.reduce((s, r) => s + r.error_count, 0)
  const avgLatency    = dailyRows.length > 0 ? dailyRows.reduce((s, r) => s + r.avg_latency_ms, 0) / dailyRows.length : 0

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold">Usage Analytics</h1>
          <p className="text-sm text-muted-foreground mt-0.5">
            Project-first token usage, costs, and request metrics
          </p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" size="sm" onClick={() => { refetchProjects(); refetchDetail(); refetchOrg() }}>
            <RefreshCw className="w-3.5 h-3.5 mr-1" />Refresh
          </Button>
          <Button variant="outline" size="sm" onClick={() => aggregate.mutate()}
            disabled={aggregate.isPending}>
            <Zap className="w-3.5 h-3.5 mr-1" />
            {aggregate.isPending ? 'Running…' : 'Run Aggregation'}
          </Button>
        </div>
      </div>

      {/* View mode tabs */}
      <div className="flex gap-0 border-b">
        {([
          { key: 'overview', label: 'All Projects Overview' },
          { key: 'project',  label: 'Project Deep-Dive' },
          { key: 'org',      label: 'Org Billing' },
        ] as const).map(t => (
          <button key={t.key}
            onClick={() => setViewMode(t.key)}
            className={`px-4 py-2.5 text-sm font-medium border-b-2 transition-colors ${
              viewMode === t.key
                ? 'border-blue-600 text-blue-600'
                : 'border-transparent text-muted-foreground hover:text-foreground'
            }`}>
            {t.label}
          </button>
        ))}
      </div>

      {/* Filters */}
      <Card>
        <CardContent className="pt-4">
          <div className="grid grid-cols-1 sm:grid-cols-4 gap-4">
            <div>
              <Label className="flex items-center gap-1.5"><Building2 className="w-3.5 h-3.5" />Organization</Label>
              <select className="w-full border rounded-md h-9 px-3 text-sm mt-1"
                value={selectedOrgId} onChange={e => { setSelectedOrgId(e.target.value); setSelectedProjectId('') }}>
                <option value="">All organizations</option>
                {(orgsData?.data ?? []).map(o => (
                  <option key={o.id} value={o.id}>{o.name}</option>
                ))}
              </select>
            </div>
            {viewMode === 'project' && (
              <div>
                <Label className="flex items-center gap-1.5"><FolderKanban className="w-3.5 h-3.5" />Project *</Label>
                <select className="w-full border rounded-md h-9 px-3 text-sm mt-1"
                  value={selectedProjectId} onChange={e => setSelectedProjectId(e.target.value)}>
                  <option value="">Select a project…</option>
                  {projects.map((p: any) => (
                    <option key={p.id} value={p.id}>{p.name} (w:{p.priority_weight})</option>
                  ))}
                </select>
              </div>
            )}
            <div>
              <Label>From</Label>
              <Input type="date" value={from} onChange={e => setFrom(e.target.value)} className="mt-1" />
            </div>
            <div>
              <Label>To</Label>
              <Input type="date" value={to} onChange={e => setTo(e.target.value)} className="mt-1" />
            </div>
          </div>
        </CardContent>
      </Card>

      {/* ── OVERVIEW: all projects ── */}
      {viewMode === 'overview' && (
        <div className="space-y-4">
          {sortedProjects.length === 0 ? (
            <Card>
              <CardContent className="py-12 text-center text-muted-foreground">
                <FolderKanban className="w-8 h-8 mx-auto mb-2 opacity-30" />
                <p>No active projects found.</p>
              </CardContent>
            </Card>
          ) : (
            <>
              {/* Summary totals across all projects (24h from view) */}
              <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
                {[
                  { label: 'Projects',        value: projects.length.toString() },
                  { label: 'Requests 24h',    value: fmt(projects.reduce((s: number, p: any) => s + (p.requests_24h ?? 0), 0)) },
                  { label: 'Tokens 24h',      value: fmtTokens(projects.reduce((s: number, p: any) => s + (p.tokens_24h ?? 0), 0)) },
                  { label: 'Cost 24h (USD)',  value: '$' + projects.reduce((s: number, p: any) => s + (p.cost_usd_24h ?? 0), 0).toFixed(4) },
                ].map(s => (
                  <Card key={s.label}>
                    <CardContent className="pt-4 pb-4">
                      <p className="text-xs text-muted-foreground">{s.label}</p>
                      <p className="text-xl font-bold mt-0.5 tabular-nums">{s.value}</p>
                    </CardContent>
                  </Card>
                ))}
              </div>

              {/* Grid of project cards */}
              <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
                {sortedProjects.map((p: any) => (
                  <ProjectUsageCard key={p.id} project={p} from={from} to={to} />
                ))}
              </div>
            </>
          )}
        </div>
      )}

      {/* ── PROJECT DEEP-DIVE ── */}
      {viewMode === 'project' && (
        <div className="space-y-4">
          {!selectedProjectId ? (
            <Card>
              <CardContent className="py-12 text-center text-muted-foreground">
                <FolderKanban className="w-8 h-8 mx-auto mb-2 opacity-30" />
                <p>Select a project above to view detailed analytics.</p>
              </CardContent>
            </Card>
          ) : (
            <>
              {/* Project header */}
              {selectedProject && (
                <div className="flex items-center gap-3 flex-wrap">
                  <FolderKanban className="w-5 h-5 text-blue-600" />
                  <span className="font-semibold text-lg">{selectedProject.name}</span>
                  <PriorityBadge weight={selectedProject.priority_weight} label={selectedProject.priority_label} showWeight />
                  <span className="text-xs text-muted-foreground">{from} → {to}</span>
                </div>
              )}

              {/* Summary stats */}
              {dailyRows.length > 0 && (
                <div className="grid grid-cols-2 sm:grid-cols-5 gap-3">
                  {[
                    { label: 'Requests',     value: fmt(totalReqs) },
                    { label: 'Total Tokens', value: fmtTokens(totalTokens) },
                    { label: 'Prompt',       value: fmtTokens(dailyRows.reduce((s, r) => s + r.prompt_tokens, 0)) },
                    { label: 'Completion',   value: fmtTokens(dailyRows.reduce((s, r) => s + r.completion_tokens, 0)) },
                    { label: 'Cost (USD)',   value: `$${totalCost.toFixed(5)}` },
                  ].map(s => (
                    <Card key={s.label}>
                      <CardContent className="pt-4 pb-4">
                        <p className="text-xs text-muted-foreground">{s.label}</p>
                        <p className="text-xl font-bold mt-0.5 tabular-nums">{s.value}</p>
                      </CardContent>
                    </Card>
                  ))}
                </div>
              )}
              <div className="grid grid-cols-2 gap-3">
                <Card>
                  <CardContent className="pt-4 pb-4">
                    <p className="text-xs text-muted-foreground">Errors</p>
                    <p className={`text-xl font-bold mt-0.5 ${totalErrors > 0 ? 'text-red-600' : 'text-green-600'}`}>
                      {fmt(totalErrors)}
                    </p>
                  </CardContent>
                </Card>
                <Card>
                  <CardContent className="pt-4 pb-4">
                    <p className="text-xs text-muted-foreground">Avg latency</p>
                    <p className="text-xl font-bold mt-0.5 tabular-nums">{avgLatency.toFixed(0)} ms</p>
                  </CardContent>
                </Card>
              </div>

              {/* Daily breakdown */}
              <Card>
                <CardHeader>
                  <CardTitle className="text-base flex items-center gap-2">
                    <BarChart3 className="w-4 h-4" />Daily Breakdown by Model
                  </CardTitle>
                </CardHeader>
                <CardContent>
                  {detailLoading ? (
                    <p className="text-muted-foreground text-sm">Loading…</p>
                  ) : dailyRows.length === 0 ? (
                    <div className="text-center py-8 text-muted-foreground">
                      <BarChart3 className="w-8 h-8 mx-auto mb-2 opacity-30" />
                      <p className="text-sm">No requests in this date range for this project.</p>
                    </div>
                  ) : (
                    <div className="overflow-x-auto">
                      <table className="w-full text-sm">
                        <thead>
                          <tr className="border-b text-xs text-muted-foreground">
                            <th className="text-left pb-2">Date</th>
                            <th className="text-left pb-2">Model</th>
                            <th className="text-right pb-2">Requests</th>
                            <th className="text-right pb-2">Errors</th>
                            <th className="text-right pb-2">Tokens</th>
                            <th className="text-right pb-2">Cost</th>
                            <th className="text-right pb-2">Latency</th>
                          </tr>
                        </thead>
                        <tbody>
                          {dailyRows.map((r, i) => (
                            <tr key={i} className="border-b last:border-0 hover:bg-gray-50">
                              <td className="py-2 text-xs">{r.day}</td>
                              <td className="py-2 font-mono text-xs">{r.model_name || '—'}</td>
                              <td className="py-2 text-right tabular-nums">{fmt(r.request_count)}</td>
                              <td className="py-2 text-right tabular-nums">
                                {r.error_count > 0
                                  ? <span className="text-red-500">{fmt(r.error_count)}</span>
                                  : <span className="text-green-600">0</span>}
                              </td>
                              <td className="py-2 text-right tabular-nums text-xs">{fmtTokens(r.total_tokens)}</td>
                              <td className="py-2 text-right tabular-nums font-mono text-xs">${r.cost_usd.toFixed(5)}</td>
                              <td className="py-2 text-right tabular-nums text-xs">{r.avg_latency_ms.toFixed(0)}ms</td>
                            </tr>
                          ))}
                          {dailyRows.length > 1 && (
                            <tr className="border-t-2 font-semibold bg-gray-50">
                              <td className="py-2 text-xs">Total</td>
                              <td className="py-2 text-xs text-muted-foreground">—</td>
                              <td className="py-2 text-right tabular-nums">{fmt(totalReqs)}</td>
                              <td className="py-2 text-right tabular-nums">{fmt(totalErrors)}</td>
                              <td className="py-2 text-right tabular-nums text-xs">{fmtTokens(totalTokens)}</td>
                              <td className="py-2 text-right tabular-nums font-mono text-xs">${totalCost.toFixed(5)}</td>
                              <td className="py-2 text-right tabular-nums text-xs">{avgLatency.toFixed(0)}ms</td>
                            </tr>
                          )}
                        </tbody>
                      </table>
                    </div>
                  )}
                </CardContent>
              </Card>
            </>
          )}
        </div>
      )}

      {/* ── ORG BILLING ── */}
      {viewMode === 'org' && (
        <div className="space-y-4">
          {!selectedOrgId ? (
            <Card>
              <CardContent className="py-12 text-center text-muted-foreground">
                <Building2 className="w-8 h-8 mx-auto mb-2 opacity-30" />
                <p>Select an organization above to view billing data.</p>
              </CardContent>
            </Card>
          ) : (
            <Card>
              <CardHeader>
                <CardTitle className="text-base flex items-center gap-2">
                  <Building2 className="w-4 h-4" />Monthly Spend
                </CardTitle>
              </CardHeader>
              <CardContent>
                {orgLoading ? (
                  <p className="text-sm text-muted-foreground">Loading…</p>
                ) : (
                  <div>
                    <p className="text-3xl font-bold tabular-nums">
                      ${orgUsageData?.monthly_spend_usd?.toFixed(4) ?? '0.0000'}
                    </p>
                    <p className="text-xs text-muted-foreground mt-1">This month&apos;s total spend across all projects</p>
                    <p className="text-xs text-muted-foreground mt-3">
                      For detailed per-project breakdown, switch to <strong>Project Deep-Dive</strong> mode.
                    </p>
                  </div>
                )}
              </CardContent>
            </Card>
          )}
        </div>
      )}
    </div>
  )
}
