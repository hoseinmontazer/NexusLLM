'use client'

// Models — list, deploy, lifecycle (enable/disable/drain/archive/restore/delete),
// per-endpoint health, start/stop/restart, and reset-health.
//
// This page was missing: the sidebar + dashboard linked to /models but no route
// existed, so every "Models" link 404'd. It now wires up the full models API
// surface exposed in lib/api.ts.

import { useState } from 'react'
import Link from 'next/link'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  api, type Model, type Endpoint, type DeployModelInput,
} from '@/lib/api'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger,
} from '@/components/ui/dialog'
import { toast } from '@/components/ui/toaster'
import {
  Cpu, RefreshCw, Plus, Filter, Play, Square, RotateCw,
  CheckCircle2, AlertTriangle, XCircle, Loader2, Archive, Trash2,
  Power, PowerOff, Droplets, Stethoscope, ChevronDown, ChevronRight,
  Box, Sparkles, Shield, Settings,
} from 'lucide-react'

// ── constants ──────────────────────────────────────────────────────────────────

// Universal model types — every AI workload is one of these
const MODEL_TYPES = [
  { value: 'CHAT',             label: 'LLM / Chat',        desc: 'Text generation, chat completion',       icon: '💬', color: 'bg-blue-100 text-blue-700' },
  { value: 'STT',              label: 'Speech-to-Text',    desc: 'Audio transcription (Whisper, Chirp)',    icon: '🎤', color: 'bg-teal-100 text-teal-700' },
  { value: 'TTS',              label: 'Text-to-Speech',    desc: 'Audio synthesis (Kokoro, XTTS)',          icon: '🔊', color: 'bg-cyan-100 text-cyan-700' },
  { value: 'OCR',              label: 'OCR',               desc: 'Optical character recognition',          icon: '📄', color: 'bg-orange-100 text-orange-700' },
  { value: 'EMBEDDING',        label: 'Embedding',         desc: 'Text embeddings (BGE, E5, Jina)',         icon: '🧮', color: 'bg-purple-100 text-purple-700' },
  { value: 'RERANK',           label: 'Reranker',          desc: 'Cross-encoder reranking',                icon: '↕️',  color: 'bg-indigo-100 text-indigo-700' },
  { value: 'VISION',           label: 'Vision',            desc: 'Multimodal image+text (Qwen-VL, LLaVA)', icon: '👁️',  color: 'bg-pink-100 text-pink-700' },
  { value: 'IMAGE_GENERATION', label: 'Image Generation',  desc: 'Stable Diffusion, FLUX, DALL-E compat',  icon: '🎨', color: 'bg-rose-100 text-rose-700' },
  { value: 'CUSTOM',           label: 'Custom',            desc: 'Any OpenAI-compatible HTTP API',         icon: '🔧', color: 'bg-gray-100 text-gray-700' },
] as const

type ModelTypeValue = (typeof MODEL_TYPES)[number]['value']

// Canonical capability identifiers (match models.capabilities JSONB values in DB)
// These are the actual string values stored in the database and enforced by the gateway.
const CAPABILITY_DEFS: Record<string, { label: string; endpoint: string; color: string }> = {
  chat:             { label: 'Chat Completions',    endpoint: 'POST /v1/chat/completions',     color: 'bg-blue-100 text-blue-700 border-blue-200' },
  completion:       { label: 'Text Completions',    endpoint: 'POST /v1/completions',           color: 'bg-sky-100 text-sky-700 border-sky-200' },
  responses:        { label: 'Responses',           endpoint: 'POST /v1/responses',             color: 'bg-indigo-100 text-indigo-700 border-indigo-200' },
  embedding:        { label: 'Embeddings',          endpoint: 'POST /v1/embeddings',            color: 'bg-purple-100 text-purple-700 border-purple-200' },
  rerank:           { label: 'Rerank',              endpoint: 'POST /v1/rerank',                color: 'bg-violet-100 text-violet-700 border-violet-200' },
  transcription:    { label: 'Audio Transcription', endpoint: 'POST /v1/audio/transcriptions',  color: 'bg-teal-100 text-teal-700 border-teal-200' },
  speech:           { label: 'Audio Speech',        endpoint: 'POST /v1/audio/speech',          color: 'bg-cyan-100 text-cyan-700 border-cyan-200' },
  image_generation: { label: 'Image Generation',    endpoint: 'POST /v1/images/generations',   color: 'bg-rose-100 text-rose-700 border-rose-200' },
  moderation:       { label: 'Moderation',          endpoint: 'POST /v1/moderations',           color: 'bg-orange-100 text-orange-700 border-orange-200' },
  ocr:              { label: 'OCR',                 endpoint: 'POST /v1/ocr',                   color: 'bg-amber-100 text-amber-700 border-amber-200' },
  vision:           { label: 'Vision (multimodal)', endpoint: 'POST /v1/chat/completions',      color: 'bg-pink-100 text-pink-700 border-pink-200' },
}

// Default capabilities per service_type — mirrors backend DefaultCapabilities() in runtime/backend.go
// and migration 033. These are what the server will use when no explicit capabilities are sent.
const DEFAULT_CAPABILITIES_BY_TYPE: Record<string, string[]> = {
  CHAT:             ['chat', 'completion'],
  STT:              ['transcription'],
  TTS:              ['speech'],
  OCR:              ['ocr'],
  EMBEDDING:        ['embedding'],
  RERANK:           ['rerank'],
  VISION:           ['chat', 'vision'],
  IMAGE_GENERATION: ['image_generation'],
  MODERATION:       ['moderation'],
  AGENT:            ['chat', 'completion'],
  MCP:              ['chat', 'completion'],
  CUSTOM:           [],
}

// Human-readable endpoint list for the Type selection step (display only)
const CAPABILITIES_BY_TYPE: Record<string, string[]> = {
  CHAT:             ['POST /v1/chat/completions', 'POST /v1/completions'],
  STT:              ['POST /v1/audio/transcriptions'],
  TTS:              ['POST /v1/audio/speech'],
  OCR:              ['POST /v1/ocr'],
  EMBEDDING:        ['POST /v1/embeddings'],
  RERANK:           ['POST /v1/rerank'],
  VISION:           ['POST /v1/chat/completions (image input)'],
  IMAGE_GENERATION: ['POST /v1/images/generations'],
  CUSTOM:           ['configurable'],
}

// Backends available per model type
const BACKENDS_BY_TYPE: Record<string, string[]> = {
  CHAT:             ['llamacpp', 'vllm', 'tgi', 'openai_compat'],
  STT:              ['cpu_native', 'openai_compat'],
  TTS:              ['cpu_native', 'openai_compat'],
  OCR:              ['cpu_native', 'openai_compat'],
  EMBEDDING:        ['vllm', 'cpu_native', 'openai_compat'],   // vllm (GPU), infinity/bge (cpu_native), hosted APIs
  RERANK:           ['vllm', 'cpu_native', 'openai_compat'],   // vllm rerank, bge-reranker, hosted
  VISION:           ['llamacpp', 'vllm', 'openai_compat'],
  IMAGE_GENERATION: ['openai_compat'],
  CUSTOM:           ['openai_compat', 'llamacpp', 'vllm', 'tgi'],
}

// Default backend image suggestions per type
const DEFAULT_IMAGES: Record<string, string> = {
  CHAT:             'ghcr.io/ggml-org/llama.cpp:server-cuda',
  STT:              'fedirz/faster-whisper-server:latest-cpu',
  TTS:              'ghcr.io/remsky/kokoro-fastapi-cpu:latest',
  OCR:              'vikParuchuri/surya:latest',
  EMBEDDING:        'michaelf34/infinity:latest',
  RERANK:           'ghcr.io/huggingface/text-embeddings-inference:cpu-1.5',
  VISION:           'ghcr.io/ggml-org/llama.cpp:server-cuda',
  IMAGE_GENERATION: 'ghcr.io/invoke-ai/invokeai:latest',
  CUSTOM:           '',
}

// Default health paths per type
const HEALTH_PATHS: Record<string, string> = {
  STT: '/health',  TTS: '/health',  OCR: '/health',
  EMBEDDING: '/health',  RERANK: '/health',
}

const BACKEND_TYPES = ['vllm', 'llamacpp', 'tgi', 'openai_compat']
const EXECUTION_TYPES = ['GPU', 'CPU', 'ANY']
const PRIORITIES = ['critical', 'high', 'normal', 'low', 'best_effort']

const RUNTIME_COLORS: Record<string, string> = {
  vllm:          'bg-green-100 text-green-700',
  llamacpp:      'bg-purple-100 text-purple-700',
  tgi:           'bg-blue-100 text-blue-700',
  openai_compat: 'bg-cyan-100 text-cyan-700',
  cpu_native:    'bg-teal-100 text-teal-700',
}

// Map service_type → color for the model list
const TYPE_COLORS: Record<string, string> = {
  CHAT:             'bg-blue-100 text-blue-700',
  STT:              'bg-teal-100 text-teal-700',
  TTS:              'bg-cyan-100 text-cyan-700',
  OCR:              'bg-orange-100 text-orange-700',
  EMBEDDING:        'bg-purple-100 text-purple-700',
  RERANK:           'bg-indigo-100 text-indigo-700',
  VISION:           'bg-pink-100 text-pink-700',
  IMAGE_GENERATION: 'bg-rose-100 text-rose-700',
  CUSTOM:           'bg-gray-100 text-gray-700',
  AGENT:            'bg-pink-100 text-pink-700',
  MCP:              'bg-gray-100 text-gray-700',
}

// ── helpers ────────────────────────────────────────────────────────────────────

function fmtContext(n?: number) {
  if (!n || n <= 0) return '—'
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `${Math.round(n / 1_000)}K`
  return String(n)
}

function LifecycleBadge({ lifecycle }: { lifecycle: string }) {
  const map: Record<string, string> = {
    active:   'bg-green-100 text-green-700',
    archived: 'bg-gray-100 text-gray-500',
    deleted:  'bg-red-100 text-red-500',
  }
  return (
    <span className={`text-[10px] px-1.5 py-0.5 rounded-full font-medium uppercase ${map[lifecycle] ?? 'bg-gray-100 text-gray-600'}`}>
      {lifecycle}
    </span>
  )
}

function HealthPill({ healthy, total }: { healthy: number; total: number }) {
  if (total === 0)
    return <span className="text-xs text-muted-foreground">no endpoints</span>
  if (healthy === total)
    return <span className="inline-flex items-center gap-1 text-xs font-medium text-green-700"><CheckCircle2 className="w-3 h-3" />{healthy}/{total}</span>
  if (healthy === 0)
    return <span className="inline-flex items-center gap-1 text-xs font-medium text-red-500"><XCircle className="w-3 h-3" />{healthy}/{total}</span>
  return <span className="inline-flex items-center gap-1 text-xs font-medium text-yellow-600"><AlertTriangle className="w-3 h-3" />{healthy}/{total}</span>
}

function EndpointStateBadge({ state }: { state: string }) {
  const running = ['active', 'ready', 'warm', 'idle']
  const starting = ['loading_model', 'waiting_ready', 'starting', 'pending', 'downloading', 'validating', 'recovering']
  const failed = ['lost', 'failed', 'unhealthy', 'stopped']
  if (running.includes(state))
    return <span className="inline-flex items-center gap-1 text-xs font-medium text-green-700"><CheckCircle2 className="w-3 h-3" />{state}</span>
  if (starting.includes(state))
    return <span className="inline-flex items-center gap-1 text-xs font-medium text-blue-600"><Loader2 className="w-3 h-3 animate-spin" />{state}</span>
  if (failed.includes(state))
    return <span className="inline-flex items-center gap-1 text-xs font-medium text-red-500"><XCircle className="w-3 h-3" />{state}</span>
  return <span className="text-xs text-muted-foreground">{state}</span>
}

// ── Deploy form — multi-step wizard ───────────────────────────────────────────

const LLAMACPP_IMAGE = 'ghcr.io/ggml-org/llama.cpp:server-cuda'
const VLLM_IMAGE     = 'vllm/vllm-openai:latest'

type WizardStep = 'type' | 'model' | 'runtime' | 'placement' | 'review'

function StepIndicator({ current, steps }: { current: WizardStep; steps: { key: WizardStep; label: string }[] }) {
  const idx = steps.findIndex(s => s.key === current)
  return (
    <div className="flex items-center gap-0 mb-5">
      {steps.map((s, i) => (
        <div key={s.key} className="flex items-center">
          <div className={`flex items-center gap-1.5 px-2 py-1 rounded text-xs font-medium transition-colors ${
            i === idx ? 'bg-blue-600 text-white' :
            i < idx  ? 'text-green-700' : 'text-muted-foreground'
          }`}>
            <span className={`w-4 h-4 rounded-full flex items-center justify-center text-[10px] font-bold ${
              i === idx ? 'bg-white text-blue-600' :
              i < idx  ? 'bg-green-100 text-green-700' : 'bg-gray-100 text-gray-400'
            }`}>{i < idx ? '✓' : i + 1}</span>
            {s.label}
          </div>
          {i < steps.length - 1 && <div className="w-4 h-px bg-gray-200 mx-1" />}
        </div>
      ))}
    </div>
  )
}

function DeployModelForm({ onDone }: { onDone: () => void }) {
  const [step, setStep] = useState<WizardStep>('type')

  // Step 0 — Model type (universal)
  const [modelType, setModelType] = useState<ModelTypeValue>('CHAT')

  // Step 1 — Model identity
  const [name, setName] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [provider, setProvider] = useState('')
  const [backendType, setBackendType] = useState('llamacpp')

  // Step 2 — Runtime config
  const [image, setImage] = useState(LLAMACPP_IMAGE)
  const [hfRepo, setHfRepo] = useState('')
  const [hfFile, setHfFile] = useState('')
  const [localPath, setLocalPath] = useState('')
  const [ctxSize, setCtxSize] = useState('4096')
  const [gpuLayers, setGpuLayers] = useState('-1')
  const [cpuLimit, setCpuLimit] = useState('')
  const [hfModelId, setHfModelId] = useState('')
  const [tensorParallel, setTensorParallel] = useState('1')
  const [gpuMemUtil, setGpuMemUtil] = useState('0.9')
  const [maxModelLen, setMaxModelLen] = useState('')
  const [dtype, setDtype] = useState('auto')
  const [hfToken, setHfToken] = useState('')
  // Thinking / reasoning mode
  const [supportsThinking, setSupportsThinking] = useState(false)
  const [thinkingEnabled, setThinkingEnabled]   = useState(false)
  // Generic runtime config (for STT/TTS/OCR/Embedding/Rerank/Custom)
  const [extraArgs, setExtraArgs] = useState('')
  const [envVarsRaw, setEnvVarsRaw] = useState('')  // KEY=VALUE per line
  const [placementMode, setPlacementMode] = useState<'auto' | 'specific_node' | 'node_group' | 'label_selector'>('auto')
  const [specificNodeId, setSpecificNodeId] = useState('')
  const [nodeGroupId, setNodeGroupId] = useState('')
  const [labelPairs, setLabelPairs] = useState<{k: string; v: string}[]>([{ k: '', v: '' }])
  const [minVram, setMinVram] = useState('0')
  const [accelerator, setAccelerator] = useState<'any' | 'gpu' | 'cpu'>('any')
  const [executionMode, setExecutionMode] = useState<'auto' | 'cpu' | 'gpu'>('auto')
  const [replicaDist, setReplicaDist] = useState('spread')
  const [priority, setPriority] = useState('normal')
  const [startNow, setStartNow] = useState('true')
  const [bindPort, setBindPort] = useState('0')
  // Capabilities — pre-seeded from model type, freely overridable by the operator.
  // Mirrors DEFAULT_CAPABILITIES_BY_TYPE so the operator sees exactly what will be stored in the DB.
  const [selectedCaps, setSelectedCaps] = useState<string[]>(DEFAULT_CAPABILITIES_BY_TYPE['CHAT'])

  const { data: nodesData } = useQuery({ queryKey: ['nodes'], queryFn: api.nodes.list })
  const nodes = nodesData?.data ?? []

  const isLLamaCpp = backendType === 'llamacpp'
  const isVllm = backendType === 'vllm'
  const isGeneric = !isLLamaCpp && !isVllm  // STT/TTS/OCR/Embedding/Custom via openai_compat/cpu_native
  const typeInfo = MODEL_TYPES.find(t => t.value === modelType)
  const availableBackends = BACKENDS_BY_TYPE[modelType] ?? BACKEND_TYPES

  const STEPS: { key: WizardStep; label: string }[] = [
    { key: 'type',      label: 'Type' },
    { key: 'model',     label: 'Identity' },
    { key: 'runtime',   label: 'Runtime' },
    { key: 'placement', label: 'Placement' },
    { key: 'review',    label: 'Review' },
  ]

  const mut = useMutation({
    mutationFn: () => {
      const nodeSelector = placementMode === 'label_selector'
        ? Object.fromEntries(labelPairs.filter(p => p.k && p.v).map(p => [p.k, p.v]))
        : undefined

      // Parse env vars from KEY=VALUE lines
      const envVars: Record<string, string> = {}
      envVarsRaw.split('\n').forEach(line => {
        const eq = line.indexOf('=')
        if (eq > 0) envVars[line.slice(0, eq).trim()] = line.slice(eq + 1).trim()
      })

      // Auto-inject port env vars for services that need them.
      // faster-whisper-server reads UVICORN_PORT; kokoro reads UVICORN_PORT too.
      // Only inject if the user hasn't already set it manually.
      const port = parseInt(bindPort) || 0
      if (port > 0 && isGeneric) {
        if (!envVars['UVICORN_PORT']) envVars['UVICORN_PORT'] = String(port)
      }

      // A node-backed deployment must never manufacture a network address —
      // the selected node is the source of placement intent, and the backend
      // resolves its canonical reachable address itself. `host` is only
      // meaningful for a pure local/no-node deployment, so it's omitted here
      // whenever a node is being targeted (see internal/nodeaddr.CanonicalHost
      // and the DeployModel bind-host invariant it enforces).
      const isNodeBackedDeploy = placementMode === 'specific_node' && !!specificNodeId

      const body: DeployModelInput = {
        name, display_name: displayName || name,
        provider: provider || undefined,
        service_type: modelType,
        backend_type: backendType,
        image: image || DEFAULT_IMAGES[modelType] || undefined,
        host: isNodeBackedDeploy ? undefined : 'localhost', port: parseInt(bindPort) || 0,
        hf_token: hfToken || undefined,
        placement_mode: placementMode,
        specific_node_id: placementMode === 'specific_node' ? specificNodeId || undefined : undefined,
        node_id:          placementMode === 'specific_node' ? specificNodeId || undefined : undefined,
        node_group_id:    placementMode === 'node_group' ? nodeGroupId || undefined : undefined,
        node_selector:    nodeSelector,
        auto_place:       placementMode === 'auto',
        min_vram_mb:      parseInt(minVram) || 0,
        accelerator_type: accelerator,
        replica_distribution: replicaDist as any,
        priority, start_now: startNow === 'true',
        // LLM-specific
        ...(isVllm ? {
          hf_model_id:     hfModelId || undefined,
          tensor_parallel: parseInt(tensorParallel) || 1,
          gpu_memory_util: parseFloat(gpuMemUtil) || 0.9,
          max_model_len:   maxModelLen ? parseInt(maxModelLen) : undefined,
          dtype,
        } : {}),
        ...(isLLamaCpp ? {
          llamacpp_hf_repo:      hfRepo || undefined,
          llamacpp_hf_file:      hfFile || undefined,
          llamacpp_model_path:   localPath || undefined,
          llamacpp_ctx_size:     parseInt(ctxSize) || 4096,
          llamacpp_n_gpu_layers: parseInt(gpuLayers) ?? -1,
          supports_thinking:     supportsThinking,
          thinking_enabled:      supportsThinking ? thinkingEnabled : false,
          min_thinking_tokens:   supportsThinking ? 500 : undefined,
        } : {}),
        // Generic extra args / env (STT, TTS, OCR, Embedding, etc.)
        ...(isGeneric && extraArgs.trim() ? {
          extra_args: extraArgs.split(/\s+/).filter(Boolean),
        } : {}),
        ...(Object.keys(envVars).length > 0 ? { env: envVars } : {}),
        // execution_mode: cpu for STT/TTS/Embedding/Rerank/OCR, gpu for vllm/tgi
        execution_mode: executionMode !== 'auto' ? executionMode : undefined,
        // Capabilities — always send the explicit list so the gateway never has to
        // fall back to service_type derivation. The operator saw and confirmed these.
        capabilities: selectedCaps.length > 0 ? selectedCaps : undefined,
      }
      return api.models.deploy(body)
    },
    onSuccess: (r) => {
      toast({ title: 'Model deployed', description: r.started ? `${name} — container starting…` : `${name} registered` })
      onDone()
    },
    onError: (e: any) => toast({ title: 'Deploy failed', description: e.message, variant: 'destructive' }),
  })

  // ── Step 0: Model type ────────────────────────────────────────────────────
  const renderType = () => (
    <div className="space-y-3">
      <p className="text-xs text-muted-foreground">
        Choose what kind of AI model you are deploying. Every type is managed identically — same runtime, same lifecycle, same policies.
      </p>
      <div className="grid grid-cols-1 gap-2">
        {MODEL_TYPES.map(t => (
          <label key={t.value} className={`flex items-center gap-3 border rounded-lg px-3 py-2.5 cursor-pointer transition-all ${
            modelType === t.value ? 'border-blue-500 bg-blue-50 shadow-sm' : 'hover:border-gray-300 hover:bg-gray-50'
          }`}>
            <input type="radio" name="svctype" value={t.value} checked={modelType === t.value}
              onChange={() => {
                setModelType(t.value as ModelTypeValue)
                // Set sensible defaults for the selected type
                const backends = BACKENDS_BY_TYPE[t.value] ?? ['openai_compat']
                if (!backends.includes(backendType)) setBackendType(backends[0])
                // Set default image based on backend priority for this type
                const effectiveBackend = backends.includes(backendType) ? backendType : backends[0]
                if (effectiveBackend === 'llamacpp') setImage(LLAMACPP_IMAGE)
                else if (effectiveBackend === 'vllm') setImage(VLLM_IMAGE)
                else setImage(DEFAULT_IMAGES[t.value] ?? '')
                // Reset capabilities to match the new type's defaults
                setSelectedCaps(DEFAULT_CAPABILITIES_BY_TYPE[t.value] ?? [])
                // Auto-set execution mode: CPU for STT/TTS/OCR; GPU for vLLM-based EMBEDDING/RERANK
                const cpuTypes = ['STT', 'TTS', 'OCR']
                setExecutionMode(cpuTypes.includes(t.value) ? 'cpu' : 'auto')
                if (cpuTypes.includes(t.value)) setAccelerator('cpu')
              }}
              className="shrink-0"
            />
            <span className="text-lg leading-none">{t.icon}</span>
            <div className="flex-1 min-w-0">
              <div className="flex items-center gap-2 flex-wrap">
                <span className="text-sm font-semibold">{t.label}</span>
                <span className={`text-[10px] px-1.5 py-0.5 rounded-full font-medium ${t.color}`}>{t.value}</span>
                {/* Show API endpoints that this type will serve */}
                {(CAPABILITIES_BY_TYPE[t.value] ?? []).map(cap => (
                  <span key={cap} className="text-[10px] px-1.5 py-0.5 rounded font-mono bg-gray-100 text-gray-600">{cap}</span>
                ))}
              </div>
              <p className="text-xs text-muted-foreground truncate">{t.desc}</p>
            </div>
          </label>
        ))}
      </div>
      <div className="flex justify-end pt-2">
        <Button onClick={() => setStep('model')}>Next: Identity →</Button>
      </div>
    </div>
  )

  // ── Step 1: Model identity ─────────────────────────────────────────────────
  const renderModel = () => (
    <div className="space-y-3">
      {typeInfo && (
        <div className={`flex items-center gap-2 px-3 py-2 rounded-lg text-sm font-medium ${typeInfo.color}`}>
          <span>{typeInfo.icon}</span>
          <span>{typeInfo.label}</span>
          <span className="ml-auto text-xs opacity-70">{typeInfo.desc}</span>
        </div>
      )}
      <div>
        <Label>Model name *</Label>
        <Input value={name} onChange={e => setName(e.target.value)}
          placeholder={modelType === 'STT' ? 'whisper-large-v3' : modelType === 'TTS' ? 'kokoro-en' : modelType === 'EMBEDDING' ? 'bge-m3' : 'my-model'}
          required className="mt-1" />
        <p className="text-xs text-muted-foreground mt-0.5">Clients send this name in the <code>model</code> field</p>
      </div>
      <div>
        <Label>Display name</Label>
        <Input value={displayName} onChange={e => setDisplayName(e.target.value)}
          placeholder={typeInfo?.label ?? 'My Model'} className="mt-1" />
      </div>
      <div className="grid grid-cols-2 gap-3">
        <div>
          <Label>Backend *</Label>
          <div className="grid grid-cols-1 gap-1.5 mt-1">
            {availableBackends.map(b => (
              <label key={b} className={`flex items-center gap-2 border rounded px-3 py-2 cursor-pointer text-sm transition-colors ${
                backendType === b ? 'border-blue-500 bg-blue-50' : 'hover:bg-gray-50'
              }`}>
                <input type="radio" name="backend" value={b} checked={backendType === b}
                  onChange={() => {
                    setBackendType(b)
                    if (b === 'llamacpp') setImage(LLAMACPP_IMAGE)
                    else if (b === 'vllm') setImage(VLLM_IMAGE)
                    else setImage(DEFAULT_IMAGES[modelType] ?? '')
                  }} className="shrink-0" />
                <span className={`text-[10px] px-1.5 py-0.5 rounded-full font-medium ${RUNTIME_COLORS[b] ?? 'bg-gray-100 text-gray-600'}`}>{b}</span>
              </label>
            ))}
          </div>
        </div>
        <div>
          <Label>Provider</Label>
          <Input value={provider} onChange={e => setProvider(e.target.value)} placeholder="google" className="mt-1" />
          <p className="text-xs text-muted-foreground mt-1">Optional metadata</p>
        </div>
      </div>
      <div className="flex justify-between pt-2">
        <Button variant="outline" onClick={() => setStep('type')}>← Back</Button>
        <Button onClick={() => setStep('runtime')} disabled={!name}>Next: Runtime →</Button>
      </div>
    </div>
  )

  // ── Step 2: Runtime config ─────────────────────────────────────────────────
  const renderRuntime = () => (
    <div className="space-y-4">
      {isLLamaCpp && (
        <>
          <div>
            <Label>Docker image</Label>
            <Input value={image} onChange={e => setImage(e.target.value)}
              className="mt-1 font-mono text-xs" />
            <p className="text-xs text-muted-foreground mt-0.5">Default: <code>{LLAMACPP_IMAGE}</code></p>
          </div>
          <div className="rounded-md border p-3 space-y-3">
            <p className="text-xs font-medium text-muted-foreground">Model source — HF download <span className="font-normal">(or use local path below)</span></p>
            <div>
              <Label>HF repo</Label>
              <Input value={hfRepo} onChange={e => setHfRepo(e.target.value)}
                placeholder="bartowski/gemma-2-2b-it-GGUF" className="mt-1" />
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div>
                <Label>GGUF file</Label>
                <Input value={hfFile} onChange={e => setHfFile(e.target.value)}
                  placeholder="*Q4_K_M.gguf" className="mt-1" />
              </div>
              <div>
                <Label>Local model path</Label>
                <Input value={localPath} onChange={e => setLocalPath(e.target.value)}
                  placeholder="/models/model.gguf" className="mt-1" />
              </div>
            </div>
          </div>
          <div className="grid grid-cols-3 gap-3">
            <div>
              <Label>Context size</Label>
              <Input type="number" value={ctxSize} onChange={e => setCtxSize(e.target.value)} className="mt-1" />
            </div>
            <div>
              <Label>GPU layers <span className="text-muted-foreground font-normal">(-1=all)</span></Label>
              <Input type="number" value={gpuLayers} onChange={e => setGpuLayers(e.target.value)} className="mt-1" />
            </div>
            <div>
              <Label>CPU threads</Label>
              <Input type="number" value={cpuLimit} onChange={e => setCpuLimit(e.target.value)} placeholder="auto" className="mt-1" />
            </div>
          </div>
          {/* Thinking / reasoning mode */}
          <div className="rounded-md border p-3 space-y-2">
            <p className="text-xs font-medium">Reasoning / Thinking mode</p>
            <label className="flex items-center gap-2 cursor-pointer text-sm select-none">
              <input
                type="checkbox"
                checked={supportsThinking}
                onChange={e => {
                  setSupportsThinking(e.target.checked)
                  if (!e.target.checked) setThinkingEnabled(false)
                }}
                className="h-4 w-4"
              />
              <span>Model supports thinking <span className="text-muted-foreground font-normal">(Qwen3, DeepSeek-R1, Qwythos…)</span></span>
            </label>
            {supportsThinking && (
              <label className="flex items-center gap-2 cursor-pointer text-sm select-none pl-6">
                <input
                  type="checkbox"
                  checked={thinkingEnabled}
                  onChange={e => setThinkingEnabled(e.target.checked)}
                  className="h-4 w-4"
                />
                <span>Enable thinking by default
                  <span className="text-muted-foreground font-normal ml-1">
                    (uncheck = <code className="bg-gray-100 px-1 rounded">--reasoning off</code> injected automatically)
                  </span>
                </span>
              </label>
            )}
            {supportsThinking && !thinkingEnabled && (
              <p className="text-xs text-amber-700 bg-amber-50 border border-amber-100 rounded px-2 py-1 pl-6">
                ⚠️ Thinking disabled — <code>--reasoning off</code> will be added to container startup args. Clients won&apos;t receive reasoning tokens.
              </p>
            )}
          </div>
        </>
      )}
      {isVllm && (
        <>
          <div>
            <Label>Docker image</Label>
            <Input value={image} onChange={e => setImage(e.target.value)}
              placeholder={VLLM_IMAGE} className="mt-1 font-mono text-xs" />
            <p className="text-xs text-muted-foreground mt-0.5">Default: <code>{VLLM_IMAGE}</code></p>
          </div>
          <div>
            <Label>HuggingFace model ID *</Label>
            <Input value={hfModelId} onChange={e => setHfModelId(e.target.value)}
              placeholder="sentence-transformers/all-MiniLM-L6-v2" className="mt-1" />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <Label>Tensor parallel</Label>
              <Input type="number" min={1} value={tensorParallel} onChange={e => setTensorParallel(e.target.value)} className="mt-1" />
            </div>
            <div>
              <Label>GPU memory util</Label>
              <Input type="number" step="0.05" min="0.1" max="1.0" value={gpuMemUtil} onChange={e => setGpuMemUtil(e.target.value)} className="mt-1" />
            </div>
            <div>
              <Label>Max model len</Label>
              <Input type="number" value={maxModelLen} onChange={e => setMaxModelLen(e.target.value)} placeholder="auto" className="mt-1" />
            </div>
            <div>
              <Label>Dtype</Label>
              <select className="w-full border rounded-md h-9 px-3 text-sm mt-1" value={dtype} onChange={e => setDtype(e.target.value)}>
                {['auto', 'float16', 'bfloat16', 'float32'].map(t => <option key={t}>{t}</option>)}
              </select>
            </div>
          </div>
          <div>
            <Label>HF token <span className="text-muted-foreground font-normal">(gated models)</span></Label>
            <Input type="password" value={hfToken} onChange={e => setHfToken(e.target.value)} placeholder="hf_…" className="mt-1" />
          </div>
        </>
      )}
      {/* Generic config — STT, TTS, OCR, Embedding, Rerank, Custom, etc. */}
      {isGeneric && (
        <div className="space-y-3">
          <div className={`flex items-center gap-2 px-3 py-2 rounded text-xs font-medium ${typeInfo?.color ?? 'bg-gray-100 text-gray-700'}`}>
            {typeInfo?.icon} Deploying a <strong>{typeInfo?.label}</strong> model via <code className="bg-white/60 px-1 rounded">{backendType}</code> adapter — no backend-specific code needed.
          </div>
          <div>
            <Label>Docker image *</Label>
            <Input value={image} onChange={e => setImage(e.target.value)}
              placeholder={DEFAULT_IMAGES[modelType] ?? 'registry/image:tag'}
              className="mt-1 font-mono text-xs" />
            <p className="text-xs text-muted-foreground mt-0.5">
              Suggested: <code>{DEFAULT_IMAGES[modelType] ?? 'any OpenAI-compatible HTTP server'}</code>
            </p>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <Label>Bind port</Label>
              <Input type="number" value={bindPort} onChange={e => setBindPort(e.target.value)}
                placeholder="8100" className="mt-1" />
              <p className="text-xs text-muted-foreground mt-0.5">
                Port the container listens on. Auto-injected as <code className="bg-gray-100 px-1 rounded">UVICORN_PORT</code>.
              </p>
            </div>
            <div>
              <Label>Execution mode</Label>
              <select className="w-full border rounded-md h-9 px-3 text-sm mt-1"
                value={executionMode} onChange={e => setExecutionMode(e.target.value as any)}>
                <option value="cpu">CPU (no GPU)</option>
                <option value="gpu">GPU</option>
                <option value="auto">Auto (detect)</option>
              </select>
              <p className="text-xs text-muted-foreground mt-0.5">
                CPU = standard runtime. GPU = nvidia runtime + --gpus.
              </p>
            </div>
          </div>
          <div>
            <Label>Extra args <span className="text-muted-foreground font-normal">(space-separated, appended to entrypoint)</span></Label>
            <Input value={extraArgs} onChange={e => setExtraArgs(e.target.value)}
              placeholder="--model whisper-large-v3 --language en"
              className="mt-1 font-mono text-xs" />
          </div>
          <div>
            <Label>HF token <span className="text-muted-foreground font-normal">(gated models)</span></Label>
            <Input type="password" value={hfToken} onChange={e => setHfToken(e.target.value)} placeholder="hf_…" className="mt-1" />
          </div>
          <div>
            <Label>Environment variables <span className="text-muted-foreground font-normal">(KEY=VALUE, one per line)</span></Label>
            <textarea value={envVarsRaw} onChange={e => setEnvVarsRaw(e.target.value)}
              placeholder={'WHISPER_MODEL=large-v3\nLANGUAGE=auto'}
              rows={3}
              className="mt-1 w-full border rounded-md px-3 py-2 text-xs font-mono resize-none focus:outline-none focus:ring-2 focus:ring-blue-300" />
          </div>
        </div>
      )}
      {/* Capabilities — which API endpoints this model supports.
          Pre-selected from the model type; operator can adjust before deploying.
          These are the exact identifiers stored in models.capabilities and enforced by the gateway. */}
      <div className="rounded-md border p-3 space-y-3">
        <div>
          <p className="text-xs font-semibold">🔌 Gateway Capabilities</p>
          <p className="text-xs text-muted-foreground mt-0.5">
            The gateway validates every request against this list before routing to the backend.
            Pre-filled from <code className="bg-gray-100 px-1 rounded">{modelType}</code> defaults — uncheck or add capabilities as needed.
          </p>
        </div>
        <div className="grid grid-cols-1 gap-1.5">
          {Object.entries(CAPABILITY_DEFS).map(([cap, meta]) => {
            const on = selectedCaps.includes(cap)
            return (
              <label
                key={cap}
                className={`flex items-center gap-3 border rounded-lg px-3 py-2 cursor-pointer transition-all ${
                  on ? `${meta.color} border-current shadow-sm` : 'hover:border-gray-300 hover:bg-gray-50'
                }`}
              >
                <input
                  type="checkbox"
                  checked={on}
                  onChange={() => setSelectedCaps(prev =>
                    prev.includes(cap) ? prev.filter(c => c !== cap) : [...prev, cap]
                  )}
                  className="h-4 w-4 shrink-0"
                />
                <div className="flex-1 min-w-0 flex items-center gap-2 flex-wrap">
                  <span className="text-sm font-medium">{meta.label}</span>
                  <code className="text-[10px] bg-white/60 px-1.5 py-0.5 rounded border border-current/20">{cap}</code>
                  <span className="text-[10px] text-muted-foreground ml-auto font-mono hidden sm:inline">{meta.endpoint}</span>
                </div>
              </label>
            )
          })}
        </div>
        {selectedCaps.length === 0 && (
          <p className="text-xs text-amber-700 bg-amber-50 border border-amber-100 rounded px-2 py-1">
            ⚠️ No capabilities selected — the gateway will reject all inference requests for this model.
          </p>
        )}
      </div>

      <div className="flex justify-between pt-2">
        <Button variant="outline" onClick={() => setStep('model')}>← Back</Button>
        <Button onClick={() => setStep('placement')}>Next: Placement →</Button>
      </div>
    </div>
  )

  // ── Step 3: Placement ──────────────────────────────────────────────────────
  const renderPlacement = () => (
    <div className="space-y-4">
      <div>
        <p className="text-xs font-semibold text-muted-foreground uppercase mb-2">Where to deploy</p>
        <div className="grid grid-cols-2 gap-2">
          {([
            { v: 'auto',           label: '🤖 Automatic',      desc: 'Scheduler picks best node' },
            { v: 'specific_node',  label: '📌 Specific Node',  desc: 'Pin to exact hardware' },
            { v: 'node_group',     label: '🖥️ Node Group',     desc: 'Any node in a group' },
            { v: 'label_selector', label: '🏷️ Label Selector', desc: 'Match by labels' },
          ] as const).map(opt => (
            <label key={opt.v}
              className={`flex items-start gap-2 border rounded-lg p-3 cursor-pointer transition-all ${
                placementMode === opt.v ? 'border-blue-500 bg-blue-50 shadow-sm' : 'hover:border-gray-300 hover:bg-gray-50'
              }`}>
              <input type="radio" name="pm" value={opt.v} checked={placementMode === opt.v}
                onChange={() => setPlacementMode(opt.v)} className="mt-0.5 shrink-0" />
              <div>
                <p className="text-xs font-semibold">{opt.label}</p>
                <p className="text-xs text-muted-foreground mt-0.5">{opt.desc}</p>
              </div>
            </label>
          ))}
        </div>
      </div>

      {/* Mode-specific input */}
      {placementMode === 'specific_node' && (
        <div className="rounded-md border p-3 space-y-2">
          <Label>Target node</Label>
          <select className="w-full border rounded-md h-9 px-3 text-sm"
            value={specificNodeId} onChange={e => setSpecificNodeId(e.target.value)}>
            <option value="">— select node —</option>
            {nodes.map(n => (
              <option key={n.id} value={n.id}>
                {n.hostname || n.id.slice(0, 8)} — {n.status}
                {n.cordoned ? ' (cordoned)' : ''}
                {(n.total_vram_mb ?? 0) > 0 ? ` · ${Math.round((n.total_vram_mb ?? 0)/1024)}GB VRAM` : ''}
                {n.total_ram_mb > 0 ? ` · ${Math.round(n.total_ram_mb/1024)}GB RAM` : ''}
              </option>
            ))}
          </select>
          {specificNodeId && (() => {
            const n = nodes.find(x => x.id === specificNodeId)
            if (!n) return null
            return (
              <div className="grid grid-cols-3 gap-2 text-xs text-muted-foreground">
                <span>🖥️ {n.total_cpu} CPUs</span>
                <span>🧠 {Math.round(n.total_ram_mb/1024)}GB RAM</span>
                {(n.total_vram_mb ?? 0) > 0 && <span>🎮 {Math.round((n.total_vram_mb ?? 0)/1024)}GB VRAM</span>}
              </div>
            )
          })()}
          <p className="text-xs text-muted-foreground">
            If the node lacks resources, deployment returns a detailed error.
          </p>
        </div>
      )}

      {placementMode === 'node_group' && (
        <div className="rounded-md border p-3 space-y-2">
          <Label>Group ID</Label>
          <Input value={nodeGroupId} onChange={e => setNodeGroupId(e.target.value)} placeholder="h200-cluster" />
          <p className="text-xs text-muted-foreground">
            Nodes need label <code className="bg-gray-100 px-1 rounded">node_group=h200-cluster</code>
          </p>
        </div>
      )}

      {placementMode === 'label_selector' && (
        <div className="rounded-md border p-3 space-y-2">
          <Label>Labels (ALL must match)</Label>
          {labelPairs.map((pair, i) => (
            <div key={i} className="flex gap-2">
              <Input value={pair.k} placeholder="key e.g. accelerator" className="flex-1 text-sm"
                onChange={e => setLabelPairs(p => p.map((x, idx) => idx === i ? { ...x, k: e.target.value } : x))} />
              <span className="self-center text-muted-foreground">=</span>
              <Input value={pair.v} placeholder="value e.g. h200" className="flex-1 text-sm"
                onChange={e => setLabelPairs(p => p.map((x, idx) => idx === i ? { ...x, v: e.target.value } : x))} />
              <Button type="button" variant="ghost" size="sm" className="text-red-400 h-9 px-2"
                onClick={() => setLabelPairs(p => p.filter((_, idx) => idx !== i))}>×</Button>
            </div>
          ))}
          <Button type="button" variant="outline" size="sm"
            onClick={() => setLabelPairs(p => [...p, { k: '', v: '' }])}>+ Add label</Button>
        </div>
      )}

      {placementMode === 'auto' && (
        <div className="grid grid-cols-2 gap-3">
          <div>
            <Label>Min VRAM (MB)</Label>
            <Input type="number" value={minVram} onChange={e => setMinVram(e.target.value)} placeholder="0 = any" className="mt-1" />
          </div>
          <div>
            <Label>Accelerator</Label>
            <select className="w-full border rounded-md h-9 px-3 text-sm mt-1" value={accelerator} onChange={e => setAccelerator(e.target.value as any)}>
              <option value="any">Any (GPU preferred)</option>
              <option value="gpu">GPU required</option>
              <option value="cpu">CPU only</option>
            </select>
          </div>
        </div>
      )}

      <div className="grid grid-cols-2 gap-3 pt-2 border-t">
        <div>
          <Label>Replica distribution</Label>
          <select className="w-full border rounded-md h-9 px-3 text-sm mt-1" value={replicaDist} onChange={e => setReplicaDist(e.target.value)}>
            <option value="spread">Spread — different nodes</option>
            <option value="anti_affinity">Anti-affinity — never same node</option>
            <option value="pack">Pack — same node</option>
          </select>
        </div>
        <div>
          <Label>Priority</Label>
          <select className="w-full border rounded-md h-9 px-3 text-sm mt-1" value={priority} onChange={e => setPriority(e.target.value)}>
            {PRIORITIES.map(p => <option key={p} value={p}>{p}</option>)}
          </select>
        </div>
        <div>
          <Label>Bind port</Label>
          <Input type="number" value={bindPort} onChange={e => setBindPort(e.target.value)}
            placeholder="0" className="mt-1" />
          <p className="text-xs text-muted-foreground mt-0.5">
            0 = agent picks a free port automatically (recommended). Set a specific port only if needed.
          </p>
        </div>
        <div>
          <Label>Start container</Label>
          <select className="w-full border rounded-md h-9 px-3 text-sm mt-1" value={startNow} onChange={e => setStartNow(e.target.value)}>
            <option value="true">Yes — start immediately</option>
            <option value="false">Register only</option>
          </select>
        </div>
      </div>

      <div className="flex justify-between pt-2">
        <Button variant="outline" onClick={() => setStep('runtime')}>← Back</Button>
        <Button onClick={() => setStep('review')}>Review →</Button>
      </div>
    </div>
  )

  // ── Step 4: Review ─────────────────────────────────────────────────────────
  const renderReview = () => {
    const selectedNode = nodes.find(n => n.id === specificNodeId)
    const labelSelector = Object.fromEntries(labelPairs.filter(p => p.k && p.v).map(p => [p.k, p.v]))
    return (
      <div className="space-y-4">
        <div className="rounded-lg border divide-y text-sm">
          <div className="px-4 py-3 flex justify-between">
            <span className="text-muted-foreground">Model</span>
            <span className="font-medium">{displayName || name}</span>
          </div>
          <div className="px-4 py-3 flex justify-between items-start">
            <span className="text-muted-foreground shrink-0">Type</span>
            <span className={`text-[10px] px-1.5 py-0.5 rounded-full font-medium ${TYPE_COLORS[modelType] ?? 'bg-gray-100 text-gray-700'}`}>
              {typeInfo?.icon} {typeInfo?.label ?? modelType}
            </span>
          </div>
          <div className="px-4 py-3 flex justify-between">
            <span className="text-muted-foreground">Name / Backend</span>
            <span><code className="bg-gray-100 px-1 rounded text-xs">{name}</code> · <span className={`text-[10px] px-1.5 py-0.5 rounded-full font-medium ${RUNTIME_COLORS[backendType] ?? 'bg-gray-100'}`}>{backendType}</span></span>
          </div>
          {provider && (
            <div className="px-4 py-3 flex justify-between">
              <span className="text-muted-foreground">Provider</span>
              <span>{provider}</span>
            </div>
          )}
          {isLLamaCpp && hfRepo && (
            <div className="px-4 py-3 flex justify-between">
              <span className="text-muted-foreground">Model source</span>
              <span className="font-mono text-xs">{hfRepo} / {hfFile || '*Q4_K_M.gguf'}</span>
            </div>
          )}
          {isLLamaCpp && localPath && (
            <div className="px-4 py-3 flex justify-between">
              <span className="text-muted-foreground">Local path</span>
              <span className="font-mono text-xs">{localPath}</span>
            </div>
          )}
          {isLLamaCpp && supportsThinking && (
            <div className="px-4 py-3 flex justify-between">
              <span className="text-muted-foreground">Thinking mode</span>
              <span className={thinkingEnabled ? 'text-blue-700 font-medium' : 'text-amber-700 font-medium'}>
                {thinkingEnabled ? 'Enabled (default on)' : 'Supported but OFF — --reasoning off injected'}
              </span>
            </div>
          )}
          {isVllm && hfModelId && (
            <div className="px-4 py-3 flex justify-between">
              <span className="text-muted-foreground">HF model</span>
              <span className="font-mono text-xs">{hfModelId}</span>
            </div>
          )}
          <div className="px-4 py-3 flex justify-between">
            <span className="text-muted-foreground">Placement</span>
            <span className="font-medium capitalize">
              {placementMode === 'auto' && `Auto · min ${minVram}MB VRAM`}
              {placementMode === 'specific_node' && (selectedNode ? selectedNode.hostname : specificNodeId || 'not selected')}
              {placementMode === 'node_group' && `Group: ${nodeGroupId || '—'}`}
              {placementMode === 'label_selector' && Object.entries(labelSelector).map(([k,v]) => `${k}=${v}`).join(', ')}
            </span>
          </div>
          <div className="px-4 py-3 flex justify-between">
            <span className="text-muted-foreground">Start now</span>
            <span className={startNow === 'true' ? 'text-green-700 font-medium' : 'text-muted-foreground'}>
              {startNow === 'true' ? 'Yes' : 'Register only'}
            </span>
          </div>
          <div className="px-4 py-3 flex justify-between items-start">
            <span className="text-muted-foreground shrink-0">Capabilities</span>
            <span className="flex flex-wrap gap-1 justify-end max-w-xs">
              {selectedCaps.length > 0 ? selectedCaps.map(cap => {
                const meta = CAPABILITY_DEFS[cap]
                return (
                  <span key={cap} className={`text-[10px] px-1.5 py-0.5 rounded-full font-medium border ${meta?.color ?? 'bg-gray-100 text-gray-700 border-gray-200'}`}>
                    {meta?.label ?? cap}
                  </span>
                )
              }) : (
                <span className="text-[10px] text-amber-600 italic">⚠️ none selected</span>
              )}
            </span>
          </div>
        </div>

        {mut.isError && (
          <div className="p-3 bg-red-50 border border-red-200 rounded text-xs text-red-700">
            {(mut.error as any)?.message}
          </div>
        )}

        <div className="flex justify-between pt-1">
          <Button variant="outline" onClick={() => setStep('placement')}>← Back</Button>
          <Button onClick={() => mut.mutate()} disabled={mut.isPending || !name}
            className="bg-blue-600 hover:bg-blue-700 text-white px-6">
            {mut.isPending ? 'Deploying…' : '🚀 Deploy Model'}
          </Button>
        </div>
      </div>
    )
  }

  return (
    <div>
      <div className="sticky top-0 bg-white z-10 pb-2">
        <StepIndicator current={step} steps={STEPS} />
      </div>
      {step === 'type'      && renderType()}
      {step === 'model'     && renderModel()}
      {step === 'runtime'   && renderRuntime()}
      {step === 'placement' && renderPlacement()}
      {step === 'review'    && renderReview()}
    </div>
  )
}
// ── Register External / Cloud Model form ─────────────────────────────────────
// Dedicated form for cloud/external provider models.
// Calls POST /admin/v1/models/external — single model registry, full policy pipeline.

const PROVIDERS = [
  { id: 'openai_provider',       label: 'OpenAI',        baseUrl: 'https://api.openai.com',                    color: 'bg-emerald-100 text-emerald-800', icon: '🟢' },
  { id: 'anthropic_provider',    label: 'Anthropic',     baseUrl: 'https://api.anthropic.com',                 color: 'bg-orange-100 text-orange-800',   icon: '🟠' },
  { id: 'google_provider',       label: 'Google Gemini', baseUrl: 'https://generativelanguage.googleapis.com', color: 'bg-blue-100 text-blue-800',       icon: '🔵' },
  { id: 'azure_openai_provider', label: 'Azure OpenAI',  baseUrl: '',                                          color: 'bg-sky-100 text-sky-800',         icon: '☁️' },
  { id: 'openrouter_provider',   label: 'OpenRouter',    baseUrl: 'https://openrouter.ai',                     color: 'bg-purple-100 text-purple-800',   icon: '🟣' },
  { id: 'groq_provider',         label: 'Groq',          baseUrl: 'https://api.groq.com',                      color: 'bg-yellow-100 text-yellow-800',   icon: '⚡' },
  { id: 'together_provider',     label: 'Together AI',   baseUrl: 'https://api.together.xyz',                  color: 'bg-teal-100 text-teal-800',       icon: '🤝' },
  { id: 'mistral_provider',      label: 'Mistral AI',    baseUrl: 'https://api.mistral.ai',                    color: 'bg-red-100 text-red-800',         icon: '🌬️' },
  { id: 'cohere_provider',       label: 'Cohere',        baseUrl: 'https://api.cohere.com',                    color: 'bg-indigo-100 text-indigo-800',   icon: '🔷' },
  { id: 'deepseek_provider',     label: 'DeepSeek',      baseUrl: 'https://api.deepseek.com',                  color: 'bg-cyan-100 text-cyan-800',       icon: '🐋' },
] as const

type ProviderId = typeof PROVIDERS[number]['id']

const PROVIDER_PRESETS: Partial<Record<ProviderId, {label:string;name:string;upstreamModel:string;svcType:string;caps:string[]}[]>> = {
  openai_provider: [
    {label:'GPT-4o',             name:'gpt-4o',                 upstreamModel:'gpt-4o',                svcType:'CHAT',      caps:['chat','completion','vision']},
    {label:'GPT-4.1',            name:'gpt-4.1',                upstreamModel:'gpt-4.1',               svcType:'CHAT',      caps:['chat','completion']},
    {label:'GPT-4.1 Mini',       name:'gpt-4.1-mini',           upstreamModel:'gpt-4.1-mini',          svcType:'CHAT',      caps:['chat','completion']},
    {label:'o4-mini',            name:'o4-mini',                upstreamModel:'o4-mini',               svcType:'CHAT',      caps:['chat','completion']},
    {label:'Whisper',            name:'whisper-1',              upstreamModel:'whisper-1',             svcType:'STT',       caps:['transcription']},
    {label:'TTS-1',              name:'tts-1',                  upstreamModel:'tts-1',                 svcType:'TTS',       caps:['speech']},
    {label:'Embed 3 Small',      name:'text-embedding-3-small', upstreamModel:'text-embedding-3-small',svcType:'EMBEDDING', caps:['embedding']},
  ],
  anthropic_provider: [
    {label:'Claude Opus 4',      name:'claude-opus-4',          upstreamModel:'claude-opus-4-5',       svcType:'CHAT',      caps:['chat','completion','vision']},
    {label:'Claude Sonnet 4',    name:'claude-sonnet-4',        upstreamModel:'claude-sonnet-4-5',     svcType:'CHAT',      caps:['chat','completion','vision']},
    {label:'Claude Haiku 3.5',   name:'claude-haiku-3-5',       upstreamModel:'claude-haiku-3-5',      svcType:'CHAT',      caps:['chat','completion']},
  ],
  google_provider: [
    {label:'Gemini 2.5 Pro',     name:'gemini-2.5-pro',         upstreamModel:'gemini-2.5-pro',        svcType:'CHAT',      caps:['chat','completion','vision']},
    {label:'Gemini 2.5 Flash',   name:'gemini-2.5-flash',       upstreamModel:'gemini-2.5-flash',      svcType:'CHAT',      caps:['chat','completion','vision']},
    {label:'Embed 004',          name:'text-embedding-004',     upstreamModel:'text-embedding-004',    svcType:'EMBEDDING', caps:['embedding']},
  ],
  groq_provider: [
    {label:'Llama 3.3 70B',      name:'groq-llama-70b',         upstreamModel:'llama-3.3-70b-versatile',svcType:'CHAT',     caps:['chat','completion']},
    {label:'Whisper Large V3',   name:'groq-whisper',           upstreamModel:'whisper-large-v3',      svcType:'STT',       caps:['transcription']},
  ],
  openrouter_provider: [
    {label:'Auto (best)',        name:'openrouter-auto',        upstreamModel:'openrouter/auto',       svcType:'CHAT',      caps:['chat','completion']},
  ],
  mistral_provider: [
    {label:'Mistral Large',      name:'mistral-large',          upstreamModel:'mistral-large-latest',  svcType:'CHAT',      caps:['chat','completion']},
    {label:'Mistral Embed',      name:'mistral-embed',          upstreamModel:'mistral-embed',         svcType:'EMBEDDING', caps:['embedding']},
  ],
  deepseek_provider: [
    {label:'DeepSeek Chat',      name:'deepseek-chat',          upstreamModel:'deepseek-chat',         svcType:'CHAT',      caps:['chat','completion']},
    {label:'DeepSeek Reasoner',  name:'deepseek-reasoner',      upstreamModel:'deepseek-reasoner',     svcType:'CHAT',      caps:['chat','completion']},
  ],
}

function RegisterExternalModelForm({ onDone }: { onDone: () => void }) {
  const qc = useQueryClient()
  const [step, setStep] = useState<'provider'|'configure'>('provider')
  const [selectedProvider, setSelectedProvider] = useState<typeof PROVIDERS[number] | null>(null)
  const [name, setName] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [svcType, setSvcType] = useState('CHAT')
  const [baseUrl, setBaseUrl] = useState('')
  const [apiKey, setApiKey] = useState('')
  const [proxyUrl, setProxyUrl] = useState('')
  const [proxyError, setProxyError] = useState('')
  const [upstreamModelName, setUpstreamModelName] = useState('')
  const [apiVersion, setApiVersion] = useState('')
  const [timeoutSec, setTimeoutSec] = useState(120)
  const [maxRetries, setMaxRetries] = useState(2)
  const [caps, setCaps] = useState<string[]>(['chat','completion'])
  const [showAdvanced, setShowAdvanced] = useState(false)

  const selectProvider = (p: typeof PROVIDERS[number]) => {
    setSelectedProvider(p); setBaseUrl(p.baseUrl); setStep('configure')
  }
  const applyPreset = (pr: {name:string;upstreamModel:string;svcType:string;caps:string[]}) => {
    if (pr.name) { setName(pr.name); setDisplayName(pr.name) }
    setUpstreamModelName(pr.upstreamModel); setSvcType(pr.svcType); setCaps(pr.caps)
  }

  // Validate proxy URL on change — give immediate feedback before submit.
  const handleProxyChange = (val: string) => {
    setProxyUrl(val)
    if (val === '') { setProxyError(''); return }
    try {
      const u = new URL(val)
      if (!['http:', 'https:', 'socks5:'].includes(u.protocol)) {
        setProxyError(`Unsupported scheme "${u.protocol.replace(':','')}". Use http, https, or socks5.`)
      } else if (!u.hostname) {
        setProxyError('Missing host in proxy URL.')
      } else {
        setProxyError('')
      }
    } catch {
      setProxyError('Invalid URL — example: http://proxy.corp:3128 or socks5://user:pass@host:1080')
    }
  }

  const mut = useMutation({
    mutationFn: () => api.models.registerExternal({
      name, display_name: displayName || name,
      provider_backend_type:    selectedProvider!.id,
      service_type:             svcType,
      upstream_api_key:         apiKey            || undefined,
      upstream_base_url:        baseUrl           || undefined,
      upstream_model_name:      upstreamModelName || undefined,
      provider_api_version:     apiVersion        || undefined,
      provider_timeout_seconds: timeoutSec,
      provider_max_retries:     maxRetries,
      capabilities:             caps,
      // Use the migration-046 proxy_url field (fully isolated per provider).
      // The legacy upstream_proxy field is not set here.
      proxy_url: proxyUrl || undefined,
    }),
    onSuccess: () => {
      toast({ title: 'Provider model registered', description: `${name} is now available in the gateway` })
      qc.invalidateQueries({ queryKey: ['models'] }); onDone()
    },
    onError: (e: any) => toast({ title: 'Registration failed', description: e.message, variant: 'destructive' }),
  })

  return (
    <div className="space-y-4 min-w-[420px]">

      {/* ── Step 1: provider picker ── */}
      {step === 'provider' && (<>
        <div className="p-3 rounded-lg bg-blue-50 border border-blue-100 text-xs text-blue-800">
          <p className="font-semibold mb-1">🌐 Register a Cloud / External Provider Model</p>
          <p>Enters the single model registry. Full policy pipeline applies — auth, rate limits, quota, audit, usage tracking. No container, no scheduler.</p>
        </div>
        <div>
          <Label className="text-xs font-semibold">Select provider</Label>
          <div className="grid grid-cols-2 gap-2 mt-2">
            {PROVIDERS.map(p => (
              <button key={p.id} type="button" onClick={() => selectProvider(p)}
                className="flex items-center gap-2 rounded-lg border px-3 py-2.5 text-left hover:border-blue-400 hover:bg-blue-50 transition-all text-xs">
                <span className="text-base leading-none">{p.icon}</span>
                <span className="font-medium">{p.label}</span>
              </button>
            ))}
          </div>
        </div>
      </>)}

      {/* ── Step 2: configuration ── */}
      {step === 'configure' && selectedProvider && (<>
        <div className={`flex items-center gap-2 px-3 py-2 rounded-lg text-xs font-semibold ${selectedProvider.color}`}>
          <span className="text-base">{selectedProvider.icon}</span>
          {selectedProvider.label}
          <button type="button" onClick={() => setStep('provider')}
            className="ml-auto text-[10px] underline opacity-70 hover:opacity-100">← change</button>
        </div>

        {/* Model presets for this provider */}
        {PROVIDER_PRESETS[selectedProvider.id as ProviderId] && (
          <div>
            <Label className="text-xs">Quick model</Label>
            <div className="flex flex-wrap gap-1.5 mt-1">
              {PROVIDER_PRESETS[selectedProvider.id as ProviderId]!.map(pr => (
                <button key={pr.name} type="button" onClick={() => applyPreset(pr)}
                  className={`text-[11px] px-2.5 py-0.5 rounded-full border transition-colors ${
                    name === pr.name ? 'bg-blue-600 text-white border-blue-600' : 'hover:bg-gray-50 border-gray-200'
                  }`}>
                  {pr.label}
                </button>
              ))}
            </div>
          </div>
        )}

        <div className="grid grid-cols-2 gap-3">
          <div>
            <Label>NexusLLM model name *</Label>
            <Input value={name} onChange={e => setName(e.target.value)}
              placeholder="gpt-4o" className="mt-1" />
            <p className="text-[11px] text-muted-foreground mt-0.5">Clients pass this in <code>model:</code></p>
          </div>
          <div>
            <Label>Display name</Label>
            <Input value={displayName} onChange={e => setDisplayName(e.target.value)}
              placeholder="OpenAI GPT-4o" className="mt-1" />
          </div>
        </div>

        <div>
          <Label>Upstream model name <span className="text-muted-foreground font-normal text-[11px]">(forwarded to provider)</span></Label>
          <Input value={upstreamModelName} onChange={e => setUpstreamModelName(e.target.value)}
            placeholder="gpt-4o" className="mt-1 font-mono text-xs" />
          <p className="text-[11px] text-muted-foreground mt-0.5">
            The model ID sent in <code>req.model</code> to the provider. Leave empty to forward the NexusLLM name.
          </p>
        </div>

        <div>
          <Label>API Key <span className="text-muted-foreground font-normal text-[11px]">(injected as Bearer / x-api-key)</span></Label>
          <Input type="password" value={apiKey} onChange={e => setApiKey(e.target.value)}
            placeholder="sk-…  /  key-…  /  AIza…" className="mt-1" />
          <p className="text-[11px] text-muted-foreground mt-0.5">Stored in DB, never returned in responses.</p>
        </div>

        {selectedProvider.id === 'azure_openai_provider' && (
          <div className="grid grid-cols-2 gap-3">
            <div>
              <Label>Resource endpoint *</Label>
              <Input value={baseUrl} onChange={e => setBaseUrl(e.target.value)}
                placeholder="https://myresource.openai.azure.com" className="mt-1 font-mono text-xs" />
            </div>
            <div>
              <Label>API version</Label>
              <Input value={apiVersion} onChange={e => setApiVersion(e.target.value)}
                placeholder="2024-08-01-preview" className="mt-1 font-mono text-xs" />
            </div>
          </div>
        )}

        {selectedProvider.id !== 'azure_openai_provider' && (
          <div>
            <Label>Base URL <span className="text-muted-foreground font-normal text-[11px]">(pre-filled from provider defaults)</span></Label>
            <Input value={baseUrl} onChange={e => setBaseUrl(e.target.value)}
              placeholder="https://api.openai.com" className="mt-1 font-mono text-xs" />
          </div>
        )}

        <div className="grid grid-cols-2 gap-3">
          <div>
            <Label>
              Proxy URL <span className="text-muted-foreground font-normal text-[11px]">(optional, per-provider)</span>
            </Label>
            <Input
              value={proxyUrl}
              onChange={e => handleProxyChange(e.target.value)}
              placeholder="socks5://192.168.0.207:3315"
              className={`mt-1 font-mono text-xs ${proxyError ? 'border-red-400 focus:ring-red-300' : ''}`}
            />
            {proxyError ? (
              <p className="text-[11px] text-red-600 mt-0.5">{proxyError}</p>
            ) : proxyUrl ? (
              <p className="text-[11px] text-green-700 mt-0.5">✓ Valid — all requests to this provider will route through this proxy</p>
            ) : (
              <p className="text-[11px] text-muted-foreground mt-0.5">
                Supports <code className="bg-gray-100 px-1 rounded">http://</code>,{' '}
                <code className="bg-gray-100 px-1 rounded">https://</code>,{' '}
                <code className="bg-gray-100 px-1 rounded">socks5://</code> — credentials optional.
                Isolated per provider — changing this never affects other providers.
              </p>
            )}
          </div>
          <div>
            <Label>Service type</Label>
            <select value={svcType} onChange={e => { setSvcType(e.target.value); setCaps((DEFAULT_CAPABILITIES_BY_TYPE as any)[e.target.value] ?? []) }}
              className="w-full border rounded-md h-9 px-3 text-sm mt-1">
              {['CHAT','STT','TTS','EMBEDDING','RERANK','VISION','IMAGE_GENERATION','MODERATION','CUSTOM'].map(t =>
                <option key={t}>{t}</option>)}
            </select>
          </div>
        </div>

        <div className="grid grid-cols-2 gap-3">
          <div>
            <Label>Timeout (s)</Label>
            <Input type="number" min={10} max={600} value={timeoutSec}
              onChange={e => setTimeoutSec(Number(e.target.value))} className="mt-1" />
          </div>
          <div>
            <Label>Max retries</Label>
            <Input type="number" min={0} max={10} value={maxRetries}
              onChange={e => setMaxRetries(Number(e.target.value))} className="mt-1" />
          </div>
        </div>

        <div>
          <Label className="text-xs">Capabilities</Label>
          <div className="flex flex-wrap gap-1.5 mt-1">
            {Object.entries(CAPABILITY_DEFS).map(([cap, meta]) => {
              const on = caps.includes(cap)
              return (
                <button key={cap} type="button"
                  onClick={() => setCaps(prev => prev.includes(cap) ? prev.filter(c => c !== cap) : [...prev, cap])}
                  className={`text-[11px] px-2 py-0.5 rounded-full border transition-all ${
                    on ? `${meta.color} border-current font-medium` : 'border-gray-200 text-muted-foreground hover:bg-gray-50'
                  }`}>
                  {on ? '✓ ' : '+ '}{cap}
                </button>
              )
            })}
          </div>
        </div>

        {/* ── Advanced transport config ── */}
        <div className="rounded-md border">
          <button
            type="button"
            onClick={() => setShowAdvanced(v => !v)}
            className="w-full flex items-center justify-between px-3 py-2 text-xs font-medium text-muted-foreground hover:bg-gray-50 transition-colors rounded-md"
          >
            <span>⚙️ Advanced transport config <span className="font-normal">(timeouts, TLS, connection pool)</span></span>
            <span>{showAdvanced ? '▲' : '▼'}</span>
          </button>
          {showAdvanced && (
            <div className="border-t px-3 py-3 space-y-3">
              <p className="text-[11px] text-muted-foreground">
                All fields are optional. Zero values apply production defaults (connect 10 s · idle 90 s · response-header 30 s · pool 32).
                These settings are isolated to this provider — changing them never affects any other provider.
              </p>
              <div className="grid grid-cols-2 gap-3">
                <div>
                  <Label className="text-xs">Connect timeout (s) <span className="text-muted-foreground font-normal">default: 10</span></Label>
                  <Input type="number" min={0} max={300} defaultValue={0}
                    id="adv-connect-timeout" className="mt-1 text-xs" placeholder="0 = default" />
                </div>
                <div>
                  <Label className="text-xs">Response header timeout (s) <span className="text-muted-foreground font-normal">default: 30</span></Label>
                  <Input type="number" min={-1} max={300} defaultValue={0}
                    id="adv-resp-header-timeout" className="mt-1 text-xs" placeholder="0 = default, -1 = off" />
                </div>
                <div>
                  <Label className="text-xs">Idle conn timeout (s) <span className="text-muted-foreground font-normal">default: 90</span></Label>
                  <Input type="number" min={0} max={600} defaultValue={0}
                    id="adv-idle-timeout" className="mt-1 text-xs" placeholder="0 = default" />
                </div>
                <div>
                  <Label className="text-xs">Max idle conns / host <span className="text-muted-foreground font-normal">default: 32</span></Label>
                  <Input type="number" min={0} max={512} defaultValue={0}
                    id="adv-max-idle" className="mt-1 text-xs" placeholder="0 = default" />
                </div>
              </div>
              <div className="flex flex-col gap-2 pt-1">
                <label className="flex items-center gap-2 cursor-pointer text-xs select-none">
                  <input type="checkbox" id="adv-disable-http2" className="h-3.5 w-3.5" />
                  <span>Disable HTTP/2 <span className="text-muted-foreground font-normal">(only if provider has HTTP/2 issues)</span></span>
                </label>
                <label className="flex items-center gap-2 cursor-pointer text-xs select-none">
                  <input type="checkbox" id="adv-tls-skip" className="h-3.5 w-3.5" />
                  <span className="text-amber-700">
                    Skip TLS verification
                    <span className="text-muted-foreground font-normal ml-1">(only for corporate MITM proxy environments)</span>
                  </span>
                </label>
              </div>
              <p className="text-[10px] text-muted-foreground bg-blue-50 border border-blue-100 rounded px-2 py-1">
                💡 These settings can also be updated after registration via{' '}
                <code className="bg-white px-1 rounded">PUT /admin/v1/models/:id/transport</code>.
              </p>
            </div>
          )}
        </div>

        {mut.isError && (
          <p className="text-xs text-red-600 bg-red-50 border border-red-200 rounded px-2 py-1.5">
            {(mut.error as any)?.message}
          </p>
        )}

        <div className="flex gap-2 pt-1">
          <Button variant="outline" onClick={() => setStep('provider')} className="flex-shrink-0">← Back</Button>
          <Button onClick={() => mut.mutate()}
            disabled={mut.isPending || !name || !!proxyError || (selectedProvider.id === 'azure_openai_provider' ? !baseUrl : false)}
            className="flex-1">
            {mut.isPending ? 'Registering…' : `🌐 Register ${selectedProvider.label} Model`}
          </Button>
        </div>
      </>)}
    </div>
  )
}

// ── Endpoint row (expandable health detail) ───────────────────────────────────

function EndpointRow({ modelId, ep }: { modelId: string; ep: Endpoint }) {
  const qc = useQueryClient()

  const start = useMutation({
    mutationFn: () => api.models.start(modelId, ep.id),
    onSuccess: () => { toast({ title: 'Endpoint starting' }); qc.invalidateQueries({ queryKey: ['model-health', modelId] }) },
    onError: (e: any) => toast({ title: 'Start failed', description: e.message, variant: 'destructive' }),
  })
  const stop = useMutation({
    mutationFn: () => api.models.stop(modelId, ep.id),
    onSuccess: () => { toast({ title: 'Endpoint stopping' }); qc.invalidateQueries({ queryKey: ['model-health', modelId] }) },
    onError: (e: any) => toast({ title: 'Stop failed', description: e.message, variant: 'destructive' }),
  })
  const restart = useMutation({
    mutationFn: () => api.models.restart(modelId, ep.id),
    onSuccess: () => { toast({ title: 'Endpoint restarting' }); qc.invalidateQueries({ queryKey: ['model-health', modelId] }) },
    onError: (e: any) => toast({ title: 'Restart failed', description: e.message, variant: 'destructive' }),
  })
  const reset = useMutation({
    mutationFn: () => api.models.resetHealth(modelId, ep.id),
    onSuccess: () => { toast({ title: 'Health reset' }); qc.invalidateQueries({ queryKey: ['model-health', modelId] }) },
    onError: (e: any) => toast({ title: 'Reset failed', description: e.message, variant: 'destructive' }),
  })

  return (
    <tr className="bg-gray-50/50">
      <td className="px-4 py-2" colSpan={6}>
        <div className="flex flex-wrap items-center gap-x-6 gap-y-1.5 pl-6 text-xs">
          <span className="font-mono text-muted-foreground">{ep.host}:{ep.port}</span>
          <span className="text-muted-foreground">endpoint <span className="font-mono">{ep.id.slice(0, 8)}</span></span>
          <span><EndpointStateBadge state={ep.lifecycle_state || ep.health_status} /></span>
          {ep.consecutive_failures > 0 && (
            <span className="text-red-500">failures: {ep.consecutive_failures}</span>
          )}
          {ep.response_time_ms != null && (
            <span className="text-muted-foreground">{Math.round(ep.response_time_ms)}ms</span>
          )}
          {ep.container_id && (
            <span className="font-mono text-muted-foreground">container {ep.container_id.slice(0, 12)}</span>
          )}
          <span className="flex items-center gap-1 ml-auto">
            <Button size="sm" variant="ghost" className="h-7 px-2 text-xs" disabled={start.isPending} onClick={() => start.mutate()}>
              <Play className="w-3 h-3" />start
            </Button>
            <Button size="sm" variant="ghost" className="h-7 px-2 text-xs" disabled={stop.isPending} onClick={() => stop.mutate()}>
              <Square className="w-3 h-3" />stop
            </Button>
            <Button size="sm" variant="ghost" className="h-7 px-2 text-xs" disabled={restart.isPending} onClick={() => restart.mutate()}>
              <RotateCw className="w-3 h-3" />restart
            </Button>
            <Button size="sm" variant="ghost" className="h-7 px-2 text-xs" disabled={reset.isPending} onClick={() => reset.mutate()}>
              <Stethoscope className="w-3 h-3" />reset health
            </Button>
          </span>
        </div>
      </td>
    </tr>
  )
}

// ── Model row ──────────────────────────────────────────────────────────────────

function ModelRow({ m, defaultOpen }: { m: Model; defaultOpen?: boolean }) {
  const qc = useQueryClient()
  const [open, setOpen] = useState(defaultOpen ?? false)

  const { data: healthData, isFetching } = useQuery({
    queryKey: ['model-health', m.id],
    queryFn: () => api.models.health(m.id),
    enabled: open,
    refetchInterval: open ? 8_000 : false,
  })
  const endpoints: Endpoint[] = healthData?.endpoints ?? []

  const enable = useMutation({
    mutationFn: () => api.models.enable(m.id),
    onSuccess: () => { toast({ title: 'Model enabled' }); qc.invalidateQueries({ queryKey: ['models'] }) },
    onError: (e: any) => toast({ title: 'Enable failed', description: e.message, variant: 'destructive' }),
  })
  const disable = useMutation({
    mutationFn: () => api.models.disable(m.id),
    onSuccess: () => { toast({ title: 'Model disabled' }); qc.invalidateQueries({ queryKey: ['models'] }) },
    onError: (e: any) => toast({ title: 'Disable failed', description: e.message, variant: 'destructive' }),
  })
  const drain = useMutation({
    mutationFn: () => api.models.drain(m.id),
    onSuccess: () => { toast({ title: 'Model draining' }); qc.invalidateQueries({ queryKey: ['models'] }) },
    onError: (e: any) => toast({ title: 'Drain failed', description: e.message, variant: 'destructive' }),
  })
  const archive = useMutation({
    mutationFn: () => api.models.archive(m.id),
    onSuccess: () => { toast({ title: 'Model archived' }); qc.invalidateQueries({ queryKey: ['models'] }) },
    onError: (e: any) => toast({ title: 'Archive failed', description: e.message, variant: 'destructive' }),
  })
  const restore = useMutation({
    mutationFn: () => api.models.restore(m.id),
    onSuccess: () => { toast({ title: 'Model restored' }); qc.invalidateQueries({ queryKey: ['models'] }) },
    onError: (e: any) => toast({ title: 'Restore failed', description: e.message, variant: 'destructive' }),
  })
  const resetAll = useMutation({
    mutationFn: () => api.models.resetHealth(m.id),
    onSuccess: () => { toast({ title: 'Health reset', description: m.name }); qc.invalidateQueries({ queryKey: ['model-health', m.id] }) },
    onError: (e: any) => toast({ title: 'Reset failed', description: e.message, variant: 'destructive' }),
  })
  const del = useMutation({
    mutationFn: () => api.models.delete(m.id),
    onSuccess: () => { toast({ title: 'Model deleted', description: m.name }); qc.invalidateQueries({ queryKey: ['models'] }) },
    onError: (e: any) => toast({ title: 'Delete failed', description: e.message, variant: 'destructive' }),
  })

  const isArchived = m.lifecycle === 'archived'

  return (
    <>
      <tr
        className={`border-b last:border-0 hover:bg-gray-50 transition-colors ${isArchived ? 'opacity-60' : ''}`}
      >
        {/* expand toggle */}
        <td className="px-2 py-3">
          <button onClick={() => setOpen(o => !o)} className="p-1 rounded hover:bg-gray-100">
            {open ? <ChevronDown className="w-3.5 h-3.5" /> : <ChevronRight className="w-3.5 h-3.5" />}
          </button>
        </td>
        {/* name + meta */}
        <td className="px-3 py-3">
          <div className="flex items-center gap-2">
            <span className="font-medium">{m.display_name || m.name}</span>
            <LifecycleBadge lifecycle={m.lifecycle} />
            {/* Universal type badge — shown for every model type */}
            <span className={`text-[10px] px-1.5 py-0.5 rounded-full font-medium ${TYPE_COLORS[m.service_type] ?? 'bg-gray-100 text-gray-600'}`}>
              {MODEL_TYPES.find(t => t.value === m.service_type)?.icon ?? '🔧'} {m.service_type}
            </span>
            {!m.enabled && <span className="text-[10px] px-1.5 py-0.5 rounded-full bg-gray-100 text-gray-500 uppercase">disabled</span>}
            {m.supports_thinking && (
              m.thinking_enabled
                ? <span className="text-[10px] px-1.5 py-0.5 rounded-full bg-purple-100 text-purple-700 font-medium">🧠 Reasoning</span>
                : <span className="text-[10px] px-1.5 py-0.5 rounded-full bg-yellow-100 text-yellow-700 font-medium">⚡ Fast</span>
            )}
          </div>
          {/* Capability chips — shown under the name when the model has explicit capabilities */}
          {m.capabilities && m.capabilities.length > 0 && (
            <div className="flex flex-wrap gap-1 mt-0.5">
              {m.capabilities.map(cap => (
                <span key={cap} className="text-[9px] px-1.5 py-0.5 rounded-full bg-slate-100 text-slate-600 font-mono font-medium border border-slate-200">
                  {cap}
                </span>
              ))}
            </div>
          )}
          <div className="text-xs text-muted-foreground font-mono">{m.name}</div>        </td>
        {/* backend / provider */}
        <td className="px-3 py-3 hidden sm:table-cell">
          {m.provider_is_external ? (
            <div className="space-y-0.5">
              <span className={`text-[10px] px-2 py-0.5 rounded-full font-medium ${
                PROVIDERS.find(p => p.id === m.provider_name)?.color ?? 'bg-gray-100 text-gray-600'
              }`}>
                {PROVIDERS.find(p => p.id === m.provider_name)?.icon ?? '🌐'}{' '}
                {PROVIDERS.find(p => p.id === m.provider_name)?.label ?? m.provider_name}
              </span>
              {(m as any).provider_catalog_id && (
                <span className="block text-[9px] px-1.5 py-0.5 rounded bg-blue-50 text-blue-700 border border-blue-200 w-fit">
                  📚 catalog alias
                </span>
              )}
            </div>
          ) : (
            <span className={`text-[10px] px-2 py-0.5 rounded-full font-medium ${RUNTIME_COLORS[m.backend_type] ?? 'bg-gray-100 text-gray-600'}`}>
              {m.backend_type}
            </span>
          )}
        </td>
        {/* context */}
        <td className="px-3 py-3 hidden md:table-cell text-xs text-muted-foreground tabular-nums">
          {fmtContext(m.max_context)} / {fmtContext(m.max_output)}
        </td>
        {/* health */}
        <td className="px-3 py-3">
          <HealthPill healthy={m.healthy_count} total={m.endpoint_count} />
        </td>
        {/* quick actions */}
        <td className="px-3 py-3">
          <div className="flex items-center justify-end gap-0.5">
            {isArchived ? (
              <>
                <Button size="icon" variant="ghost" className="h-8 w-8" title="Restore" disabled={restore.isPending} onClick={() => restore.mutate()}>
                  <RotateCw className="w-3.5 h-3.5" />
                </Button>
                <Button size="icon" variant="ghost" className="h-8 w-8 hover:text-red-600" title="Delete permanently" disabled={del.isPending} onClick={() => del.mutate()}>
                  <Trash2 className="w-3.5 h-3.5" />
                </Button>
              </>
            ) : (
              <>
                <Button size="icon" variant="ghost" className="h-8 w-8" title={m.enabled ? 'Disable' : 'Enable'}
                  disabled={enable.isPending || disable.isPending} onClick={() => (m.enabled ? disable.mutate() : enable.mutate())}>
                  {m.enabled ? <PowerOff className="w-3.5 h-3.5" /> : <Power className="w-3.5 h-3.5" />}
                </Button>
                <Button size="icon" variant="ghost" className="h-8 w-8" title="Drain" disabled={drain.isPending} onClick={() => drain.mutate()}>
                  <Droplets className="w-3.5 h-3.5" />
                </Button>
                <Button size="icon" variant="ghost" className="h-8 w-8" title="Reset health" disabled={resetAll.isPending} onClick={() => resetAll.mutate()}>
                  <Stethoscope className="w-3.5 h-3.5" />
                </Button>
                <Link href={`/models/${m.id}`}>
                  <Button size="icon" variant="ghost" className="h-8 w-8" title="Details / Change Placement">
                    <Settings className="w-3.5 h-3.5" />
                  </Button>
                </Link>
                <Button size="icon" variant="ghost" className="h-8 w-8 hover:text-red-600" title="Archive" disabled={archive.isPending} onClick={() => archive.mutate()}>
                  <Archive className="w-3.5 h-3.5" />
                </Button>
              </>
            )}
          </div>
        </td>
      </tr>

      {open && (
        <tr className="bg-gray-50/50">
          <td colSpan={6} className="px-4 pb-3 pt-1">
            <div className="rounded-md border bg-white p-3">
              <div className="flex items-center justify-between mb-2">
                <span className="text-xs font-semibold text-muted-foreground uppercase tracking-wide">
                  Endpoints {isFetching && <Loader2 className="inline w-3 h-3 ml-1 animate-spin" />}
                </span>
                {endpoints.length > 0 && (
                  <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={() => qc.invalidateQueries({ queryKey: ['model-health', m.id] })}>
                    <RefreshCw className="w-3 h-3 mr-1" />refresh
                  </Button>
                )}
              </div>
              {endpoints.length === 0 ? (
                <p className="text-xs text-muted-foreground py-3 text-center">No endpoints registered for this model.</p>
              ) : (
                <table className="w-full text-sm">
                  <tbody className="divide-y">
                    {endpoints.map(ep => <EndpointRow key={ep.id} modelId={m.id} ep={ep} />)}
                  </tbody>
                </table>
              )}
            </div>
          </td>
        </tr>
      )}
    </>
  )
}

// ── Main page ──────────────────────────────────────────────────────────────────

export default function ModelsPage() {
  const qc = useQueryClient()
  const [filter, setFilter] = useState('')
  const [lifecycleFilter, setLifecycleFilter] = useState<'active' | 'archived' | 'all'>('active')
  const [typeFilter, setTypeFilter] = useState('')
  const [openDeploy, setOpenDeploy] = useState(false)
  const [openImport, setOpenImport] = useState(false)
  const [openExternal, setOpenExternal] = useState(false)
  const [openExternalManual, setOpenExternalManual] = useState(false)

  const { data, isLoading } = useQuery({
    queryKey: ['models', lifecycleFilter],
    queryFn: () => api.models.list(lifecycleFilter === 'all' ? undefined : lifecycleFilter),
    refetchInterval: 20_000,
  })

  const modelList: Model[] = data?.data ?? []
  const filtered = modelList.filter(m =>
    (!filter ||
      m.name.toLowerCase().includes(filter.toLowerCase()) ||
      m.display_name.toLowerCase().includes(filter.toLowerCase())) &&
    (!typeFilter || m.service_type === typeFilter)
  )

  const reload = () => qc.invalidateQueries({ queryKey: ['models'] })

  const activeCount = modelList.filter(m => m.lifecycle === 'active').length
  const archivedCount = modelList.filter(m => m.lifecycle === 'archived').length
  const healthyCount = modelList.filter(m => m.healthy_count === m.endpoint_count && m.endpoint_count > 0).length

  // Count models per type for the filter pill display
  const countByType = modelList.reduce<Record<string, number>>((acc, m) => {
    acc[m.service_type] = (acc[m.service_type] ?? 0) + 1
    return acc
  }, {})

  return (
    <div className="space-y-5">
      {/* Header */}
      <div className="flex items-center justify-between flex-wrap gap-2">
        <div>
          <h1 className="text-2xl font-bold flex items-center gap-2">
            <Cpu className="w-6 h-6 text-blue-600" />Models
          </h1>
          <p className="text-sm text-muted-foreground mt-0.5">
            Every AI workload — LLM, STT, TTS, OCR, Embedding, Rerank, Vision — managed identically
          </p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" size="sm" onClick={() => setOpenExternal(true)}>
            🌐 Register Cloud/External
          </Button>
          <Dialog open={openExternal} onOpenChange={setOpenExternal}>
            <DialogContent className="max-w-lg max-h-[90vh] flex flex-col">
              <DialogHeader className="shrink-0">
                <DialogTitle>Add a Cloud Model</DialogTitle>
              </DialogHeader>
              <div className="overflow-y-auto flex-1 pr-1 space-y-3">
                <div className="grid grid-cols-2 gap-3 pt-1">
                  <a href="/providers" className="block rounded-lg border p-4 hover:border-blue-400 hover:bg-blue-50 transition-all text-left">
                    <p className="font-semibold text-sm">📚 From Catalog</p>
                    <p className="text-xs text-muted-foreground mt-1">Browse your synced provider catalogs and create a named alias. Requires a provider with catalog sync enabled.</p>
                    <p className="text-xs text-blue-600 mt-2">→ Go to Providers</p>
                  </a>
                  <button type="button" onClick={() => { setOpenExternal(false); setTimeout(() => setOpenExternalManual(true), 100) }}
                    className="rounded-lg border p-4 hover:border-gray-400 hover:bg-gray-50 transition-all text-left">
                    <p className="font-semibold text-sm">⚙️ Manual</p>
                    <p className="text-xs text-muted-foreground mt-1">Enter provider, base URL, and API key directly. Use for models not in any catalog.</p>
                    <p className="text-xs text-gray-600 mt-2">→ Manual registration</p>
                  </button>
                </div>
              </div>
            </DialogContent>
          </Dialog>

          <Dialog open={openExternalManual} onOpenChange={setOpenExternalManual}>
            <DialogContent className="max-w-lg max-h-[90vh] flex flex-col">
              <DialogHeader className="shrink-0">
                <DialogTitle>Register External / Cloud Model</DialogTitle>
              </DialogHeader>
              <div className="overflow-y-auto flex-1 pr-1">
                <RegisterExternalModelForm onDone={() => { setOpenExternalManual(false); reload() }} />
              </div>
            </DialogContent>
          </Dialog>

          <Button variant="outline" size="sm" onClick={() => setOpenDeploy(true)}>
            <Plus className="w-3.5 h-3.5 mr-1" />Deploy Model
          </Button>
          <Dialog open={openDeploy} onOpenChange={setOpenDeploy}>
            <DialogContent className="max-w-2xl max-h-[90vh] flex flex-col">
              <DialogHeader className="shrink-0">
                <DialogTitle>Deploy a new model</DialogTitle>
              </DialogHeader>
              <div className="overflow-y-auto flex-1 pr-1">
                <DeployModelForm onDone={() => { setOpenDeploy(false); reload() }} />
              </div>
            </DialogContent>
          </Dialog>

          <Button variant="outline" size="sm" onClick={reload}>
            <RefreshCw className="w-3.5 h-3.5 mr-1" />Refresh
          </Button>
        </div>
      </div>

      {/* Summary + filter bar */}
      <div className="flex items-center gap-3 flex-wrap">
        <div className="flex gap-0 border rounded-md overflow-hidden text-xs">
          {([
            { key: 'active' as const,   label: `Active (${activeCount})`,   cls: activeCount > 0 ? 'text-green-700' : '' },
            { key: 'archived' as const, label: `Archived (${archivedCount})`, cls: archivedCount > 0 ? 'text-gray-500' : '' },
          ] as const).map(f => (
            <button key={f.key} onClick={() => setLifecycleFilter(f.key)}
              className={`px-3 py-1.5 font-medium transition-colors ${
                lifecycleFilter === f.key
                  ? 'bg-gray-900 text-white'
                  : 'bg-white text-muted-foreground hover:bg-gray-50'
              } ${f.cls}`}>
              {f.label}
            </button>
          ))}
        </div>

        <div className="flex items-center gap-1.5 border rounded-md px-2.5 h-8 bg-white text-xs">
          <Filter className="w-3 h-3 text-muted-foreground" />
          <input
            className="outline-none bg-transparent w-36"
            placeholder="filter model…"
            value={filter}
            onChange={e => setFilter(e.target.value)}
          />
        </div>

        {/* Type filter pills */}
        <div className="flex items-center gap-1 flex-wrap">
          <button
            onClick={() => setTypeFilter('')}
            className={`text-[10px] px-2 py-1 rounded-full font-medium border transition-colors ${
              !typeFilter ? 'bg-gray-900 text-white border-gray-900' : 'text-muted-foreground border-transparent hover:border-gray-200'
            }`}
          >All</button>
          {MODEL_TYPES.filter(t => countByType[t.value]).map(t => (
            <button key={t.value}
              onClick={() => setTypeFilter(typeFilter === t.value ? '' : t.value)}
              className={`text-[10px] px-2 py-1 rounded-full font-medium border transition-colors ${
                typeFilter === t.value ? `${t.color} border-current` : 'text-muted-foreground border-transparent hover:border-gray-200'
              }`}
            >
              {t.icon} {t.label} <span className="opacity-60">({countByType[t.value]})</span>
            </button>
          ))}
        </div>

        <span className="text-xs text-muted-foreground ml-auto flex items-center gap-3">
          <span className="flex items-center gap-1"><Sparkles className="w-3 h-3 text-green-600" />{healthyCount} healthy</span>
          <span>{filtered.length} shown</span>
        </span>
      </div>

      {/* Table */}
      {isLoading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : filtered.length === 0 ? (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground">
            <Cpu className="w-8 h-8 mx-auto mb-2 opacity-20" />
            <p className="font-medium text-sm">
              {modelList.length === 0 ? 'No models yet' : 'No models match the filter'}
            </p>
            {modelList.length === 0 && (
              <p className="text-xs mt-1">
                Deploy your first model, or{' '}
                <Link href="/cluster" className="text-blue-600 hover:underline">check the cluster</Link>.
              </p>
            )}
          </CardContent>
        </Card>
      ) : (
        <div className="rounded-lg border bg-white overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b bg-gray-50 text-xs text-muted-foreground">
                <th className="w-8 px-2 py-2.5"></th>
                <th className="text-left px-3 py-2.5 font-medium">Model</th>
                <th className="text-left px-3 py-2.5 font-medium hidden sm:table-cell">Backend</th>
                <th className="text-left px-3 py-2.5 font-medium hidden md:table-cell">Ctx / Output</th>
                <th className="text-left px-3 py-2.5 font-medium">Health</th>
                <th className="text-right px-3 py-2.5 font-medium">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y">
              {filtered.map(m => <ModelRow key={m.id} m={m} />)}
            </tbody>
          </table>
        </div>
      )}

      <Card>
        <CardHeader>
          <CardTitle className="text-base flex items-center gap-2">
            <Shield className="w-4 h-4 text-muted-foreground" />Need replica failover?
          </CardTitle>
        </CardHeader>
        <CardContent className="text-sm text-muted-foreground">
          Configure desired replicas, minimum availability, placement policy, and auto-recovery in{' '}
          <Link href="/ha" className="text-blue-600 hover:underline">High Availability</Link>, and watch live containers in{' '}
          <Link href="/runtimes" className="text-blue-600 hover:underline">Runtimes</Link>.
        </CardContent>
      </Card>
    </div>
  )
}
