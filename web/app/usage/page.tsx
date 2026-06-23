'use client'

import { useState } from 'react'
import { useQuery, useMutation } from '@tanstack/react-query'
import { api, type UsageSummary } from '@/lib/api'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { toast } from '@/components/ui/toaster'
import { BarChart3, RefreshCw, Zap } from 'lucide-react'

function fmt(n: number) { return n.toLocaleString() }

export default function UsagePage() {
  const { data: teams } = useQuery({ queryKey: ['teams'], queryFn: () => api.teams.list() })
  const [teamId, setTeamId] = useState('')
  const [from, setFrom] = useState(() => {
    const d = new Date(); d.setDate(d.getDate() - 30)
    return d.toISOString().split('T')[0]
  })
  const [to, setTo] = useState(() => new Date().toISOString().split('T')[0])

  const { data, isLoading, refetch } = useQuery({
    queryKey: ['usage', teamId, from, to],
    queryFn:  () => api.usage.teamDaily(teamId, from, to),
    enabled:  !!teamId,
    refetchInterval: 60_000,
  })

  // Trigger manual aggregation (for usage_daily rollup — optional)
  const aggregate = useMutation({
    mutationFn: () => fetch('/api/admin/usage/aggregate', { method: 'POST' }).then(r => r.json()),
    onSuccess: () => { toast({ title: 'Aggregation triggered' }); refetch() },
  })

  const rows: UsageSummary[] = data?.data ?? []
  const totalReqs    = rows.reduce((s, r) => s + r.request_count, 0)
  const totalTokens  = rows.reduce((s, r) => s + r.total_tokens, 0)
  const totalCost    = rows.reduce((s, r) => s + r.cost_usd, 0)
  const totalErrors  = rows.reduce((s, r) => s + r.error_count, 0)
  const avgLatencyMs = rows.length > 0
    ? rows.reduce((s, r) => s + r.avg_latency_ms, 0) / rows.length : 0

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold">Usage Analytics</h1>
          <p className="text-sm text-muted-foreground mt-0.5">
            Real-time token usage, costs, and request history per team
          </p>
        </div>
        <div className="flex gap-2">
          {teamId && (
            <Button variant="outline" size="sm" onClick={() => refetch()}>
              <RefreshCw className="w-3.5 h-3.5 mr-1" />Refresh
            </Button>
          )}
          <Button variant="outline" size="sm" onClick={() => aggregate.mutate()}
            disabled={aggregate.isPending} title="Trigger daily aggregation rollup">
            <Zap className="w-3.5 h-3.5 mr-1" />
            {aggregate.isPending ? 'Running…' : 'Run Aggregation'}
          </Button>
        </div>
      </div>

      {/* Filters */}
      <Card>
        <CardContent className="pt-4">
          <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
            <div>
              <Label>Team *</Label>
              <select className="w-full border rounded-md h-9 px-3 text-sm mt-1"
                value={teamId} onChange={e => setTeamId(e.target.value)}>
                <option value="">Select a team…</option>
                {(teams?.data ?? []).map(t => (
                  <option key={t.id} value={t.id}>{t.name}</option>
                ))}
              </select>
            </div>
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

      {!teamId && (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground">
            <BarChart3 className="w-8 h-8 mx-auto mb-2 opacity-30" />
            <p>Select a team above to view usage data.</p>
          </CardContent>
        </Card>
      )}

      {teamId && (
        <>
          {/* Summary stats */}
          <div className="grid grid-cols-2 sm:grid-cols-5 gap-3">
            {[
              { label: 'Requests',     value: fmt(totalReqs) },
              { label: 'Total Tokens', value: fmt(totalTokens) },
              { label: 'Prompt',       value: fmt(rows.reduce((s, r) => s + r.prompt_tokens, 0)) },
              { label: 'Completion',   value: fmt(rows.reduce((s, r) => s + r.completion_tokens, 0)) },
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

          {/* Secondary stats row */}
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
                <p className="text-xl font-bold mt-0.5 tabular-nums">{avgLatencyMs.toFixed(0)} ms</p>
              </CardContent>
            </Card>
          </div>

          {/* Per-model breakdown */}
          <Card>
            <CardHeader>
              <CardTitle className="text-base flex items-center gap-2">
                <BarChart3 className="w-4 h-4" />
                Per-Model Breakdown
                <span className="text-xs font-normal text-muted-foreground ml-auto">
                  Real-time · {from} → {to}
                </span>
              </CardTitle>
            </CardHeader>
            <CardContent>
              {isLoading ? (
                <p className="text-muted-foreground text-sm">Loading…</p>
              ) : rows.length === 0 ? (
                <div className="text-center py-8 text-muted-foreground">
                  <BarChart3 className="w-8 h-8 mx-auto mb-2 opacity-30" />
                  <p className="text-sm">No requests in this date range.</p>
                  <p className="text-xs mt-1">Try widening the date range or check that the team has made requests.</p>
                </div>
              ) : (
                <div className="overflow-x-auto">
                  <table className="w-full text-sm">
                    <thead>
                      <tr className="border-b text-xs text-muted-foreground">
                        <th className="text-left pb-2">Model</th>
                        <th className="text-right pb-2">Requests</th>
                        <th className="text-right pb-2">Errors</th>
                        <th className="text-right pb-2">Prompt tokens</th>
                        <th className="text-right pb-2">Completion tokens</th>
                        <th className="text-right pb-2">Total tokens</th>
                        <th className="text-right pb-2">Avg latency</th>
                        <th className="text-right pb-2">Cost (USD)</th>
                      </tr>
                    </thead>
                    <tbody>
                      {rows.map((r, i) => (
                        <tr key={i} className="border-b last:border-0 hover:bg-gray-50">
                          <td className="py-2 font-mono text-xs font-medium">{r.model_name}</td>
                          <td className="py-2 text-right tabular-nums">{fmt(r.request_count)}</td>
                          <td className="py-2 text-right tabular-nums">
                            {r.error_count > 0
                              ? <span className="text-red-500 font-medium">{fmt(r.error_count)}</span>
                              : <span className="text-green-600">0</span>}
                          </td>
                          <td className="py-2 text-right tabular-nums text-xs">{fmt(r.prompt_tokens)}</td>
                          <td className="py-2 text-right tabular-nums text-xs">{fmt(r.completion_tokens)}</td>
                          <td className="py-2 text-right tabular-nums font-medium">{fmt(r.total_tokens)}</td>
                          <td className="py-2 text-right tabular-nums text-xs">{r.avg_latency_ms.toFixed(0)} ms</td>
                          <td className="py-2 text-right tabular-nums font-mono text-xs">${r.cost_usd.toFixed(5)}</td>
                        </tr>
                      ))}
                      {/* Totals row */}
                      {rows.length > 1 && (
                        <tr className="border-t-2 font-semibold bg-gray-50">
                          <td className="py-2 text-xs">Total</td>
                          <td className="py-2 text-right tabular-nums">{fmt(totalReqs)}</td>
                          <td className="py-2 text-right tabular-nums">{fmt(totalErrors)}</td>
                          <td className="py-2 text-right tabular-nums text-xs">
                            {fmt(rows.reduce((s, r) => s + r.prompt_tokens, 0))}
                          </td>
                          <td className="py-2 text-right tabular-nums text-xs">
                            {fmt(rows.reduce((s, r) => s + r.completion_tokens, 0))}
                          </td>
                          <td className="py-2 text-right tabular-nums">{fmt(totalTokens)}</td>
                          <td className="py-2 text-right tabular-nums text-xs">{avgLatencyMs.toFixed(0)} ms</td>
                          <td className="py-2 text-right tabular-nums font-mono text-xs">${totalCost.toFixed(5)}</td>
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
  )
}
