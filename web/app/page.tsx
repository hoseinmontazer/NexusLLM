import Link from 'next/link'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Cpu, Users, Server, Network, Activity,
  CheckCircle2, AlertTriangle, XCircle, Zap,
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
  const [nodes, haStatus, models] = await Promise.all([
    fetchJSON('/nodes'),
    fetchJSON('/ha/status'),
    fetchJSON('/models'),
  ])

  const nodeList: any[]  = nodes?.data      ?? []
  const haModels: any[]  = haStatus?.models ?? []
  const modelList: any[] = models?.data     ?? []

  const onlineNodes   = nodeList.filter(n => n.status === 'online' || n.status === 'degraded').length
  const offlineNodes  = nodeList.filter(n => n.status === 'offline').length

  // Runtime counts from HA status
  const activeReplicas   = haModels.reduce((s: number, m: any) => s + (m.active_replicas  ?? 0), 0)
  const startingReplicas = haModels.reduce((s: number, m: any) => s + (m.starting_replicas ?? 0), 0)
  const lostReplicas     = haModels.reduce((s: number, m: any) => s + (m.lost_replicas     ?? 0), 0)

  const healthyModels    = haModels.filter((m: any) => m.ha_status === 'healthy').length
  const degradedModels   = haModels.filter((m: any) => m.ha_status === 'degraded').length
  const unavailModels    = haModels.filter((m: any) => m.ha_status === 'unavailable').length

  return {
    nodeList,
    onlineNodes,
    offlineNodes,
    totalNodes: nodeList.length,
    activeReplicas,
    startingReplicas,
    lostReplicas,
    totalModels:   modelList.length,
    healthyModels,
    degradedModels,
    unavailModels,
    haModels,
    reconcilerLastSweep: haStatus?.reconciler_last_sweep ?? null,
    recoveriesTriggered: haStatus?.recoveries_triggered  ?? 0,
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

export default async function DashboardPage() {
  const s = await getStats()

  return (
    <div className="space-y-8">
      {/* Header */}
      <div>
        <h1 className="text-2xl font-bold">Cluster Overview</h1>
        <p className="text-muted-foreground mt-1 text-sm">
          NexusLLM — AI Infrastructure Platform
          {s.reconcilerLastSweep && (
            <span className="ml-3 text-xs text-muted-foreground">
              reconciler: {new Date(s.reconcilerLastSweep).toLocaleTimeString()}
            </span>
          )}
        </p>
      </div>

      {/* Top-level stat cards */}
      <div className="grid grid-cols-2 sm:grid-cols-3 xl:grid-cols-6 gap-4">
        <StatCard title="Online Nodes"      value={s.onlineNodes}       sub={`${s.offlineNodes} offline`}                           icon={Network}       color="text-teal-600"   href="/cluster" />
        <StatCard title="Active Runtimes"   value={s.activeReplicas}    sub={`${s.startingReplicas} starting`}                      icon={Activity}      color="text-green-600"  href="/runtimes" />
        <StatCard title="Failed Runtimes"   value={s.lostReplicas}      sub={s.lostReplicas > 0 ? 'needs recovery' : 'all clear'}   icon={Cpu}           color={s.lostReplicas > 0 ? 'text-red-500' : 'text-gray-400'} href="/runtimes" />
        <StatCard title="Healthy Models"    value={s.healthyModels}     sub={`${s.degradedModels} degraded`}                        icon={CheckCircle2}  color="text-green-600"  href="/models" />
        <StatCard title="Degraded"          value={s.degradedModels}    sub={`${s.unavailModels} unavailable`}                      icon={AlertTriangle} color={s.degradedModels > 0 ? 'text-yellow-600' : 'text-gray-400'} href="/ha" />
        <StatCard title="Auto Recoveries"   value={s.recoveriesTriggered} sub="total triggered"                                     icon={Zap}           color="text-blue-500"   href="/ha" />
      </div>

      <div className="grid grid-cols-1 xl:grid-cols-2 gap-6">
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
                  {s.haModels.slice(0, 12).map((m: any) => (
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
              <div className="text-center py-6 text-muted-foreground">
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
      </div>

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
            <Link href="/models" className="flex flex-col gap-1 p-3 rounded-lg border hover:bg-gray-50 transition-colors">
              <span className="font-medium">🦙 Deploy Model</span>
              <span className="text-xs text-muted-foreground">Register or deploy an LLM with HA replicas</span>
            </Link>
            <Link href="/ha" className="flex flex-col gap-1 p-3 rounded-lg border hover:bg-gray-50 transition-colors">
              <span className="font-medium">🔁 HA & Failover</span>
              <span className="text-xs text-muted-foreground">Configure replicas, recovery policy, placement</span>
            </Link>
            <Link href="/runtimes" className="flex flex-col gap-1 p-3 rounded-lg border hover:bg-gray-50 transition-colors">
              <span className="font-medium">⚡ Runtime Status</span>
              <span className="text-xs text-muted-foreground">View all containers across every node</span>
            </Link>
            <Link href="/cluster" className="flex flex-col gap-1 p-3 rounded-lg border hover:bg-gray-50 transition-colors">
              <span className="font-medium">🖥 Cluster Nodes</span>
              <span className="text-xs text-muted-foreground">GPU inventory, placement, resource usage</span>
            </Link>
          </div>
        </CardContent>
      </Card>
    </div>
  )
}
