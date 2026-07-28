package nodeagent

// cache.go — Model cache scanning for the node agent.
// Scans the local HuggingFace cache and reports models to the control plane
// so operators know which models are already downloaded on each node.

import (
	"os"
	"path/filepath"
	"strings"
)

// CachedModel represents one model found in the local cache.
type CachedModel struct {
	ModelRef  string `json:"model_ref"` // HF repo ID (e.g. "google/gemma-2b")
	Backend   string `json:"backend"`   // huggingface
	SizeBytes int64  `json:"size_bytes"`
	IsCached  bool   `json:"is_cached"`
}

// ScanModelCache returns all models currently cached on this node.
func ScanModelCache() []CachedModel {
	return scanHFCache()
}

// ─── HuggingFace Hub cache ────────────────────────────────────────────────────

// scanHFCache scans the HuggingFace Hub cache.
// Cache structure: <cache_dir>/models--{org}--{name}/snapshots/...
func scanHFCache() []CachedModel {
	cacheDirs := hfCacheDirs()
	seen := make(map[string]bool)
	var models []CachedModel

	for _, dir := range cacheDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), "models--") {
				continue
			}
			// "models--google--gemma-2b" → "google/gemma-2b"
			modelRef := hfDirToRef(e.Name())
			if seen[modelRef] {
				continue
			}
			seen[modelRef] = true

			size := dirSize(filepath.Join(dir, e.Name()))
			if size == 0 {
				continue // empty dir = not actually downloaded
			}
			models = append(models, CachedModel{
				ModelRef:  modelRef,
				Backend:   "huggingface", // source; actual backend is determined at deploy time
				SizeBytes: size,
				IsCached:  true,
			})
		}
	}
	return models
}

func hfCacheDirs() []string {
	dirs := []string{
		"/root/.cache/huggingface/hub",
		"/home/user/.cache/huggingface/hub",
	}
	if hfHome := os.Getenv("HF_HOME"); hfHome != "" {
		dirs = append([]string{filepath.Join(hfHome, "hub")}, dirs...)
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".cache", "huggingface", "hub"))
	}
	return dirs
}

// hfDirToRef converts "models--google--gemma-2b" → "google/gemma-2b"
func hfDirToRef(dirName string) string {
	s := strings.TrimPrefix(dirName, "models--")
	before, after, found := strings.Cut(s, "--")
	if found {
		return before + "/" + after
	}
	return s
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// dirSize returns total size of a directory tree in bytes.
func dirSize(path string) int64 {
	var total int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
