import Link from 'next/link'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Cpu, Users, Server, Network, Activity,
  CheckCircle2, AlertTriangle, XCircle, Zap,
  FolderKanban, KeyRound, Building2, BarChart3,
} from 'lucide-react'

const ADMIN = process.env.NEXUS_ADMIN_URL ?? 'http://localhost:8081/admin/v1'

async function fetchJSON(path: string) {
  try {
    const res = await fetch(`${ADMIN}${path}`, { cache: 'no-store' })
    if (!res.ok) return null
    return res.json()
  } catch {
    return null
  }
}

async function getStats() {
  const [nodes, haStatus, models, projects, orgs, apiKeysData] = await Promise.all([
    fetchJSON('/nodes'),
    fetchJSON('/ha/status'),
    fetchJSON('/models'),
    fetchJSON('/projects?status=active'),
    fetchJSON('/orgs'),
    fetchJSON('/projects'), // used to count active keys via project usage
  ])

  const nodeList: any[]    = nodes?.data      ?? []
  const haModels: any[]    = haStatus?.models ?? []
  const modelList: any[]   = models?.data     ?? []
  const projectList: any[] = projects?.data   ?? []
  const orgList: any[]     = orgs?.data       ?? []

  const onlineNodes   = nodeList.filter(n => n.status === 'online' || n.status === 'degraded').length
  const offlineNodes  = nodeList.filter(n => n.status === 'offline').length

  const activeReplicas   = haModels.reduce((s: number, m: any) => s + (m.active_replicas  ?? 0), 0)
  const startingReplicas = haModels.reduce((s: number, m: any) => s + (m.starting_replicas ?? 0), 0)
  const lostReplicas     = haModels.reduce((s: number, m: any) => s + (m.lost_replicas     ?? 0), 0)

  const healthyModels    = haModels.filter((m: any) => m.ha_status === 'healthy').length
  const degradedModels   = haModels.filter((m: any) => m.ha_status === 'degraded').length
  const unavailModels    = haModels.filter((m: any) => m.ha_status === 'unavailable').length

  // Project-level aggregated 24h metrics (from project_runtime_summary view)
  const totalRequests24h = projectList.reduce((s: number, p: any) => s + (p.requests_24h ?? 0), 0)
  const totalTokens24h   = projectList.reduce((s: number, p: any) => s + (p.tokens_24h   ?? 0), 0)
  const totalCost24h     = projectList.reduce((s: number, p: any) => s + (p.cost_usd_24h ?? 0), 0)

  // Priority distribution
  const critProjects = projectList.filter((p: any) => p.priority_weight >= 800).length
  const stdProjects  = projectList.filter((p: any) => p.priority_weight >= 300 && p.priority_weight < 800).length
  const lowProjects  = projectList.filter((p: any) => p.priority_weight < 300).length

  return {
    nodeList,
    onlineNodes, offlineNodes, totalNodes: nodeList.length,
    activeReplicas, startingReplicas, lostReplicas,
    totalModels: modelList.length,
    healthyModels, degradedModels, unavailModels,
    haModels,
    reconcilerLastSweep: haStatus?.reconciler_last_sweep ?? null,
    recoveriesTriggered: haStatus?.recoveries_triggered  ?? 0,
    // Project metrics
    totalProjects: projectList.length,
    totalOrgs: orgList.length,
    projectList,
    totalRequests24h, totalTokens24h, totalCost24h,
    critProjects, stdProjects, lowProjects,
  }
}

function StatCard({
  title, value, sub, icon: Icon, color, href,
}: {
  title: string; value: string | number; sub?: string
  icon: React.ElementType; color: string; href?: string
}) {
  const inner = (
    <Card className="hover:shadow-md transition-shadow cursor-pointer">
      <CardHeader className="flex flex-row items-center justify-between pb-1 pt-4 px-4">
        <CardTitle className="text-xs font-medium text-muted-foreground">{title}</CardTitle>
        <Icon className={`w-4 h-4 ${color}`} />
      </CardHeader>
      <CardContent className="px-4 pb-4">
        <p className="text-3xl font-bold tabular-nums">{value}</p>
        {sub && <p className="text-xs text-muted-foreground mt-0.5">{sub}</p>}
      </CardContent>
    </Card>
  )
  return href ? <Link href={href}>{inner}</Link> : inner
}

function HAStatusBadge({ status }: { status: string }) {
  if (status === 'healthy')     return <span className="flex items-center gap-1 text-green-600 text-xs font-semibold"><CheckCircle2 className="w-3.5 h-3.5" />healthy</span>
  if (status === 'degraded')    return <span className="flex items-center gap-1 text-yellow-600 text-xs font-semibold"><AlertTriangle className="w-3.5 h-3.5" />degraded</span>
  if (status === 'unavailable') return <span className="flex items-center gap-1 text-red-500 text-xs font-semibold"><XCircle className="w-3.5 h-3.5" />unavailable</span>
  return <span className="text-xs text-muted-foreground">{status}</span>
}

function PriorityDot({ weight }: { weight: number }) {
  const color = weight >= 800 ? 'bg-red-500' : weight >= 300 ? 'bg-blue-500' : 'bg-gray-400'
  return <span className={`inline-block w-2 h-2 rounded-full ${color}`} />
}

export default async function DashboardPage() {
  const s = await getStats()

  const fmt = (n: number) => n >= 1_000_000 ? (n / 1_000_000).toFixed(2) + 'M' :
                              n >= 1_000     ? (n / 1_000).toFixed(1) + 'K'    : String(n)

  return (
    <div className="space-y-8">
      {/* Header */}
      <div>
        <h1 className="text-2xl font-bold">Platform Overview</h1>
        <p className="text-muted-foreground mt-1 text-sm">
          NexusLLM — AI Infrastructure · Projects are the execution unit
          {s.reconcilerLastSweep && (
            <span className="ml-3 text-xs text-muted-foreground">
              reconciler: {new Date(s.reconcilerLastSweep).toLocaleTimeString()}
            </span>
          )}
        </p>
      </div>

      {/* Top-level stat cards — project-first */}
      <div className="grid grid-cols-2 sm:grid-cols-3 xl:grid-cols-6 gap-4">
        <StatCard title="Organizations"   value={s.totalOrgs}         sub="active tenants"                                         icon={Building2}     color="text-violet-600"  href="/orgs" />
        <StatCard title="Active Projects" value={s.totalProjects}     sub="execution units"                                        icon={FolderKanban}  color="text-blue-600"    href="/projects" />
        <StatCard title="Running Models"  value={s.activeReplicas}    sub={`${s.startingReplicas} starting`}                       icon={Activity}      color="text-green-600"   href="/runtimes" />
        <StatCard title="Active Requests" value={s.totalRequests24h}  sub="last 24 h across all projects"                         icon={BarChart3}     color="text-teal-600"    href="/usage" />
        <StatCard title="Token Usage 24h" value={fmt(s.totalTokens24h)} sub={s.totalCost24h > 0 ? `$${s.totalCost24h.toFixed(3)} cost` : 'across all projects'} icon={Zap} color="text-amber-500" href="/usage" />
        <StatCard title="Cluster Nodes"   value={s.onlineNodes}       sub={`${s.offlineNodes} offline`}                            icon={Network}       color="text-teal-600"    href="/cluster" />
      </div>

      <div className="grid grid-cols-1 xl:grid-cols-2 gap-6">
        {/* Active Projects with usage */}
        <Card>
          <CardHeader>
            <div className="flex items-center justify-between">
              <div className="flex items-center gap-2">
                <FolderKanban className="w-4 h-4 text-muted-foreground" />
                <CardTitle className="text-base">Active Projects</CardTitle>
              </div>
              <Link href="/projects" className="text-xs text-blue-600 hover:underline">Manage →</Link>
            </div>
          </CardHeader>
          <CardContent>
            {s.projectList.length === 0 ? (
              <div className="text-center py-6 text-muted-foreground">
                <p className="text-sm">No active projects — create one to start routing requests.</p>
              </div>
            ) : (
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-xs text-muted-foreground">
                    <th className="text-left pb-2">Project</th>
                    <th className="text-center pb-2">Priority</th>
                    <th className="text-right pb-2">Runtimes</th>
                    <th className="text-right pb-2">Requests 24h</th>
                    <th className="text-right pb-2">Tokens 24h</th>
                  </tr>
                </thead>
                <tbody>
                  {s.projectList.slice(0, 10).map((p: any) => (
                    <tr key={p.id} className="border-b last:border-0">
                      <td className="py-1.5">
                        <Link href={`/projects/${p.id}`} className="font-medium hover:text-blue-600 flex items-center gap-1.5">
                          <PriorityDot weight={p.priority_weight} />
                          {p.name}
                        </Link>
                      </td>
                      <td className="py-1.5 text-center text-xs text-muted-foreground">{p.priority_weight}</td>
                      <td className="py-1.5 text-right tabular-nums text-xs">{p.runtime_count ?? 0}</td>
                      <td className="py-1.5 text-right tabular-nums text-xs">{(p.requests_24h ?? 0).toLocaleString()}</td>
                      <td className="py-1.5 text-right tabular-nums text-xs">{fmt(p.tokens_24h ?? 0)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            {/* Priority distribution */}
            {s.totalProjects > 0 && (
              <div className="mt-4 pt-3 border-t flex gap-4 text-xs text-muted-foreground">
                <span className="flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-red-500 inline-block" />{s.critProjects} critical (≥800)</span>
                <span className="flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-blue-500 inline-block" />{s.stdProjects} standard (300–799)</span>
                <span className="flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-gray-400 inline-block" />{s.lowProjects} low (&lt;300)</span>
              </div>
            )}
          </CardContent>
        </Card>

        {/* HA Model Status */}
        <Card>
          <CardHeader>
            <div className="flex items-center justify-between">
              <div className="flex items-center gap-2">
                <Activity className="w-4 h-4 text-muted-foreground" />
                <CardTitle className="text-base">Model Replica Health</CardTitle>
              </div>
              <Link href="/ha" className="text-xs text-blue-600 hover:underline">View HA →</Link>
            </div>
          </CardHeader>
          <CardContent>
            {s.haModels.length === 0 ? (
              <div className="text-center py-6 text-muted-foreground">
                <p className="text-sm">No models with HA specs — go to <strong>Models</strong> to deploy.</p>
              </div>
            ) : (
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-xs text-muted-foreground">
                    <th className="text-left pb-2">Model</th>
                    <th className="text-center pb-2">Active</th>
                    <th className="text-center pb-2">Desired</th>
                    <th className="text-left pb-2">HA Status</th>
                  </tr>
                </thead>
                <tbody>
                  {s.haModels.slice(0, 10).map((m: any) => (
                    <tr key={m.model_id} className="border-b last:border-0">
                      <td className="py-1.5 font-medium max-w-[140px] truncate">{m.model_name}</td>
                      <td className="py-1.5 text-center tabular-nums">
                        <span className={m.active_replicas > 0 ? 'text-green-600 font-semibold' : 'text-red-500'}>
                          {m.active_replicas}
                        </span>
                        {m.starting_replicas > 0 && (
                          <span className="text-blue-500 text-xs ml-1">+{m.starting_replicas}</span>
                        )}
                      </td>
                      <td className="py-1.5 text-center tabular-nums text-muted-foreground">{m.desired_replicas}</td>
                      <td className="py-1.5"><HAStatusBadge status={m.ha_status} /></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </CardContent>
        </Card>
      </div>

      {/* Cluster Nodes */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2">
              <Network className="w-4 h-4 text-muted-foreground" />
              <CardTitle className="text-base">Cluster Nodes</CardTitle>
            </div>
            <Link href="/cluster" className="text-xs text-blue-600 hover:underline">Manage →</Link>
          </div>
        </CardHeader>
        <CardContent>
          {s.nodeList.length === 0 ? (
            <div className="text-center py-4 text-muted-foreground">
              <p className="text-sm">No nodes registered — start the node agent on your servers.</p>
            </div>
          ) : (
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b text-xs text-muted-foreground">
                  <th className="text-left pb-2">Host</th>
                  <th className="text-left pb-2">CPU</th>
                  <th className="text-left pb-2">RAM</th>
                  <th className="text-left pb-2">VRAM</th>
                  <th className="text-left pb-2">Status</th>
                </tr>
              </thead>
              <tbody>
                {s.nodeList.map((n: any) => (
                  <tr key={n.id} className="border-b last:border-0">
                    <td className="py-1.5 font-mono text-xs">{n.hostname}</td>
                    <td className="py-1.5 text-xs">{n.total_cpu}</td>
                    <td className="py-1.5 text-xs">{n.total_ram_mb ? `${Math.round(n.total_ram_mb / 1024)}GB` : '—'}</td>
                    <td className="py-1.5 text-xs">{n.total_vram_mb ? `${Math.round(n.total_vram_mb / 1024)}GB` : '—'}</td>
                    <td className="py-1.5">
                      <span className={`text-xs px-1.5 py-0.5 rounded-full font-medium ${
                        n.status === 'online'    ? 'bg-green-100 text-green-700' :
                        n.status === 'degraded'  ? 'bg-yellow-100 text-yellow-700' :
                        n.status === 'offline'   ? 'bg-red-100 text-red-700' :
                        'bg-gray-100 text-gray-600'
                      }`}>{n.status}</span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </CardContent>
      </Card>

      {/* Quick Actions */}
      <Card>
        <CardHeader>
          <div className="flex items-center gap-2">
            <Zap className="w-4 h-4 text-muted-foreground" />
            <CardTitle className="text-base">Quick Actions</CardTitle>
          </div>
        </CardHeader>
        <CardContent>
          <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-3 text-sm">
            <Link href="/projects" className="group flex flex-col gap-1 p-3 rounded-lg border hover:border-blue-300 hover:bg-blue-50/50 transition-colors">
              <span className="font-medium flex items-center gap-1.5"><FolderKanban className="w-4 h-4 text-blue-600" />New Project</span>
              <span className="text-xs text-muted-foreground">Create execution unit with rate limits, quota and priority</span>
            </Link>
            <Link href="/models" className="group flex flex-col gap-1 p-3 rounded-lg border hover:border-blue-300 hover:bg-blue-50/50 transition-colors">
              <span className="font-medium flex items-center gap-1.5"><Cpu className="w-4 h-4 text-blue-600" />Deploy Model</span>
              <span className="text-xs text-muted-foreground">Register or deploy an LLM with HA replicas</span>
            </Link>
            <Link href="/api-keys" className="group flex flex-col gap-1 p-3 rounded-lg border hover:border-blue-300 hover:bg-blue-50/50 transition-colors">
              <span className="font-medium flex items-center gap-1.5"><KeyRound className="w-4 h-4 text-amber-500" />Create API Key</span>
              <span className="text-xs text-muted-foreground">Issue project-scoped keys with automatic priority inheritance</span>
            </Link>
            <Link href="/usage" className="group flex flex-col gap-1 p-3 rounded-lg border hover:border-blue-300 hover:bg-blue-50/50 transition-colors">
              <span className="font-medium flex items-center gap-1.5"><BarChart3 className="w-4 h-4 text-teal-600" />Project Analytics</span>
              <span className="text-xs text-muted-foreground">Token usage, costs, latency per project</span>
            </Link>
          </div>
        </CardContent>
      </Card>
    </div>
  )
}
