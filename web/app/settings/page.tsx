import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Settings, ExternalLink } from 'lucide-react'

export default function SettingsPage() {
  const links = [
    { label: 'Gateway API',     url: 'http://localhost:8080', desc: 'OpenAI-compatible inference API — all /v1/* routes' },
    { label: 'Admin API',       url: 'http://localhost:8081', desc: 'Management REST API — models, teams, nodes, placement' },
    { label: 'Gateway Metrics', url: 'http://localhost:9090/metrics', desc: 'Prometheus metrics (gateway)' },
    { label: 'Admin Metrics',   url: 'http://localhost:9091/metrics', desc: 'Prometheus metrics (admin)' },
    { label: 'Grafana',         url: 'http://localhost:3000', desc: 'Dashboards (admin/admin)' },
  ]

  const env = [
    { key: 'NEXUS_AUTH_JWTSECRET',    desc: 'JWT signing secret — must be set before production use' },
    { key: 'NEXUS_DATABASE_DSN',      desc: 'PostgreSQL connection string' },
    { key: 'NEXUS_REDIS_ADDR',        desc: 'Redis address (default: localhost:6379)' },
    { key: 'NEXUS_VLLM_POLLINTERVAL', desc: 'Watcher health-check interval (default: 5s)' },
    { key: 'NEXUS_SERVER_PORT',       desc: 'Gateway listen port (default: 8080)' },
    { key: 'NEXUS_ADMIN_PORT',        desc: 'Admin API listen port (default: 8081)' },
  ]

  const gatewayRoutes = [
    ['POST', '/v1/chat/completions',     'LLM inference (streaming + sync)'],
    ['POST', '/v1/embeddings',           'Text embeddings'],
    ['POST', '/v1/rerank',               'Cross-encoder reranking'],
    ['POST', '/v1/audio/transcriptions', 'Speech-to-text (multipart upload)'],
    ['POST', '/v1/audio/speech',         'Text-to-speech → audio binary'],
    ['POST', '/v1/ocr',                  'Optical character recognition'],
    ['GET',  '/v1/models',               'List allowed models for this API key'],
    ['GET',  '/healthz',                 'Liveness probe'],
    ['GET',  '/readyz',                  'Readiness probe + live model list'],
  ]

  const adminRoutes = [
    ['POST',   '/admin/v1/models/deploy',            'Register + auto-place + start container'],
    ['POST',   '/admin/v1/models/import-ollama',     'Bulk-import all models from a running Ollama'],
    ['POST',   '/admin/v1/models/:id/reset-health',  'Reset failed health state (triggers re-check)'],
    ['POST',   '/admin/v1/services/deploy',          'Deploy any AI service with auto-placement'],
    ['GET',    '/admin/v1/services[?type=]',         'List all AI services by type'],
    ['POST',   '/admin/v1/placement/simulate',       'Dry-run placement (no resources committed)'],
    ['GET',    '/admin/v1/placement/decisions',      'Placement audit log'],
    ['GET',    '/admin/v1/nodes',                    'List cluster nodes'],
    ['POST',   '/admin/v1/nodes/:id/heartbeat',      'Agent heartbeat'],
    ['GET',    '/admin/v1/nodes/:id/telemetry',      'Last 60 telemetry snapshots'],
  ]

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-2">
        <Settings className="w-6 h-6" />
        <h1 className="text-2xl font-bold">Settings & Reference</h1>
      </div>

      {/* Service endpoints */}
      <Card>
        <CardHeader><CardTitle>Service Endpoints</CardTitle></CardHeader>
        <CardContent className="space-y-2">
          {links.map(l => (
            <div key={l.url} className="flex items-center justify-between border rounded-md px-3 py-2">
              <div>
                <p className="text-sm font-medium">{l.label}</p>
                <p className="text-xs text-muted-foreground">{l.desc}</p>
              </div>
              <a href={l.url} target="_blank" rel="noreferrer"
                className="text-blue-600 hover:text-blue-800 flex items-center gap-1 text-xs font-mono">
                {l.url} <ExternalLink className="w-3 h-3" />
              </a>
            </div>
          ))}
        </CardContent>
      </Card>

      {/* Gateway API */}
      <Card>
        <CardHeader><CardTitle>Gateway API (inference, :8080)</CardTitle></CardHeader>
        <CardContent>
          <table className="w-full text-xs">
            <thead><tr className="border-b text-muted-foreground">
              <th className="text-left pb-2 w-16">Method</th>
              <th className="text-left pb-2 w-64">Path</th>
              <th className="text-left pb-2">Description</th>
            </tr></thead>
            <tbody>
              {gatewayRoutes.map(([m, p, d]) => (
                <tr key={p} className="border-b last:border-0">
                  <td className="py-1.5">
                    <span className={`font-mono font-semibold ${m === 'GET' ? 'text-green-600' : 'text-blue-600'}`}>{m}</span>
                  </td>
                  <td className="py-1.5 font-mono text-muted-foreground pr-4">{p}</td>
                  <td className="py-1.5 text-muted-foreground">{d}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </CardContent>
      </Card>

      {/* Admin API highlights */}
      <Card>
        <CardHeader><CardTitle>Admin API highlights (:8081)</CardTitle></CardHeader>
        <CardContent>
          <table className="w-full text-xs">
            <thead><tr className="border-b text-muted-foreground">
              <th className="text-left pb-2 w-16">Method</th>
              <th className="text-left pb-2 w-72">Path</th>
              <th className="text-left pb-2">Description</th>
            </tr></thead>
            <tbody>
              {adminRoutes.map(([m, p, d]) => (
                <tr key={p} className="border-b last:border-0">
                  <td className="py-1.5">
                    <span className={`font-mono font-semibold ${
                      m === 'GET' ? 'text-green-600' :
                      m === 'DELETE' ? 'text-red-500' : 'text-blue-600'
                    }`}>{m}</span>
                  </td>
                  <td className="py-1.5 font-mono text-muted-foreground pr-4 truncate max-w-[260px]">{p}</td>
                  <td className="py-1.5 text-muted-foreground">{d}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <p className="text-xs text-muted-foreground mt-3">Full API reference: see README.md</p>
        </CardContent>
      </Card>

      {/* Env vars */}
      <Card>
        <CardHeader><CardTitle>Environment Variables</CardTitle></CardHeader>
        <CardContent>
          <table className="w-full text-sm">
            <thead><tr className="border-b text-muted-foreground">
              <th className="text-left pb-2">Variable</th>
              <th className="text-left pb-2">Description</th>
            </tr></thead>
            <tbody>
              {env.map(e => (
                <tr key={e.key} className="border-b last:border-0">
                  <td className="py-2 font-mono text-xs">{e.key}</td>
                  <td className="py-2 text-muted-foreground text-sm">{e.desc}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </CardContent>
      </Card>

      {/* Quick start */}
      <Card>
        <CardHeader><CardTitle>Quick Start</CardTitle></CardHeader>
        <CardContent>
          <pre className="bg-gray-900 text-green-400 p-4 rounded-md text-xs overflow-x-auto">{`# 1. Start postgres + redis + run all migrations (001-006)
make dev-up

# 2. Run services (3 terminals)
make run-gateway    # inference API  :8080
make run-admin      # management API :8081
make run-scheduler  # queue dispatcher

# 3. Start this UI
cd web && npm run dev    # :3001

# 4. Import Ollama models (if Ollama is running)
# → Models → Import from Ollama

# 5. Deploy GPU model (vLLM, requires Docker + GPU)
# → Models → Deploy vLLM Model → set auto_place: true

# 6. Deploy CPU service (embeddings, STT, etc.)
# → AI Services → Register Service

# 7. Simulate placement before deploying
# → Placement → Run Placement Simulation`}</pre>
        </CardContent>
      </Card>
    </div>
  )
}
