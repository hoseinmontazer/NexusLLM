'use client'

import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { BarChart3 } from 'lucide-react'

function fmt(n: number) { return n.toLocaleString() }

export default function UsagePage() {
  const { data: teams } = useQuery({ queryKey: ['teams'], queryFn: () => api.teams.list() })
  const [teamId, setTeamId] = useState('')
  const [from, setFrom]     = useState(() => {
    const d = new Date(); d.setDate(d.getDate() - 30)
    return d.toISOString().split('T')[0]
  })
  const [to, setTo] = useState(() => new Date().toISOString().split('T')[0])

  const { data, isLoading } = useQuery({
    queryKey: ['usage', teamId, from, to],
    queryFn:  () => api.usage.teamDaily(teamId, from, to),
    enabled:  !!teamId,
  })

  const rows = data?.data ?? []
  const totalReqs   = rows.reduce((s, r) => s + r.request_count, 0)
  const totalTokens = rows.reduce((s, r) => s + r.total_tokens, 0)
  const totalCost   = rows.reduce((s, r) => s + r.cost_usd, 0)
  const totalErrors = rows.reduce((s, r) => s + r.error_count, 0)

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold">Usage Analytics</h1>
        <p className="text-sm text-muted-foreground mt-0.5">Token usage, costs, and request history</p>
      </div>

      {/* Filters */}
      <Card>
        <CardContent className="pt-4">
          <div className="grid grid-cols-3 gap-4">
            <div>
              <Label>Team</Label>
              <select className="w-full border rounded-md h-9 px-3 text-sm mt-1"
                value={teamId} onChange={e => setTeamId(e.target.value)}>
                <option value="">Select team…</option>
                {(teams?.data ?? []).map(t => <option key={t.id} value={t.id}>{t.name}</option>)}
              </select>
            </div>
            <div><Label>From</Label><Input type="date" value={from} onChange={e => setFrom(e.target.value)} /></div>
            <div><Label>To</Label><Input type="date" value={to} onChange={e => setTo(e.target.value)} /></div>
          </div>
        </CardContent>
      </Card>

      {teamId && (
        <>
          {/* Summary stats */}
          <div className="grid grid-cols-4 gap-4">
            {[
              { label: 'Total Requests', value: fmt(totalReqs) },
              { label: 'Total Tokens',   value: fmt(totalTokens) },
              { label: 'Total Cost',     value: `$${totalCost.toFixed(4)}` },
              { label: 'Error Count',    value: fmt(totalErrors) },
            ].map(s => (
              <Card key={s.label}>
                <CardContent className="pt-4">
                  <p className="text-xs text-muted-foreground">{s.label}</p>
                  <p className="text-2xl font-bold mt-1">{s.value}</p>
                </CardContent>
              </Card>
            ))}
          </div>

          {/* Daily breakdown table */}
          <Card>
            <CardHeader><CardTitle>Daily Breakdown</CardTitle></CardHeader>
            <CardContent>
              {isLoading ? <p className="text-muted-foreground text-sm">Loading…</p> :
               rows.length === 0 ? (
                <div className="text-center py-8 text-muted-foreground">
                  <BarChart3 className="w-8 h-8 mx-auto mb-2 opacity-30" />
                  <p>No usage data for this period.</p>
                </div>
               ) : (
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b text-muted-foreground">
                      <th className="text-left pb-2">Date</th>
                      <th className="text-left pb-2">Model</th>
                      <th className="text-right pb-2">Requests</th>
                      <th className="text-right pb-2">Errors</th>
                      <th className="text-right pb-2">Prompt Tokens</th>
                      <th className="text-right pb-2">Completion Tokens</th>
                      <th className="text-right pb-2">Cost (USD)</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((r, i) => (
                      <tr key={i} className="border-b last:border-0">
                        <td className="py-2">{r.day}</td>
                        <td className="py-2 font-mono text-xs">{r.model_name}</td>
                        <td className="py-2 text-right">{fmt(r.request_count)}</td>
                        <td className="py-2 text-right">{r.error_count > 0
                          ? <span className="text-red-500">{r.error_count}</span>
                          : <span className="text-green-600">0</span>}
                        </td>
                        <td className="py-2 text-right">{fmt(r.prompt_tokens)}</td>
                        <td className="py-2 text-right">{fmt(r.completion_tokens)}</td>
                        <td className="py-2 text-right font-mono">${r.cost_usd.toFixed(5)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
               )}
            </CardContent>
          </Card>
        </>
      )}
    </div>
  )
}
