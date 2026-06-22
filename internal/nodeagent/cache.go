package nodeagent

// cache.go — Model cache scanning for the node agent.
// Scans local HuggingFace and Ollama caches and reports them to the control plane
// so operators know which models are already downloaded on each node.

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// CachedModel represents one model found in the local cache.
type CachedModel struct {
	ModelRef  string `json:"model_ref"`  // HF repo ID (e.g. "google/gemma-2b") or Ollama name
	Backend   string `json:"backend"`    // vllm | ollama
	SizeBytes int64  `json:"size_bytes"`
	IsCached  bool   `json:"is_cached"`
}

// ScanModelCache returns all models currently cached on this node from all backends.
func ScanModelCache() []CachedModel {
	var out []CachedModel
	out = append(out, scanHFCache()...)
	out = append(out, scanOllamaCache()...)
	return out
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
				Backend:   "vllm",
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
	// replace first -- with /
	idx := strings.Index(s, "--")
	if idx >= 0 {
		return s[:idx] + "/" + s[idx+2:]
	}
	return s
}

// ─── Ollama cache ─────────────────────────────────────────────────────────────

// scanOllamaCache lists models via `ollama list` output.
func scanOllamaCache() []CachedModel {
	out, err := exec.Command("ollama", "list").Output()
	if err != nil {
		return nil // ollama not installed
	}

	var models []CachedModel
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	first := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if first {
			first = false
			continue // skip header line "NAME  ID  SIZE  MODIFIED"
		}
		if line == "" {
			continue
		}
		// "gemma2:2b    8ccf136fdd52    1.6 GB    4 hours ago"
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		modelName := fields[0]
		var sizeBytes int64
		if len(fields) >= 4 {
			sizeBytes = parseSize(fields[2], fields[3])
		}
		models = append(models, CachedModel{
			ModelRef:  modelName,
			Backend:   "ollama",
			SizeBytes: sizeBytes,
			IsCached:  true,
		})
	}
	return models
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

// parseSize converts "1.6 GB" / "748 MB" to bytes.
func parseSize(valueStr, unit string) int64 {
	f, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return 0
	}
	switch strings.ToUpper(unit) {
	case "GB":
		return int64(f * 1024 * 1024 * 1024)
	case "MB":
		return int64(f * 1024 * 1024)
	case "KB":
		return int64(f * 1024)
	default:
		return int64(f)
	}
}
