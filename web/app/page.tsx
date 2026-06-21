import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { api } from '@/lib/api'
import { Cpu, Users, Server, Box, Network, Activity } from 'lucide-react'

async function getStats() {
  const [orgs, teams, models, services, nodes, gpuNodes] = await Promise.allSettled([
    api.orgs.list(),
    api.teams.list(),
    api.models.list(),
    api.services.list(),
    api.nodes.list(),
    api.gpu.listNodes(),
  ])
  return {
    orgs:      orgs.status     === 'fulfilled' ? orgs.value.total      : 0,
    teams:     teams.status    === 'fulfilled' ? teams.value.total     : 0,
    models:    models.status   === 'fulfilled' ? models.value.total    : 0,
    services:  services.status === 'fulfilled' ? services.value.total  : 0,
    nodes:     nodes.status    === 'fulfilled' ? nodes.value.total     : 0,
    gpuNodes:  gpuNodes.status === 'fulfilled' ? gpuNodes.value.total  : 0,
    modelList: models.status   === 'fulfilled' ? models.value.data     : [],
    nodeList:  nodes.status    === 'fulfilled' ? nodes.value.data      : [],
  }
}

const SERVICE_TYPE_COLORS: Record<string, string> = {
  CHAT:      'bg-blue-100 text-blue-700',
  EMBEDDING: 'bg-purple-100 text-purple-700',
  RERANK:    'bg-indigo-100 text-indigo-700',
  STT:       'bg-teal-100 text-teal-700',
  TTS:       'bg-cyan-100 text-cyan-700',
  OCR:       'bg-orange-100 text-orange-700',
  AGENT:     'bg-pink-100 text-pink-700',
  MCP:       'bg-gray-100 text-gray-700',
}

export default async function DashboardPage() {
  const stats = await getStats()

  const statCards = [
    { title: 'Organizations', value: stats.orgs,     icon: Users,   color: 'text-blue-600' },
    { title: 'Teams',         value: stats.teams,    icon: Users,   color: 'text-purple-600' },
    { title: 'LLM Models',    value: stats.models,   icon: Cpu,     color: 'text-green-600' },
    { title: 'AI Services',   value: stats.services, icon: Box,     color: 'text-pink-600' },
    { title: 'Cluster Nodes', value: stats.nodes,    icon: Network, color: 'text-teal-600' },
    { title: 'GPU Nodes',     value: stats.gpuNodes, icon: Server,  color: 'text-orange-600' },
  ]

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-bold">Dashboard</h1>
        <p className="text-muted-foreground mt-1">NexusLLM — AI Resource Orchestrator</p>
      </div>

      {/* Stat cards */}
      <div className="grid grid-cols-2 lg:grid-cols-3 xl:grid-cols-6 gap-4">
        {statCards.map(s => (
          <Card key={s.title}>
            <CardHeader className="flex flex-row items-center justify-between pb-1 pt-4 px-4">
              <CardTitle className="text-xs font-medium text-muted-foreground">{s.title}</CardTitle>
              <s.icon className={`w-4 h-4 ${s.color}`} />
            </CardHeader>
            <CardContent className="px-4 pb-4">
              <p className="text-3xl font-bold">{s.value}</p>
            </CardContent>
          </Card>
        ))}
      </div>

      <div className="grid grid-cols-1 xl:grid-cols-2 gap-6">
        {/* Model Health */}
        <Card>
          <CardHeader>
            <div className="flex items-center gap-2">
              <Cpu className="w-4 h-4 text-muted-foreground" />
              <CardTitle className="text-base">Model Status</CardTitle>
            </div>
          </CardHeader>
          <CardContent>
            {stats.modelList.length === 0 ? (
              <div className="text-center py-6 text-muted-foreground">
                <p className="text-sm">No models yet — go to <strong>Models</strong> to import from Ollama or deploy vLLM.</p>
              </div>
            ) : (
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-xs text-muted-foreground">
                    <th className="text-left pb-2">Model</th>
                    <th className="text-left pb-2">Type</th>
                    <th className="text-left pb-2">Backend</th>
                    <th className="text-left pb-2">Health</th>
                  </tr>
                </thead>
                <tbody>
                  {stats.modelList.map(m => (
                    <tr key={m.id} className="border-b last:border-0">
                      <td className="py-2 font-medium max-w-[120px] truncate">{m.name}</td>
                      <td className="py-2">
                        <span className={`text-xs px-1.5 py-0.5 rounded-full font-medium ${
                          SERVICE_TYPE_COLORS[m.service_type] ?? 'bg-gray-100 text-gray-600'
                        }`}>{m.service_type ?? 'CHAT'}</span>
                      </td>
                      <td className="py-2 text-xs text-muted-foreground font-mono">{m.backend_type}</td>
                      <td className="py-2">
                        <span className={`text-xs font-semibold ${
                          m.healthy_count > 0 ? 'text-green-600' : m.endpoint_count > 0 ? 'text-red-500' : 'text-gray-400'
                        }`}>
                          {m.healthy_count}/{m.endpoint_count}
                        </span>
                      </td>
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
            <div className="flex items-center gap-2">
              <Network className="w-4 h-4 text-muted-foreground" />
              <CardTitle className="text-base">Cluster Nodes</CardTitle>
            </div>
          </CardHeader>
          <CardContent>
            {stats.nodeList.length === 0 ? (
              <div className="text-center py-6 text-muted-foreground">
                <p className="text-sm">No nodes registered — run migrations 005+006 to seed the H200 node.</p>
              </div>
            ) : (
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-xs text-muted-foreground">
                    <th className="text-left pb-2">Host</th>
                    <th className="text-left pb-2">CPUs</th>
                    <th className="text-left pb-2">RAM</th>
                    <th className="text-left pb-2">VRAM</th>
                    <th className="text-left pb-2">Status</th>
                  </tr>
                </thead>
                <tbody>
                  {stats.nodeList.map(n => (
                    <tr key={n.id} className="border-b last:border-0">
                      <td className="py-2 font-mono text-xs">{n.hostname}</td>
                      <td className="py-2 text-xs">{n.total_cpu}</td>
                      <td className="py-2 text-xs">{n.total_ram_mb ? `${Math.round(n.total_ram_mb / 1024)}GB` : '—'}</td>
                      <td className="py-2 text-xs">{n.total_vram_mb ? `${Math.round(n.total_vram_mb / 1024)}GB` : '—'}</td>
                      <td className="py-2">
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
            <Activity className="w-4 h-4 text-muted-foreground" />
            <CardTitle className="text-base">Quick Start</CardTitle>
          </div>
        </CardHeader>
        <CardContent>
          <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-3 text-sm">
            <a href="/models" className="flex flex-col gap-1 p-3 rounded-lg border hover:bg-gray-50 transition-colors">
              <span className="font-medium">🦙 Import Ollama Models</span>
              <span className="text-xs text-muted-foreground">Register all local Ollama models in one click</span>
            </a>
            <a href="/services" className="flex flex-col gap-1 p-3 rounded-lg border hover:bg-gray-50 transition-colors">
              <span className="font-medium">📦 Deploy AI Service</span>
              <span className="text-xs text-muted-foreground">Add embeddings, rerankers, STT, TTS, OCR</span>
            </a>
            <a href="/placement" className="flex flex-col gap-1 p-3 rounded-lg border hover:bg-gray-50 transition-colors">
              <span className="font-medium">📍 Simulate Placement</span>
              <span className="text-xs text-muted-foreground">Dry-run resource placement before deploying</span>
            </a>
            <a href="/teams" className="flex flex-col gap-1 p-3 rounded-lg border hover:bg-gray-50 transition-colors">
              <span className="font-medium">👥 Create Team</span>
              <span className="text-xs text-muted-foreground">Set up rate limits, quotas, and API keys</span>
            </a>
          </div>
        </CardContent>
      </Card>
    </div>
  )
}
