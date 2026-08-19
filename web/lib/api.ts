// Typed API client — all calls go to /api/admin/* which Next.js proxies
// to http://nexus-admin:8081/admin/v1/*

const BASE = '/api/admin'

async function req<T>(method: string, path: string, body?: unknown): Promise<T> {
  const token = typeof window !== 'undefined' ? localStorage.getItem('nexus_token') : null
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
  }
  if (token) {
    headers['Authorization'] = `Bearer ${token}`
  }

  const res = await fetch(`${BASE}${path}`, {
    method,
    headers,
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

export interface AuthUser {
  user_id: string
  org_id: string
  email: string
  role: 'admin' | 'member' | 'developer' | string
  token: string
}

export async function loginUser(email: string, password: string, isAdmin = false): Promise<AuthUser> {
  const endpoint = isAdmin ? '/auth/login' : '/portal/v1/auth/login'
  const res = await fetch(`${BASE}${endpoint}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password }),
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: 'Login failed' }))
    throw new Error(err.error || 'Authentication failed')
  }
  return res.json()
}

export async function registerUser(email: string, password: string, name?: string, orgName?: string): Promise<AuthUser> {
  const res = await fetch(`${BASE}/portal/v1/auth/register`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password, name, org_name: orgName }),
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: 'Registration failed' }))
    throw new Error(err.error || 'Registration failed')
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
  backend_type: string; service_type: string
  max_context: number; max_output: number
  enabled: boolean; endpoint_count: number; healthy_count: number
  lifecycle: string  // active | archived | deleted
  // Who owns the container (migration 061):
  //   "managed" — NexusLLM starts/stops/recovers it
  //   "manual"  — the operator deployed it themselves; NexusLLM only routes to
  //               it and health-checks it, so an unhealthy endpoint stays
  //               unhealthy until the operator starts the container
  deployment_mode?: string
  tags?: string
  // Universal capabilities — what API endpoints this model supports
  capabilities?: string[]   // e.g. ["chat","completion"] or ["transcription"]
  // Thinking/reasoning mode capability flags
  supports_thinking: boolean
  thinking_enabled: boolean
  min_thinking_tokens: number
  // Provider / external model fields (migration 044).
  // All undefined for local self-hosted models.
  provider_is_external?: boolean
  provider_name?: string          // e.g. "openai_provider"
  upstream_base_url?: string
  upstream_model_name?: string
  upstream_api_key_set?: boolean  // true = key stored, never the key itself
}

// A project's Public-model grant, plus whether it's confirmed present in the
// live Redis ACL (not just Postgres) — see ProjectHandler.ListProjectModels.
// synced=false means the grant exists in the database but a Redis sync
// (grant/revoke/redeploy-restore) hasn't landed yet — the periodic
// reconciliation sweep will repair it, or POST /admin/v1/system/reconcile-permissions
// can be called to force an immediate repair.
export interface ProjectModelGrant {
  name: string
  synced: boolean
}

export interface RuntimeRequirements {
  id: string; model_id: string
  execution_type: string   // GPU | CPU | ANY
  required_vram_mb: number; gpu_count: number
  required_cpu: number; required_memory_mb: number
  requires_docker: boolean; requires_gpu: boolean
  requires_vllm: boolean
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
  // host is intentionally optional: when a node is targeted (node_id/
  // specific_node_id set), the backend resolves the node's own canonical
  // reachable address and it must not be overridable by a client-supplied
  // placeholder — omit this field for any node-backed deployment.
  host?: string; port: number; gpu_devices?: number[]
  tensor_parallel?: number; gpu_memory_util?: number
  max_model_len?: number; dtype?: string; hf_token?: string
  start_now?: boolean
  // Universal model type — every workload declares its type
  // LLM | STT | TTS | OCR | EMBEDDING | RERANK | VISION | IMAGE_GENERATION | CUSTOM
  service_type?: string
  // Explicit capability list — overrides automatic derivation from service_type.
  // Example: ["chat","completion"] for LLMs, ["transcription"] for Whisper models.
  // When omitted, capabilities are derived automatically from service_type.
  capabilities?: string[]
  // Extra args forwarded verbatim to the container entrypoint.
  // Used for non-llamacpp backends (faster-whisper, kokoro, surya, etc.)
  extra_args?: string[]
  // Environment variables injected into the container (key=value map)
  env?: Record<string, string>
  // env_vars is a legacy alias — use env instead
  env_vars?: Record<string, string>
  // Volume mounts beyond the default models volume: [{host:"/data", container:"/app/data"}]
  volume_mounts?: { host: string; container: string }[]
  // Custom health check path (default: /health)
  health_path?: string
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
  // Execution mode: cpu | gpu | auto — controls whether nvidia runtime is used
  execution_mode?: string
  // Cloud / external API credentials — leave blank for local self-hosted models.
  // upstream_api_key is injected as Authorization: Bearer on every upstream request.
  // upstream_base_url overrides host:port routing (e.g. "https://api.openai.com").
  // upstream_proxy routes outbound calls through an HTTP/SOCKS5 proxy.
  // upstream_model_name overrides req.model sent to the upstream provider.
  upstream_api_key?: string
  upstream_base_url?: string
  upstream_proxy?: string
  upstream_model_name?: string
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
  gpu_devices?: number[] | null
  env?: Record<string, string> | null
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
  service_type?: string
  max_context?: number; max_output?: number
  capabilities?: string[]
  // Cloud / external API credentials — leave blank for local self-hosted models.
  // upstream_api_key is injected as Authorization: Bearer on every upstream request.
  // upstream_base_url overrides host:port routing (e.g. "https://api.openai.com").
  // upstream_proxy routes outbound calls through an HTTP/SOCKS5 proxy.
  // upstream_model_name overrides req.model sent to the upstream provider.
  upstream_api_key?: string
  upstream_base_url?: string
  upstream_proxy?: string
  upstream_model_name?: string
}

// ── Project types ─────────────────────────────────────────────────────────────
export type ProjectStatus   = 'active' | 'inactive' | 'archived' | 'pending'
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

// ── Provider Catalog types (migration 047 + 050) ─────────────────────────
export type ExposureMode = 'managed' | 'catalog' | 'hybrid'

export interface CatalogProvider {
  id: string; name: string; display_name: string
  backend_type: string; base_url: string
  api_key_set: boolean
  exposure_mode: ExposureMode
  catalog_sync_enabled: boolean; catalog_sync_interval: number
  catalog_direct_expose: boolean; catalog_expose_prefix: string
  catalog_last_synced_at?: string; catalog_model_count: number
  catalog_sync_status: string; catalog_sync_error?: string
  proxy_url?: string; enabled: boolean; health: string
  last_health_check?: string; created_at: string; updated_at: string
}

export interface ProjectProviderAccess {
  id: string
  project_id: string
  provider_id: string
  provider_name: string
  exposure_mode: string
  allowed_prefixes: string[]
  denied_prefixes: string[]
  enabled: boolean
  created_at: string
  updated_at: string
}

export interface CatalogEntry {
  id: string; provider_model_id: string; display_name: string
  context_length?: number
  input_cost_per_1m?: number; output_cost_per_1m?: number
  supports_streaming: boolean; supports_tools: boolean
  supports_vision: boolean; supports_audio: boolean
  supports_embeddings: boolean; supports_reasoning: boolean
  tags: string[]; enabled: boolean; last_seen_at: string
}

export interface ExposureRule {
  id: string; provider_id: string; rule_type: string
  pattern?: string; model_id?: string
  require_streaming?: boolean; require_tools?: boolean
  require_vision?: boolean; require_audio?: boolean
  require_embeddings?: boolean; require_reasoning?: boolean
  deny_tags_raw?: string; priority: number; enabled: boolean
}

// ── Live provider model (raw shape from provider's /models endpoint) ─────────
export interface ProviderLiveModel {
  id: string
  canonical_slug?: string
  name?: string
  description?: string
  created?: number
  context_length?: number
  architecture?: {
    modality?: string
    input_modalities?: string[]
    output_modalities?: string[]
    tokenizer?: string
    instruct_type?: string | null
  }
  pricing?: {
    prompt?: string
    completion?: string
    input_cache_read?: string
    input_cache_write?: string
    image?: string
  }
  top_provider?: {
    context_length?: number
    max_completion_tokens?: number
    is_moderated?: boolean
  }
  supported_parameters?: string[]
  per_request_limits?: unknown
  reasoning?: {
    mandatory?: boolean
    default_enabled?: boolean
    supported_efforts?: string[]
    default_effort?: string
  }
  hugging_face_id?: string | null
  knowledge_cutoff?: string | null
  expiration_date?: string | null
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
    registerExternal: (b: {
      name: string
      display_name: string
      provider_backend_type: string
      service_type?: string
      max_context?: number
      max_output?: number
      upstream_api_key?: string
      upstream_base_url?: string
      upstream_model_name?: string
      /** @deprecated use proxy_url instead — stored in provider_proxy_url column (migration 046) */
      upstream_proxy?: string
      provider_api_version?: string
      provider_timeout_seconds?: number
      provider_max_retries?: number
      provider_extra_headers?: Record<string, string>
      capabilities?: string[]
      tags?: string[]
      // ── Per-provider transport config (migration 046) ──────────────────
      // All optional. Zero values apply BuildProviderClient() production defaults.
      // Transport is fully isolated per provider — changing one never affects others.
      /** Outbound proxy for this provider only. Schemes: http, https, socks5.
       *  Credentials may be embedded: socks5://user:pass@host:port
       *  Send "" or omit for direct connection. HTTP_PROXY env var is never used. */
      proxy_url?: string
      /** Disable TLS cert verification. Only for corporate MITM proxy environments. */
      tls_insecure_skip_verify?: boolean
      /** PEM root CA bundle appended to system roots (for self-signed proxy certs). */
      tls_root_ca_pem?: string
      /** TCP dial + TLS handshake timeout seconds. Default: 10. */
      connect_timeout_seconds?: number
      /** Non-streaming response body read timeout seconds. 0 = unlimited. */
      read_timeout_seconds?: number
      /** Keep-alive idle connection pool timeout seconds. Default: 90. */
      idle_conn_timeout_seconds?: number
      /** Max wait for response headers after request is sent, seconds. Default: 30. */
      response_header_timeout_seconds?: number
      /** Max idle keep-alive connections per host. Default: 32. */
      max_idle_conns_per_host?: number
      /** Max total connections (idle + active) per host. 0 = unlimited. */
      max_conns_per_host?: number
      /** Disable HTTP/2 negotiation. Only for providers with HTTP/2 issues. */
      disable_http2?: boolean
    }) => req<{
      model_id: string; endpoint_id: string
      provider_backend_type: string
      upstream_base_url: string
      upstream_api_key_set: boolean
      capabilities: string
      status: string; note: string
    }>('POST', '/models/external', b),
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
    setDeploymentMode: (id: string, deployment_mode: 'managed' | 'manual') =>
      req<{
        model_id: string
        model_name: string
        deployment_mode: string
        note?: string
        warning?: string
      }>('PUT', `/models/${id}/deployment-mode`, { deployment_mode }),
    setThinkingMode: (id: string, b: {
      supports_thinking?: boolean
      thinking_enabled?: boolean
      min_thinking_tokens?: number
    }) => req<{ message: string }>('PUT', `/models/${id}/thinking`, b),
    setCapabilities: (id: string, capabilities: string[]) =>
      req<{ model_id: string; capabilities: string[]; message: string }>(
        'PUT', `/models/${id}/capabilities`, { capabilities }
      ),
    getRuntimeStatus: (id: string) =>
      req<{ model_id: string; runtimes: RuntimeStatus[]; count: number }>(
        'GET', `/models/${id}/runtime-status`),
    getReservation: (id: string) =>
      req<{
        id: string; model_id: string
        min_vram_mb: number; max_vram_mb: number
        cpu_cores: number; numa_node_pref: number; ram_mb: number
        preferred_runtime: string; created_at: string; updated_at: string
      }>('GET', `/models/${id}/reservation`),
    upsertReservation: (id: string, b: {
      min_vram_mb?: number; max_vram_mb?: number
      cpu_cores?: number; numa_node_pref?: number; ram_mb?: number
      preferred_runtime?: string
    }) => req<{ message: string }>('PUT', `/models/${id}/reservation`, b),
    updateUpstream: (id: string, b: {
      upstream_api_key?: string
      upstream_base_url?: string
      upstream_proxy?: string
      upstream_model_name?: string
    }) => req<{ message: string; model_id: string; proxy_set: boolean }>(
      'PUT', `/models/${id}/upstream`, b),

    /** Update the per-provider HTTP transport config (migration 046 columns).
     *  All fields are optional — only provided fields are written.
     *  Send proxy_url: "" to remove the proxy and connect directly.
     *  The registry rebuilds the per-endpoint *http.Client immediately after save.
     *  Transport isolation is guaranteed: only this provider's client is rebuilt. */
    updateTransport: (id: string, b: {
      proxy_url?: string
      tls_insecure_skip_verify?: boolean
      tls_root_ca_pem?: string
      connect_timeout_seconds?: number
      read_timeout_seconds?: number
      idle_conn_timeout_seconds?: number
      response_header_timeout_seconds?: number
      max_idle_conns_per_host?: number
      max_conns_per_host?: number
      disable_http2?: boolean
    }) => req<{
      message: string
      model_id: string
      changed: Record<string, unknown>
      note: string
    }>('PUT', `/models/${id}/transport`, b),

    /** Get the current per-provider transport config for all endpoints of a model. */
    getTransport: (id: string) => req<{
      model_id: string
      count: number
      note: string
      endpoints: {
        endpoint_id: string
        proxy_url: string | null
        tls_insecure_skip_verify: boolean
        tls_root_ca_pem_set: boolean
        connect_timeout_seconds: number
        read_timeout_seconds: number
        idle_conn_timeout_seconds: number
        response_header_timeout_seconds: number
        max_idle_conns_per_host: number
        max_conns_per_host: number
        disable_http2: boolean
        upstream_proxy_legacy?: string | null
      }[]
    }>('GET', `/models/${id}/transport`),
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
    orgDailyUsage: (orgId: string, from: string, to: string) =>
      req<{ data: UsageSummary[] }>('GET', `/usage/orgs/${orgId}/daily?from=${from}&to=${to}`),
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
      model_name: string; runtime_type?: string
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
      organization_id: string
      team_id?: string          // optional — RBAC grouping only (migration 031)
      name: string
      description?: string
      priority_weight?: number
      preemptible?: boolean
      status?: ProjectStatus
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
    // Project-level Public-model authorization (migration 058). Narrows the
    // project's team model access — a model must be granted to BOTH the team
    // and the project to be usable by a project-scoped token. A project with
    // no grants here inherits its team's full access unchanged (legacy
    // passthrough) until the first grant/revoke call.
    listModels: (id: string) =>
      req<{ models: ProjectModelGrant[] }>('GET', `/projects/${id}/models`),
    addModel: (id: string, modelName: string) =>
      req<{ message: string }>('POST', `/projects/${id}/models`, { model_name: modelName }),
    removeModel: (id: string, model: string) =>
      req<{ message: string }>('DELETE', `/projects/${id}/models/${model}`),
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

  // ── Provider Catalog (migration 047) ─────────────────────────────────────
  providers: {
    list: () => req<{ data: CatalogProvider[]; total: number }>('GET', '/providers'),
    get: (id: string) => req<CatalogProvider>('GET', `/providers/${id}`),
    create: (b: {
      name: string; display_name: string; backend_type: string; base_url: string
      api_key?: string; exposure_mode?: ExposureMode
      catalog_sync_enabled?: boolean; catalog_sync_interval?: number
      catalog_direct_expose?: boolean; catalog_expose_prefix?: string; proxy_url?: string
      request_timeout_seconds?: number; max_retries?: number
    }) => req<{ id: string; name: string; exposure_mode: string; status: string }>('POST', '/providers', b),
    update: (id: string, b: Partial<{
      display_name: string; base_url: string; api_key: string
      exposure_mode: ExposureMode
      catalog_sync_enabled: boolean; catalog_sync_interval: number
      catalog_direct_expose: boolean; catalog_expose_prefix: string
      proxy_url: string; enabled: boolean
    }>) => req<{ message: string; id: string }>('PUT', `/providers/${id}`, b),
    delete: (id: string) => req<{ message: string; id: string }>('DELETE', `/providers/${id}`),
    sync: (id: string) => req<{ message: string; provider_id: string }>('POST', `/providers/${id}/sync`, {}),
    health: (id: string) => req<{ provider_id: string; health: string; latency_ms: number; error: string }>('GET', `/providers/${id}/health`),
    updateTransport: (id: string, b: {
      proxy_url?: string; tls_insecure_skip_verify?: boolean
      connect_timeout_seconds?: number; disable_http2?: boolean
    }) => req<{ message: string; provider_id: string }>('PUT', `/providers/${id}/transport`, b),
    listCatalog: (id: string, params?: {
      q?: string; capability?: string; tag?: string; exposed?: string
      page?: number; per_page?: number
    }) => {
      const qs = params ? '?' + Object.entries(params).filter(([,v]) => v !== undefined && v !== '').map(([k,v]) => `${k}=${v}`).join('&') : ''
      return req<{ data: CatalogEntry[]; total: number; page: number; per_page: number }>('GET', `/providers/${id}/catalog${qs}`)
    },
    listExposedModelIDs: (id: string) =>
      req<{ exposed: Record<string, string>; count: number }>('GET', `/providers/${id}/exposed-models`),
    exposeModels: (id: string, modelIds: string[]) =>
      req<{ created: number; provider_id: string; note: string }>('POST', `/providers/${id}/expose-models`, { model_ids: modelIds }),
    hideModels: (id: string, ruleIds: string[]) =>
      req<{ hidden: number; provider_id: string }>('POST', `/providers/${id}/hide-models`, { rule_ids: ruleIds }),
    listRules: (id: string) => req<{ data: ExposureRule[]; total: number }>('GET', `/providers/${id}/rules`),
    createRule: (id: string, b: {
      rule_type: string; pattern?: string; model_id?: string
      require_tools?: boolean; require_vision?: boolean; require_reasoning?: boolean
      deny_tags?: string[]; priority?: number
    }) => req<{ id: string; provider_id: string }>('POST', `/providers/${id}/rules`, b),
    deleteRule: (id: string, rid: string) => req<{ message: string }>('DELETE', `/providers/${id}/rules/${rid}`),
    previewRules: (id: string) => req<{ exposed_count: number; blocked_count: number; exposed: string[]; blocked: string[] }>('POST', `/providers/${id}/rules/preview`, {}),

    /** Promote catalog entries to Public Models (creates models + endpoints rows).
     *  After registration the models appear in api.models.list and can be granted
     *  to teams via the standard team_model_permissions flow. */
    registerModels: (id: string, models: { public_name: string; provider_model_id: string; display_name?: string; service_type?: string }[]) =>
      req<{
        created: number
        total: number
        results: { public_name: string; provider_model_id: string; model_id?: string; endpoint_id?: string; error?: string }[]
        note: string
      }>('POST', `/providers/${id}/register-models`, { models }),

    /** Proxy the provider's own /models endpoint via the admin server.
     *  Returns the raw JSON the provider sends — every provider-specific field
     *  (canonical_slug, supported_parameters, pricing, reasoning, etc.) intact.
     *  The admin server uses the stored API key + transport config (including proxy)
     *  so no client-side credentials are needed. */
    liveModels: (id: string, query?: string) =>
      req<{ data: ProviderLiveModel[] }>(
        'GET', `/providers/${id}/live-models${query ? `?${query}` : ''}`),
  },

  // ── Project Provider Access (migration 050) ──────────────────────────────
  providerAccess: {
    list: (projectId: string) =>
      req<{ data: ProjectProviderAccess[]; total: number; project_id: string }>(
        'GET', `/projects/${projectId}/provider-access`),
    grant: (projectId: string, b: {
      provider_id: string
      allowed_prefixes?: string[]
      denied_prefixes?: string[]
    }) => req<{
      id: string; project_id: string; provider_id: string
      provider_name: string; exposure_mode: string
      allowed_prefixes: string[]; denied_prefixes: string[]; note: string
    }>('POST', `/projects/${projectId}/provider-access`, b),
    update: (projectId: string, providerId: string, b: {
      allowed_prefixes?: string[]
      denied_prefixes?: string[]
      enabled?: boolean
    }) => req<{ message: string; project_id: string; provider_id: string }>(
      'PUT', `/projects/${projectId}/provider-access/${providerId}`, b),
    revoke: (projectId: string, providerId: string) =>
      req<{ message: string; project_id: string; provider_id: string }>(
        'DELETE', `/projects/${projectId}/provider-access/${providerId}`),
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
