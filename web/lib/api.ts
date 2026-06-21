// Typed API client — all calls go to /api/admin/* which Next.js proxies
// to http://localhost:8081/admin/v1/*

const BASE = '/api/admin'

async function req<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
    cache: 'no-store',
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }))
    throw new Error(err.error ?? res.statusText)
  }
  return res.json()
}

// ── Types ─────────────────────────────────────────────────────────────────────

export interface Org {
  id: string; name: string; slug: string; active: boolean; created_at: string
}
export interface Team {
  id: string; org_id: string; name: string; slug: string
  priority: number; active: boolean; created_at: string
}
export interface Policy {
  id: string; team_id: string; rpm: number; tpd: number
  max_concurrent: number; max_context_tokens: number
}
export interface ApiKey {
  id: string; team_id: string; name: string
  key_prefix: string; active: boolean; created_at: string
  expires_at?: string; last_used_at?: string
}
export interface Model {
  id: string; name: string; display_name: string; provider: string
  backend_type: string; service_type: string; runtime_type: string
  max_context: number; max_output: number
  enabled: boolean; endpoint_count: number; healthy_count: number
  tags?: string
}
export interface Endpoint {
  id: string; host: string; port: number; health_status: string
  lifecycle_state: string; container_id: string
  consecutive_failures: number; response_time_ms?: number
  last_checked_at?: string
}
export interface GpuNode {
  id: string; name: string; host: string; driver_type: string
  total_vram_mb: number; is_available: boolean
}
export interface GpuDevice {
  id: string; node_id: string; device_index: number; name: string
  vram_mb: number; status: string; utilization_pct: number
  temperature_c: number; power_draw_w: number
}
export interface UsageSummary {
  team_id: string; model_name: string; day: string
  request_count: number; error_count: number
  prompt_tokens: number; completion_tokens: number
  total_tokens: number; cost_usd: number
}

export interface ServiceRecord {
  id: string; name: string; display_name: string
  service_type: string; runtime_type: string; backend_type: string
  provider: string; max_context: number; max_output: number
  enabled: boolean; endpoint_count: number; healthy_count: number
  tags?: string; created_at: string
}

export interface ResourceReservation {
  id: string; model_id: string
  min_vram_mb: number; max_vram_mb: number
  cpu_cores: number; numa_node_pref: number; ram_mb: number
  priority: string; preferred_runtime: string
  created_at: string; updated_at: string
}

export interface ClusterNode {
  id: string; hostname: string; display_name: string
  total_cpu: number; total_ram_mb: number; total_vram_mb: number
  status: string; agent_version: string
  last_heartbeat_at?: string; labels: string; created_at: string
}

export interface NodeTelemetry {
  cpu_cores_total: number; cpu_util_pct: number
  ram_total_mb: number; ram_used_mb: number; ram_avail_mb: number
  numa_nodes: number; disk_total_gb: number; disk_used_gb: number
  recorded_at: string
}

export interface PlacementDecision {
  id: string; model_id: string; node_id?: string
  gpu_devices: string; cpu_cores: number; numa_node: number
  strategy: string; score: number; reason: string
  applied: boolean; created_at: string
}
export interface DeployModelInput {
  name: string; display_name: string; provider?: string
  backend_type: string; image?: string; hf_model_id?: string
  host: string; port: number; gpu_devices?: number[]
  tensor_parallel?: number; gpu_memory_util?: number
  max_model_len?: number; dtype?: string; hf_token?: string
  start_now?: boolean
}
export interface RegisterModelInput {
  name: string; display_name: string; backend_type: string
  host: string; port: number; provider?: string
  max_context?: number; max_output?: number
}

// ── Organisations ─────────────────────────────────────────────────────────────
export const api = {
  orgs: {
    list: () => req<{ data: Org[]; total: number }>('GET', '/orgs'),
    create: (b: { name: string; slug: string }) => req<Org>('POST', '/orgs', b),
    delete: (id: string) => req<void>('DELETE', `/orgs/${id}`),
  },

  teams: {
    list: (orgId?: string) =>
      req<{ data: Team[]; total: number }>('GET', orgId ? `/teams?org_id=${orgId}` : '/teams'),
    get: (id: string) => req<Team>('GET', `/teams/${id}`),
    create: (b: { org_id: string; name: string; slug: string; priority?: number }) =>
      req<Team>('POST', '/teams', b),
    delete: (id: string) => req<void>('DELETE', `/teams/${id}`),
    getPolicy: (id: string) => req<Policy>('GET', `/teams/${id}/policy`),
    updatePolicy: (id: string, b: Partial<Policy>) =>
      req<{ message: string }>('PUT', `/teams/${id}/policy`, b),
    addModel: (id: string, modelName: string) =>
      req<{ message: string }>('POST', `/teams/${id}/models`, { model_name: modelName }),
    removeModel: (id: string, model: string) =>
      req<void>('DELETE', `/teams/${id}/models/${model}`),
  },

  apiKeys: {
    list: (teamId: string) =>
      req<{ data: ApiKey[]; total: number }>('GET', `/teams/${teamId}/api-keys`),
    create: (teamId: string, name: string, expiresAt?: string) =>
      req<ApiKey & { key: string }>('POST', `/teams/${teamId}/api-keys`, { name, expires_at: expiresAt }),
    revoke: (id: string) => req<void>('DELETE', `/api-keys/${id}`),
  },

  models: {
    list: () => req<{ data: Model[]; total: number }>('GET', '/models'),
    deploy: (b: DeployModelInput) =>
      req<{ model_id: string; endpoint_id: string; started: boolean; note?: string }>('POST', '/models/deploy', b),
    register: (b: RegisterModelInput) =>
      req<{ model_id: string; endpoint_id: string }>('POST', '/models', b),
    importOllama: (host = 'localhost', port = 11434) =>
      req<{ results: { name: string; status: string; model_id?: string }[]; total: number }>(
        'POST', '/models/import-ollama', { host, port }),
    health: (id: string) =>
      req<{ model_id: string; endpoints: Endpoint[] }>('GET', `/models/${id}/health`),
    resetHealth: (id: string, epId?: string) =>
      req<{ message: string; endpoints_updated: number }>(
        'POST', epId ? `/models/${id}/reset-health?endpoint_id=${epId}` : `/models/${id}/reset-health`, {}),
    enable: (id: string) => req<void>('POST', `/models/${id}/enable`),
    disable: (id: string) => req<void>('POST', `/models/${id}/disable`),
    drain: (id: string) => req<void>('POST', `/models/${id}/drain`),
    delete: (id: string) => req<void>('DELETE', `/models/${id}`),
    start: (id: string, epId: string) =>
      req<void>('POST', `/models/${id}/start?endpoint_id=${epId}`),
    stop: (id: string, epId: string) =>
      req<void>('POST', `/models/${id}/stop?endpoint_id=${epId}`),
    restart: (id: string, epId: string) =>
      req<void>('POST', `/models/${id}/restart?endpoint_id=${epId}`),
  },

  gpu: {
    listNodes: () => req<{ data: GpuNode[]; total: number }>('GET', '/gpu/nodes'),
    listDevices: (nodeId: string) =>
      req<{ data: GpuDevice[]; total: number }>('GET', `/gpu/nodes/${nodeId}/devices`),
    registerNode: (b: { name: string; host: string; driver_type?: string }) =>
      req<GpuNode>('POST', '/gpu/nodes', b),
    registerDevice: (nodeId: string, b: { device_index: number; name: string; vram_mb: number }) =>
      req<GpuDevice>('POST', `/gpu/nodes/${nodeId}/devices`, b),
  },

  usage: {
    teamDaily: (teamId: string, from: string, to: string) =>
      req<{ data: UsageSummary[] }>('GET', `/usage/teams/${teamId}?from=${from}&to=${to}`),
    orgMonthlySpend: (orgId: string) =>
      req<{ monthly_spend_usd: number }>('GET', `/usage/orgs/${orgId}/monthly-spend`),
  },

  services: {
    list: (type?: string) =>
      req<{ data: ServiceRecord[]; total: number }>('GET', type ? `/services?type=${type}` : '/services'),
    register: (b: Partial<ServiceRecord> & { host: string; port: number }) =>
      req<{ model_id: string; endpoint_id: string }>('POST', '/services', b),
    deploy: (b: Record<string, unknown>) =>
      req<{ model_id: string; endpoint_id: string; placement: Record<string, unknown> }>('POST', '/services/deploy', b),
    getReservation: (id: string) =>
      req<ResourceReservation>('GET', `/services/${id}/reservation`),
    upsertReservation: (id: string, b: Partial<ResourceReservation>) =>
      req<{ message: string }>('PUT', `/services/${id}/reservation`, b),
  },

  nodes: {
    list: () => req<{ data: ClusterNode[]; total: number }>('GET', '/nodes'),
    get: (id: string) => req<{ node: ClusterNode; telemetry?: NodeTelemetry }>('GET', `/nodes/${id}`),
    register: (b: { hostname: string; display_name?: string; total_cpu?: number; total_ram_mb?: number; labels?: Record<string, string> }) =>
      req<{ id: string; hostname: string }>('POST', '/nodes', b),
    getTelemetry: (id: string) =>
      req<{ data: NodeTelemetry[]; node_id: string }>('GET', `/nodes/${id}/telemetry`),
    getInventory: (id: string) =>
      req<{ id: string; snapshot: string; agent_version: string; reported_at: string }>('GET', `/nodes/${id}/inventory`),
  },

  placement: {
    simulate: (b: {
      model_name: string; service_type: string; runtime_type?: string
      min_vram_mb?: number; gpu_count?: number; cpu_cores?: number
      numa_node?: number; ram_mb?: number; priority?: string
    }) => req<{ feasible: boolean; decision?: Record<string, unknown>; error?: string }>('POST', '/placement/simulate', b),
    listDecisions: () =>
      req<{ data: PlacementDecision[]; total: number }>('GET', '/placement/decisions'),
  },
}
