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
  // For 204 No Content or empty body responses, return undefined
  const text = await res.text()
  if (!text) return undefined as T
  return JSON.parse(text) as T
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
  lifecycle: string  // active | archived | deleted
  tags?: string
  // Thinking/reasoning mode capability flags
  supports_thinking: boolean
  thinking_enabled: boolean
  min_thinking_tokens: number
}

export interface RuntimeRequirements {
  id: string; model_id: string
  execution_type: string   // GPU | CPU | ANY
  required_vram_mb: number; gpu_count: number
  required_cpu: number; required_memory_mb: number
  requires_docker: boolean; requires_gpu: boolean
  requires_vllm: boolean; requires_ollama: boolean
  requires_tts: boolean; requires_whisper: boolean
  priority: string
  updated_at: string
}

export interface CompatibleNode {
  id: string; hostname: string; ip_address: string; status: string
  total_vram_mb: number; total_cpu: number; total_ram_mb: number
  compatible: boolean; reason: string
}
export interface Endpoint {
  id: string; host: string; port: number; health_status: string
  lifecycle_state: string; container_id: string
  consecutive_failures: number; response_time_ms?: number
  last_checked_at?: string
}
export interface GpuNode {
  id: string; name: string; host: string; driver_type: string
  total_vram_mb: number; is_available: boolean; node_id?: string
}
export interface GpuDevice {
  id: string; node_id: string; device_index: number; name: string
  vram_mb: number; status: string; utilization_pct: number
  temperature_c: number; power_draw_w: number; numa_node: number
}
export interface UsageSummary {
  model_name: string
  request_count: number
  error_count: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cost_usd: number
  avg_latency_ms: number
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
  ip_address: string
  total_cpu: number; total_ram_mb: number; total_vram_mb: number
  status: string; agent_version: string
  cordoned: boolean; cordon_reason: string
  last_heartbeat_at?: string; labels: string; created_at: string
}

export interface NodeGPUDevice {
  id: string
  device_index: number
  name: string
  vram_mb: number
  status: string
  pcie_bus_id: string
  numa_node: number
  utilization_pct: number
  mem_used_mb: number
  temperature_c: number
  power_draw_w: number
  power_limit_w: number
  last_seen_at?: string
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
  // Legacy node agent deployment
  node_id?: string
  auto_place?: boolean
  min_vram_mb?: number
  priority?: string
  // Placement v2 — strategy
  placement_strategy?: 'auto' | 'pinned' | 'spread' | 'packed'
  accelerator_type?: 'any' | 'gpu' | 'cpu'
  replica_distribution?: 'spread' | 'pack' | 'anti_affinity'
  pinned_node_id?: string
  // Placement v2 — modes
  placement_mode?: 'auto' | 'specific_node' | 'node_group' | 'label_selector'
  specific_node_id?: string
  node_group_id?: string
  node_selector?: Record<string, string>
  // llamacpp-specific
  llamacpp_model_path?: string
  llamacpp_hf_repo?: string
  llamacpp_hf_file?: string
  llamacpp_ctx_size?: number
  llamacpp_n_gpu_layers?: number
  llamacpp_models_volume?: string
  // Thinking / reasoning mode
  supports_thinking?: boolean
  thinking_enabled?: boolean
  min_thinking_tokens?: number
}

export interface LazyConfig {
  gguf_path?: string
  hf_repo?: string
  hf_file?: string
  hf_token?: string
  ctx_size: number
  n_gpu_layers: number
  cpu_threads?: number
  memory_limit?: string
  models_volume?: string
  idle_timeout_secs?: number
  execution_mode?: string
  node_id?: string
  extra_args?: string[] | null
  updated_at: string
}

export interface RuntimeStatus {
  runtime_id: string
  node_id: string
  hostname: string
  state: string
  container_id: string
  bind_host: string
  bind_port: number
  last_used_at?: string
  updated_at: string
}
export interface RegisterModelInput {
  name: string; display_name: string; backend_type: string
  host: string; port: number; provider?: string
  max_context?: number; max_output?: number
}

// ── Project types ─────────────────────────────────────────────────────────────
export type ProjectStatus   = 'active' | 'inactive' | 'archived'
export type AdmissionPolicy = 'queue' | 'preempt_then_queue' | 'reject'

/** priority_weight is a continuous integer in [0, 1000]. Higher = scheduled sooner. */
export type PriorityWeight = number

export interface PriorityPreset {
  weight: number
  label: string
  color: string
}

export interface EffectivePriority {
  base_weight: number
  waiting_bonus: number
  reservation_bonus: number
  resource_penalty: number
  effective_priority: number
}

export interface Project {
  id: string
  organization_id: string
  team_id: string
  name: string
  description: string
  priority_weight: number
  priority_label: string
  effective_priority: number
  waiting_bonus: number
  reservation_bonus: number
  resource_penalty: number
  preemptible: boolean
  status: ProjectStatus
  runtime_count: number
  reserved_vram_mb: number
  reserved_cpu_cores: number
  reserved_memory_mb: number
  max_gpu_vram_mb: number
  max_cpu: number
  max_memory_mb: number
  always_running: boolean
  protected: boolean
  minimum_replicas: number
  admission_policy: AdmissionPolicy
  created_at: string
  updated_at: string
}

export interface ProjectRuntime {
  id: string
  model_id: string
  state: string
  node_id: string
  gpu_ids: string
  bind_host: string
  bind_port: number
  last_used_at?: string
  updated_at: string
}

export interface ProjectUsage {
  project_id: string
  from: string
  to: string
  total_requests: number
  total_tokens: number
  prompt_tokens: number
  completion_tokens: number
  cost_usd: number
  gpu_time_ms: number
  avg_latency_ms: number
  error_count: number
  runtime_count: number
  preemption_count: number
  breakdown?: ProjectUsageModelRow[]
}

export interface ProjectUsageModelRow {
  model_name: string
  total_requests: number
  total_tokens: number
  prompt_tokens: number
  completion_tokens: number
  cost_usd: number
  gpu_time_ms: number
  avg_latency_ms: number
  error_count: number
}

export interface PreemptionEvent {
  id: string
  node_id?: string
  preempted_runtime_id?: string
  preempted_project_id?: string
  preempted_weight?: number
  requesting_runtime_id?: string
  requesting_project_id?: string
  requesting_weight?: number
  trigger: string
  created_at: string
}

export interface DeploymentQueueEntry {
  id: string
  model_name: string
  priority_weight: number
  effective_priority: number
  admission_policy: string
  status: string
  attempts: number
  waiting_since: string
  enqueued_at: string
  expires_at?: string
  required_vram_mb: number
  required_ram_mb: number
  required_cpu: number
  preemption_reason: string
  error_msg: string
}

export interface SchedulerDecision {
  id: string
  model_id: string
  model_name: string
  project_id?: string
  node_id?: string
  decision_type: 'placement' | 'preemption' | 'queue' | 'reject' | 'reschedule'
  priority_weight: number
  effective_priority: number
  waiting_bonus: number
  reservation_bonus: number
  resource_penalty: number
  node_score: number
  reason: string
  decision_trace: Record<string, unknown>
  alternatives: unknown[]
  outcome: 'pending' | 'success' | 'failed' | 'timeout' | 'cancelled'
  error_msg: string
  decided_at: string
  completed_at?: string
}

// ── Project policy & quota types (migration 023) ─────────────────────────────
export interface ProjectPolicy {
  id?: string
  project_id?: string
  rpm: number
  tpm: number
  max_concurrent: number
  max_context_tokens: number
  daily_token_budget: number
  monthly_token_budget: number
  daily_cost_budget: number
  monthly_cost_budget: number
  updated_at?: string
}

export interface ProjectQuotaStatus {
  project_id: string
  rpm_limit: number
  tpm_limit: number
  max_concurrent_limit: number
  daily_token_budget: number
  monthly_token_budget: number
  daily_cost_budget: number
  monthly_cost_budget: number
  // live counters
  daily_tokens_used: number
  tpm_current: number
  inflight: number
  daily_tokens_remaining: number | null
}

export interface ProjectDailySummary {
  project_id: string
  model_name: string
  day: string
  request_count: number
  error_count: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cost_usd: number
  avg_latency_ms: number
}

export interface ProjectUsageSummary {
  project_id: string
  from: string
  to: string
  request_count: number
  error_count: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cost_usd: number
  avg_latency_ms: number
}

// ── HA / Replica types ────────────────────────────────────────────────────────
export type HAStatus = 'healthy' | 'degraded' | 'starting' | 'unavailable'
export type PlacementPolicy = 'spread' | 'pack' | 'anti_affinity'

export interface ReplicaStatus {
  model_id: string
  model_name: string
  desired_replicas: number
  min_available: number
  placement_policy: PlacementPolicy
  auto_recover: boolean
  active_replicas: number
  starting_replicas: number
  idle_replicas: number
  lost_replicas: number
  node_count: number
  ha_status: HAStatus
}

export interface ReplicaInstance {
  runtime_id: string
  node_id: string
  node_hostname: string
  state: string
  bind_host: string
  bind_port: number
  updated_at: string
}

export interface RecoveryLogEntry {
  id: string
  model_id: string
  model_name: string
  lost_runtime_id?: string
  lost_node_id?: string
  new_runtime_id?: string
  new_node_id?: string
  replica_index?: number
  trigger: string
  status: string
  reason: string
  created_at: string
  completed_at?: string
}

export interface ClusterHAStatus {
  models: ReplicaStatus[]
  total: number
  healthy: number
  degraded: number
  unavailable: number
  reconciler_last_sweep: string
  recoveries_triggered: number
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
    update: (id: string, b: { name?: string; slug?: string; priority?: number; active?: boolean }) =>
      req<{ message: string }>('PUT', `/teams/${id}`, b),
    delete: (id: string) => req<void>('DELETE', `/teams/${id}`),
    getPolicy: (id: string) => req<Policy>('GET', `/teams/${id}/policy`),
    updatePolicy: (id: string, b: Partial<Policy>) =>
      req<{ message: string }>('PUT', `/teams/${id}/policy`, b),
    addModel: (id: string, modelName: string) =>
      req<{ message: string }>('POST', `/teams/${id}/models`, { model_name: modelName }),
    removeModel: (id: string, model: string) =>
      req<void>('DELETE', `/teams/${id}/models/${model}`),
    listModels: (id: string) =>
      req<{ models: string[]; total: number }>('GET', `/teams/${id}/models`),
  },

  apiKeys: {
    list: (teamId: string) =>
      req<{ data: ApiKey[]; total: number }>('GET', `/teams/${teamId}/api-keys`),
    create: (teamId: string, name: string, expiresAt?: string, projectId?: string) =>
      req<ApiKey & { key: string; project_name?: string; project_priority_weight?: number }>(
        'POST', `/teams/${teamId}/api-keys`,
        { name, expires_at: expiresAt, project_id: projectId || undefined }
      ),
    revoke: (id: string) => req<void>('DELETE', `/api-keys/${id}`),
    setProject: (id: string, projectId: string | null) =>
      req<{ message: string }>('PUT', `/api-keys/${id}/project`, { project_id: projectId }),
  },

  models: {
    list: (lifecycle?: string) => req<{ data: Model[]; total: number }>('GET', lifecycle ? `/models?lifecycle=${lifecycle}` : '/models'),
    deploy: (b: DeployModelInput) =>
      req<{ model_id: string; endpoint_id: string; started: boolean; note?: string; task_id?: string }>('POST', '/models/deploy', b),
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
    archive: (id: string) =>
      req<{ message: string; model_id: string }>('POST', `/models/${id}/archive`, {}),
    restore: (id: string) =>
      req<{ message: string; model_id: string }>('POST', `/models/${id}/restore`, {}),
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
    getRequirements: (id: string) =>
      req<RuntimeRequirements>('GET', `/models/${id}/requirements`),
    setRequirements: (id: string, b: Partial<RuntimeRequirements>) =>
      req<{ message: string }>('POST', `/models/${id}/requirements`, b),
    compatibleNodes: (modelId: string) =>
      req<{ compatible: CompatibleNode[]; incompatible: CompatibleNode[]; model_id: string }>(
        'GET', `/scheduler/compatible-nodes?model_id=${modelId}`),
    getLazyConfig: (id: string) =>
      req<LazyConfig>('GET', `/models/${id}/lazy-config`),
    setLazyConfig: (id: string, b: Partial<LazyConfig>) =>
      req<{ message: string }>('PUT', `/models/${id}/lazy-config`, b),
    setThinkingMode: (id: string, b: {
      supports_thinking?: boolean
      thinking_enabled?: boolean
      min_thinking_tokens?: number
    }) => req<{ message: string }>('PUT', `/models/${id}/thinking`, b),
    getRuntimeStatus: (id: string) =>
      req<{ model_id: string; runtimes: RuntimeStatus[]; count: number }>(
        'GET', `/models/${id}/runtime-status`),
  },

  gpu: {
    listNodes: () => req<{ data: GpuNode[]; total: number }>('GET', '/gpu/nodes'),
    listDevices: (nodeId: string) =>
      req<{ data: GpuDevice[]; total: number }>('GET', `/gpu/nodes/${nodeId}/devices`),
    registerNode: (b: { name: string; host: string; driver_type?: string }) =>
      req<GpuNode>('POST', '/gpu/nodes', b),
    registerDevice: (nodeId: string, b: { device_index: number; name: string; vram_mb: number }) =>
      req<GpuDevice>('POST', `/gpu/nodes/${nodeId}/devices`, b),
    deleteNode: (nodeId: string) =>
      req<{ message: string }>('DELETE', `/gpu/nodes/${nodeId}`),
    deleteDevice: (nodeId: string, deviceId: string) =>
      req<{ message: string }>('DELETE', `/gpu/nodes/${nodeId}/devices/${deviceId}`),
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
    delete: (id: string) =>
      req<{ message: string; node_id: string }>('DELETE', `/nodes/${id}`),
    getTelemetry: (id: string) =>
      req<{ data: NodeTelemetry[]; node_id: string }>('GET', `/nodes/${id}/telemetry`),
    getInventory: (id: string) =>
      req<{ id: string; snapshot: string; agent_version: string; reported_at: string }>('GET', `/nodes/${id}/inventory`),
    getModelCache: (id: string) =>
      req<{ data: { model_ref: string; backend: string; size_bytes: number; cached_at?: string }[]; node_id: string; total: number }>('GET', `/nodes/${id}/model-cache`),
    getGPUs: (id: string) =>
      req<{ data: NodeGPUDevice[]; node_id: string; total: number }>('GET', `/nodes/${id}/gpus`),
    drain: (id: string) =>
      req<{ message: string; node_id: string }>('POST', `/nodes/${id}/drain`, {}),
    cordon: (id: string, reason?: string) =>
      req<{ message: string; node_id: string }>('POST', `/nodes/${id}/cordon`, { reason: reason ?? 'admin cordoned' }),
    uncordon: (id: string) =>
      req<{ message: string; node_id: string }>('POST', `/nodes/${id}/uncordon`, {}),
    setLabels: (id: string, labels: Record<string, string>) =>
      req<{ message: string; node_id: string; labels: Record<string, string> }>('PUT', `/nodes/${id}/labels`, { labels }),
    getHealthEvents: (id: string) =>
      req<{ data: { id: number; from_status: string; to_status: string; reason: string; created_at: string }[]; node_id: string }>('GET', `/nodes/${id}/health-events`),
    // Task management
    dispatchTask: (nodeId: string, taskType: string, payload: Record<string, unknown>, priority?: number) =>
      req<{ task_id: string; node_id: string; status: string }>('POST', `/nodes/${nodeId}/tasks`, {
        task_type: taskType, payload, priority: priority ?? 70, actor: 'admin-ui',
      }),
    listTasks: (nodeId: string) =>
      req<{ data: unknown[]; total: number }>('GET', `/nodes/${nodeId}/tasks`),
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

  projects: {
    list: (params?: { org_id?: string; team_id?: string; min_weight?: number; max_weight?: number; status?: string }) => {
      const qs = params ? '?' + Object.entries(params).filter(([,v]) => v !== undefined && v !== '').map(([k,v]) => `${k}=${v}`).join('&') : ''
      return req<{ data: Project[]; total: number }>('GET', `/projects${qs}`)
    },
    get: (id: string) => req<Project>('GET', `/projects/${id}`),
    create: (b: {
      organization_id: string; team_id: string; name: string
      description?: string; priority_weight?: number; preemptible?: boolean; status?: ProjectStatus
    }) => req<{ id: string; name: string; priority_weight: number; priority_label: string; status: string }>('POST', '/projects', b),
    update: (id: string, b: { name?: string; description?: string; priority_weight?: number; preemptible?: boolean; status?: ProjectStatus }) =>
      req<{ message: string }>('PUT', `/projects/${id}`, b),
    delete: (id: string) => req<{ message: string }>('DELETE', `/projects/${id}`),
    reserve: (id: string, b: {
      reserved_vram_mb?: number; reserved_cpu_cores?: number; reserved_memory_mb?: number
      max_gpu_vram_mb?: number; max_cpu?: number; max_memory_mb?: number
    }) => req<{ message: string }>('POST', `/projects/${id}/reserve`, b),
    setPriority: (id: string, priority_weight: number) =>
      req<{ message: string; old_priority_weight: number; new_priority_weight: number; new_priority_label: string; changed: boolean }>(
        'POST', `/projects/${id}/priority`, { priority_weight }
      ),
    setProtection: (id: string, b: { always_running?: boolean; protected?: boolean; minimum_replicas?: number; admission_policy?: AdmissionPolicy }) =>
      req<{ message: string }>('PUT', `/projects/${id}/protection`, b),
    getRuntimes: (id: string) =>
      req<{ data: ProjectRuntime[]; total: number; project_id: string }>('GET', `/projects/${id}/runtimes`),
    getUsage: (id: string, from?: string, to?: string, breakdown?: 'model') => {
      const qs = [from && `from=${from}`, to && `to=${to}`, breakdown && `breakdown=${breakdown}`].filter(Boolean).join('&')
      return req<ProjectUsage>('GET', `/projects/${id}/usage${qs ? '?' + qs : ''}`)
    },
    getPreemptions: (id: string, limit = 50, offset = 0) =>
      req<{ data: PreemptionEvent[]; total: number; limit: number; offset: number }>('GET', `/projects/${id}/preemptions?limit=${limit}&offset=${offset}`),
    getQueue: (id: string) =>
      req<{ data: DeploymentQueueEntry[]; total: number }>('GET', `/projects/${id}/queue`),
  },

  scheduler: {
    getPriorityPresets: () =>
      req<{ presets: PriorityPreset[] }>('GET', '/scheduler/priority-presets'),
    getQueue: (params?: { limit?: number; offset?: number }) => {
      const qs = params ? '?' + Object.entries(params).filter(([,v]) => v !== undefined).map(([k,v]) => `${k}=${v}`).join('&') : ''
      return req<{ data: DeploymentQueueEntry[]; total: number }>('GET', `/scheduler/queue${qs}`)
    },
    getDecisions: (params?: { model_id?: string; project_id?: string; limit?: number }) => {
      const qs = params ? '?' + Object.entries(params).filter(([,v]) => v !== undefined && v !== '').map(([k,v]) => `${k}=${v}`).join('&') : ''
      return req<{ data: SchedulerDecision[]; total: number }>('GET', `/scheduler/decisions${qs}`)
    },
  },

  ha: {
    getClusterStatus: () =>
      req<ClusterHAStatus>('GET', '/ha/status'),
    getModelStatus: (modelId: string) =>
      req<{ status: ReplicaStatus; replicas: ReplicaInstance[] }>('GET', `/ha/status/${modelId}`),
    setReplicaSpec: (modelId: string, b: {
      desired_replicas?: number
      min_available?: number
      placement_policy?: PlacementPolicy
      auto_recover?: boolean
      recovery_delay_s?: number
      max_surge?: number
    }) => req<{ message: string; model_id: string; model_name: string }>('PUT', `/ha/models/${modelId}/replicas`, b),
    getRecoveryLog: (params?: { limit?: number }) => {
      const qs = params?.limit ? `?limit=${params.limit}` : ''
      return req<{ data: RecoveryLogEntry[]; total: number }>('GET', `/ha/recovery-log${qs}`)
    },
    getModelRecoveryLog: (modelId: string, params?: { limit?: number }) => {
      const qs = params?.limit ? `?limit=${params.limit}` : ''
      return req<{ data: RecoveryLogEntry[]; total: number; model_id: string }>('GET', `/ha/recovery-log/${modelId}${qs}`)
    },
  },

  // ── Project policy & quota (migration 023) ────────────────────────────────
  projectPolicy: {
    getPolicy: (projectId: string) =>
      req<ProjectPolicy>('GET', `/projects/${projectId}/policy`),
    updatePolicy: (projectId: string, b: Partial<ProjectPolicy>) =>
      req<{ message: string; project_id: string }>('PUT', `/projects/${projectId}/policy`, b),
    getQuota: (projectId: string) =>
      req<ProjectQuotaStatus>('GET', `/projects/${projectId}/quota`),
    getDailyUsage: (projectId: string, from?: string, to?: string) => {
      const qs = [from && `from=${from}`, to && `to=${to}`].filter(Boolean).join('&')
      return req<{ data: ProjectDailySummary[]; total: number; project_id: string; from: string; to: string }>(
        'GET', `/projects/${projectId}/usage/daily${qs ? '?' + qs : ''}`)
    },
    getSummary: (projectId: string, from?: string, to?: string) => {
      const qs = [from && `from=${from}`, to && `to=${to}`].filter(Boolean).join('&')
      return req<ProjectUsageSummary>('GET', `/projects/${projectId}/usage/summary${qs ? '?' + qs : ''}`)
    },
  },
}
