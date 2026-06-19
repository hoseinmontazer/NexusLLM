package gpu

import (
	"fmt"
	"sort"
)

// ─────────────────────────────────────────────────────────────────────────────
// Multi-Model GPU Packing
// Determines which GPU indices to assign to each model on a single server,
// maximising utilisation while respecting VRAM and tensor-parallel constraints.
// ─────────────────────────────────────────────────────────────────────────────

// ModelPlacementRequest describes one model that needs GPU assignment.
type ModelPlacementRequest struct {
	ModelName      string
	RequiredVRAMMB int  // VRAM needed per GPU
	GPUCount       int  // number of GPUs (= tensor-parallel size)
}

// PlacementResult maps model names to assigned GPU device indices.
type PlacementResult struct {
	Assignments map[string][]int // model name → []device_index
	Unscheduled []string         // models that could not be placed
}

// PackModels runs a bin-packing algorithm over the available GPU devices
// and returns an assignment plan. Devices are sorted by index so assignments
// are deterministic and contiguous where possible (better NVLink bandwidth).
//
// Algorithm: first-fit decreasing (FFD) — largest VRAM request first.
// A model occupies a contiguous block of GPUs within one node when possible.
func PackModels(devices []Device, requests []ModelPlacementRequest) PlacementResult {
	result := PlacementResult{
		Assignments: make(map[string][]int),
	}

	// Build a mutable list of available devices sorted by index
	avail := make([]Device, 0, len(devices))
	for _, d := range devices {
		if d.Status == "available" {
			avail = append(avail, d)
		}
	}
	sort.Slice(avail, func(i, j int) bool {
		return avail[i].DeviceIndex < avail[j].DeviceIndex
	})

	used := make([]bool, len(avail))

	// Sort requests largest first (FFD heuristic)
	sorted := make([]ModelPlacementRequest, len(requests))
	copy(sorted, requests)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].RequiredVRAMMB*sorted[i].GPUCount > sorted[j].RequiredVRAMMB*sorted[j].GPUCount
	})

	for _, req := range sorted {
		assigned := findContiguousBlock(avail, used, req)
		if assigned == nil {
			// Fall back to non-contiguous
			assigned = findNonContiguous(avail, used, req)
		}
		if assigned == nil {
			result.Unscheduled = append(result.Unscheduled, req.ModelName)
			continue
		}
		indices := make([]int, len(assigned))
		for i, idx := range assigned {
			indices[i] = avail[idx].DeviceIndex
			used[idx] = true
		}
		result.Assignments[req.ModelName] = indices
	}
	return result
}

// ExplainPacking returns a human-readable placement plan string.
func ExplainPacking(result PlacementResult) string {
	out := "GPU Packing Plan:\n"
	for model, gpus := range result.Assignments {
		out += fmt.Sprintf("  %-30s → GPU %v\n", model, gpus)
	}
	if len(result.Unscheduled) > 0 {
		out += fmt.Sprintf("  UNSCHEDULED: %v\n", result.Unscheduled)
	}
	return out
}

// ─── private helpers ──────────────────────────────────────────────────────────

// findContiguousBlock looks for GPUCount consecutive free devices with sufficient VRAM.
func findContiguousBlock(avail []Device, used []bool, req ModelPlacementRequest) []int {
	n := len(avail)
	for start := 0; start <= n-req.GPUCount; start++ {
		ok := true
		for i := start; i < start+req.GPUCount; i++ {
			if used[i] || avail[i].VRAM_MB < req.RequiredVRAMMB {
				ok = false
				break
			}
		}
		if ok {
			block := make([]int, req.GPUCount)
			for i := range block {
				block[i] = start + i
			}
			return block
		}
	}
	return nil
}

// findNonContiguous picks any GPUCount free devices with sufficient VRAM.
func findNonContiguous(avail []Device, used []bool, req ModelPlacementRequest) []int {
	var chosen []int
	for i, d := range avail {
		if !used[i] && d.VRAM_MB >= req.RequiredVRAMMB {
			chosen = append(chosen, i)
			if len(chosen) == req.GPUCount {
				return chosen
			}
		}
	}
	return nil
}
