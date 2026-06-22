package handlers

// requirements.go — Runtime resource requirements API
//
// Routes:
//   POST /admin/v1/models/:id/requirements      — set requirements
//   GET  /admin/v1/models/:id/requirements      — get requirements
//   GET  /admin/v1/scheduler/compatible-nodes   — find nodes that can run a model

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// RequirementsHandler manages runtime resource requirements.
type RequirementsHandler struct {
	db *sqlx.DB
}

// NewRequirementsHandler constructs a RequirementsHandler.
func NewRequirementsHandler(db *sqlx.DB) *RequirementsHandler {
	return &RequirementsHandler{db: db}
}

// UpsertRequirements handles POST /admin/v1/models/:id/requirements
func (h *RequirementsHandler) UpsertRequirements(c *gin.Context) {
	modelID := c.Param("id")
	var input struct {
		ExecutionType     string `json:"execution_type"`   // GPU | CPU | ANY
		RequiredVRAMMB    int64  `json:"required_vram_mb"`
		GPUCount          int    `json:"gpu_count"`
		RequiredCPU       int    `json:"required_cpu"`
		RequiredMemoryMB  int64  `json:"required_memory_mb"`
		RequiresDocker    bool   `json:"requires_docker"`
		RequiresGPU       bool   `json:"requires_gpu"`
		RequiresVLLM      bool   `json:"requires_vllm"`
		RequiresOllama    bool   `json:"requires_ollama"`
		RequiresTTS       bool   `json:"requires_tts"`
		RequiresWhisper   bool   `json:"requires_whisper"`
		Priority          string `json:"priority"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.ExecutionType == "" {
		input.ExecutionType = "GPU"
	}
	if input.Priority == "" {
		input.Priority = "normal"
	}
	if input.GPUCount == 0 && input.ExecutionType == "GPU" {
		input.GPUCount = 1
	}
	if input.ExecutionType == "GPU" {
		input.RequiresGPU = true
	}

	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO runtime_requirements
		  (id, model_id, execution_type, required_vram_mb, gpu_count,
		   required_cpu, required_memory_mb,
		   requires_docker, requires_gpu, requires_vllm, requires_ollama,
		   requires_tts, requires_whisper, priority)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (model_id) DO UPDATE SET
		  execution_type    = EXCLUDED.execution_type,
		  required_vram_mb  = EXCLUDED.required_vram_mb,
		  gpu_count         = EXCLUDED.gpu_count,
		  required_cpu      = EXCLUDED.required_cpu,
		  required_memory_mb= EXCLUDED.required_memory_mb,
		  requires_docker   = EXCLUDED.requires_docker,
		  requires_gpu      = EXCLUDED.requires_gpu,
		  requires_vllm     = EXCLUDED.requires_vllm,
		  requires_ollama   = EXCLUDED.requires_ollama,
		  requires_tts      = EXCLUDED.requires_tts,
		  requires_whisper  = EXCLUDED.requires_whisper,
		  priority          = EXCLUDED.priority,
		  updated_at        = NOW()`,
		uuid.New().String(), modelID,
		input.ExecutionType, input.RequiredVRAMMB, input.GPUCount,
		input.RequiredCPU, input.RequiredMemoryMB,
		input.RequiresDocker, input.RequiresGPU, input.RequiresVLLM, input.RequiresOllama,
		input.RequiresTTS, input.RequiresWhisper, input.Priority,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "requirements saved", "model_id": modelID})
}

// GetRequirements handles GET /admin/v1/models/:id/requirements
func (h *RequirementsHandler) GetRequirements(c *gin.Context) {
	modelID := c.Param("id")
	type reqRow struct {
		ID               string    `db:"id"                json:"id"`
		ModelID          string    `db:"model_id"          json:"model_id"`
		ExecutionType    string    `db:"execution_type"    json:"execution_type"`
		RequiredVRAMMB   int64     `db:"required_vram_mb"  json:"required_vram_mb"`
		GPUCount         int       `db:"gpu_count"         json:"gpu_count"`
		RequiredCPU      int       `db:"required_cpu"      json:"required_cpu"`
		RequiredMemoryMB int64     `db:"required_memory_mb" json:"required_memory_mb"`
		RequiresDocker   bool      `db:"requires_docker"   json:"requires_docker"`
		RequiresGPU      bool      `db:"requires_gpu"      json:"requires_gpu"`
		RequiresVLLM     bool      `db:"requires_vllm"     json:"requires_vllm"`
		RequiresOllama   bool      `db:"requires_ollama"   json:"requires_ollama"`
		RequiresTTS      bool      `db:"requires_tts"      json:"requires_tts"`
		RequiresWhisper  bool      `db:"requires_whisper"  json:"requires_whisper"`
		Priority         string    `db:"priority"          json:"priority"`
		UpdatedAt        time.Time `db:"updated_at"        json:"updated_at"`
	}
	var req reqRow
	if err := h.db.GetContext(c.Request.Context(), &req, `
		SELECT id, model_id, execution_type, required_vram_mb, gpu_count,
		       required_cpu, required_memory_mb,
		       requires_docker, requires_gpu, requires_vllm, requires_ollama,
		       requires_tts, requires_whisper, priority, updated_at
		FROM runtime_requirements WHERE model_id = $1`, modelID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no requirements set for this model"})
		return
	}
	c.JSON(http.StatusOK, req)
}

// CompatibleNodes handles GET /admin/v1/scheduler/compatible-nodes?model_id=...
// Returns nodes that satisfy the model's resource requirements.
// This is what the scheduler and the deploy form use to show which nodes
// can actually run a given model.
func (h *RequirementsHandler) CompatibleNodes(c *gin.Context) {
	modelID := c.Query("model_id")
	if modelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model_id required"})
		return
	}

	// Load requirements
	var req modelRequirements
	req.ExecutionType = "ANY" // default: all nodes compatible
	type reqDB struct {
		ExecutionType    string `db:"execution_type"`
		RequiredVRAMMB   int64  `db:"required_vram_mb"`
		RequiredCPU      int    `db:"required_cpu"`
		RequiredMemoryMB int64  `db:"required_memory_mb"`
		RequiresGPU      bool   `db:"requires_gpu"`
		RequiresVLLM     bool   `db:"requires_vllm"`
		RequiresOllama   bool   `db:"requires_ollama"`
		RequiresTTS      bool   `db:"requires_tts"`
		RequiresWhisper  bool   `db:"requires_whisper"`
	}
	var row reqDB
	if err := h.db.GetContext(c.Request.Context(), &row, `
		SELECT execution_type, required_vram_mb, required_cpu, required_memory_mb,
		       requires_gpu, requires_vllm, requires_ollama, requires_tts, requires_whisper
		FROM runtime_requirements WHERE model_id = $1`, modelID); err == nil {
		req = modelRequirements{
			ExecutionType:    row.ExecutionType,
			RequiredVRAMMB:   row.RequiredVRAMMB,
			RequiredCPU:      row.RequiredCPU,
			RequiredMemoryMB: row.RequiredMemoryMB,
			RequiresGPU:      row.RequiresGPU,
			RequiresVLLM:     row.RequiresVLLM,
			RequiresOllama:   row.RequiresOllama,
			RequiresTTS:      row.RequiresTTS,
			RequiresWhisper:  row.RequiresWhisper,
		}
	}

	type nodeResult struct {
		ID          string `db:"id"            json:"id"`
		Hostname    string `db:"hostname"      json:"hostname"`
		IPAddress   string `db:"ip_address"    json:"ip_address"`
		Status      string `db:"status"        json:"status"`
		TotalVRAMMB int64  `db:"total_vram_mb" json:"total_vram_mb"`
		TotalCPU    int    `db:"total_cpu"     json:"total_cpu"`
		TotalRAMMB  int64  `db:"total_ram_mb"  json:"total_ram_mb"`
		Compatible  bool   `json:"compatible"`
		Reason      string `json:"reason"`
	}

	var nodes []nodeResult
	_ = h.db.SelectContext(c.Request.Context(), &nodes, `
		SELECT n.id, n.hostname, COALESCE(host(n.ip_address),'') AS ip_address,
		       n.status, n.total_vram_mb, n.total_cpu, n.total_ram_mb
		FROM nodes n
		WHERE n.status IN ('online','degraded')
		ORDER BY n.hostname`)

	// Score each node against requirements
	for i := range nodes {
		n := &nodes[i]
		cap := h.loadCapabilities(c.Request.Context(), n.ID)

		compat := nodeCompat{
			ID: n.ID, Hostname: n.Hostname, IPAddress: n.IPAddress,
			Status: n.Status, TotalVRAMMB: n.TotalVRAMMB,
			TotalCPU: n.TotalCPU, TotalRAMMB: n.TotalRAMMB,
		}
		mreq := modelRequirements{
			ExecutionType:    req.ExecutionType,
			RequiredVRAMMB:   req.RequiredVRAMMB,
			RequiredCPU:      req.RequiredCPU,
			RequiredMemoryMB: req.RequiredMemoryMB,
			RequiresGPU:      req.RequiresGPU,
			RequiresVLLM:     req.RequiresVLLM,
			RequiresOllama:   req.RequiresOllama,
			RequiresTTS:      req.RequiresTTS,
			RequiresWhisper:  req.RequiresWhisper,
		}
		ok, reason := checkCompatibility(mreq, compat, cap)
		n.Compatible = ok
		n.Reason = reason
	}

	compatible := make([]nodeResult, 0)
	incompatible := make([]nodeResult, 0)
	for _, n := range nodes {
		if n.Compatible {
			compatible = append(compatible, n)
		} else {
			incompatible = append(incompatible, n)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"model_id":     modelID,
		"compatible":   compatible,
		"incompatible": incompatible,
	})
}

type capRow struct {
	HasGPU      bool `db:"has_gpu"`
	HasVLLM     bool `db:"has_vllm"`
	HasOllama   bool `db:"has_ollama"`
	HasTTS      bool `db:"has_tts"`
	HasWhisper  bool `db:"has_whisper"`
	GPUCount    int  `db:"gpu_count"`
}

type nodeCompat struct {
	ID          string
	Hostname    string
	IPAddress   string
	Status      string
	TotalVRAMMB int64
	TotalCPU    int
	TotalRAMMB  int64
	Compatible  bool
	Reason      string
}

type modelRequirements struct {
	ExecutionType    string
	RequiredVRAMMB   int64
	RequiredCPU      int
	RequiredMemoryMB int64
	RequiresGPU      bool
	RequiresVLLM     bool
	RequiresOllama   bool
	RequiresTTS      bool
	RequiresWhisper  bool
}

func (h *RequirementsHandler) loadCapabilities(ctx context.Context, nodeID string) capRow {
	var cap capRow
	_ = h.db.GetContext(ctx, &cap, `
		SELECT has_gpu, has_vllm, has_ollama, has_tts, has_whisper, gpu_count
		FROM node_capabilities WHERE node_id = $1`, nodeID)
	return cap
}

func checkCompatibility(req modelRequirements, node nodeCompat, cap capRow) (bool, string) {
	if req.ExecutionType == "GPU" || req.RequiresGPU {
		if !cap.HasGPU {
			return false, "no GPU available on node"
		}
		if req.RequiredVRAMMB > 0 && node.TotalVRAMMB < req.RequiredVRAMMB {
			return false, "insufficient VRAM"
		}
	}
	if req.ExecutionType == "CPU" {
		if req.RequiredCPU > 0 && node.TotalCPU < req.RequiredCPU {
			return false, "insufficient CPU cores"
		}
		if req.RequiredMemoryMB > 0 && node.TotalRAMMB < req.RequiredMemoryMB {
			return false, "insufficient RAM"
		}
	}
	if req.RequiresVLLM && !cap.HasVLLM {
		return false, "vllm not available on node"
	}
	if req.RequiresOllama && !cap.HasOllama {
		return false, "ollama not available on node"
	}
	if req.RequiresTTS && !cap.HasTTS {
		return false, "tts not available on node"
	}
	if req.RequiresWhisper && !cap.HasWhisper {
		return false, "whisper not available on node"
	}
	return true, "compatible"
}
